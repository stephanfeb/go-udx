package udx

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Loss detection under reordering.
//
// A gap in the acknowledged sequence numbers is suspicion, not proof. UDP
// reorders and a late packet still arrives, so declaring every gap lost
// re-sends packets that were already on their way. That is not a small effect:
// the netem baseline measured reordering costing an order of magnitude more
// retransmit overhead than actual loss.
//
// netem is far too noisy to measure this — the same condition on unchanged code
// ranged from 27.9% to 40.5% across runs — so these tests use a seeded path and
// count retransmissions directly, which makes the effect exact and repeatable.

// countingPath models a link with propagation delay on which some datagrams
// are displaced, and counts how many distinct data packets are sent more than
// once.
//
// The base delay matters as much as the jitter. Reordering on a real path is
// bounded by that path's delay — a packet takes a different route or queue and
// arrives a fraction of an RTT late — and RFC 9002's time threshold is defined
// as a multiple of the RTT for exactly that reason. Displacing packets by a
// fixed interval on a loopback socket instead produces reordering hundreds of
// times the RTT, which no threshold proportional to RTT can absorb, and
// measures nothing about real behaviour.
//
// Retransmissions are counted by sequence number on the wire rather than by
// asking the packet manager, so the measurement is of what the path actually
// carried.
type countingPath struct {
	net.PacketConn

	baseDelay  time.Duration // applied to every datagram: the propagation delay
	jitter     time.Duration // extra delay on the displaced fraction
	jitterRate float64

	mu        sync.Mutex
	rng       *rand.Rand
	sentSeqs  map[uint32]int
	reordered int64

	normal    chan delivery
	jittered  chan delivery
	closed    chan struct{}
	closeOnce sync.Once
}

func newCountingPath(pc net.PacketConn, baseDelay, jitter time.Duration, jitterRate float64, seed int64) *countingPath {
	c := &countingPath{
		PacketConn: pc,
		baseDelay:  baseDelay,
		jitter:     jitter,
		jitterRate: jitterRate,
		rng:        rand.New(rand.NewSource(seed)),
		sentSeqs:   make(map[uint32]int),
		normal:     make(chan delivery, 4096),
		jittered:   make(chan delivery, 4096),
		closed:     make(chan struct{}),
	}
	go c.runLane(c.normal, baseDelay)
	go c.runLane(c.jittered, baseDelay+jitter)
	return c
}

func (c *countingPath) WriteTo(b []byte, addr net.Addr) (int, error) {
	pkt, err := UnmarshalPacket(b)
	if err == nil && isDataBearing(pkt) {
		c.mu.Lock()
		c.sentSeqs[pkt.Sequence]++
		c.mu.Unlock()
	}

	c.mu.Lock()
	displaced := c.rng.Float64() < c.jitterRate
	c.mu.Unlock()

	buf := make([]byte, len(b))
	copy(buf, b)

	// Two strictly ordered lanes rather than a goroutine per datagram. A
	// goroutine each looks equivalent and is not: they wake in whatever order
	// the scheduler chooses, so the "undisturbed" traffic arrives lightly
	// shuffled and the harness measures its own reordering. Within a lane order
	// is exactly preserved, so the only reordering is the intended one —
	// datagrams that changed lanes.
	lane := c.normal
	if displaced {
		atomic.AddInt64(&c.reordered, 1)
		lane = c.jittered
	}
	wait := c.baseDelay
	if displaced {
		wait += c.jitter
	}
	select {
	case lane <- delivery{buf: buf, addr: addr, at: time.Now().Add(wait)}:
	case <-c.closed:
	}
	return len(b), nil
}

type delivery struct {
	buf  []byte
	addr net.Addr
	at   time.Time // when this datagram is due, measured from when it was sent
}

// runLane delivers a lane's datagrams in order, each at its own due time.
//
// The deadline has to be per datagram. Sleeping the delay once per delivery
// instead serialises the lane — every datagram waits behind the previous one's
// sleep, so a 10ms link delivers 100 packets per second and the sender spends
// the whole transfer timing out.
func (c *countingPath) runLane(lane <-chan delivery, _ time.Duration) {
	for {
		select {
		case d := <-lane:
			if wait := time.Until(d.at); wait > 0 {
				time.Sleep(wait)
			}
			c.PacketConn.WriteTo(d.buf, d.addr)
		case <-c.closed:
			return
		}
	}
}

func (c *countingPath) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.PacketConn.Close()
}

// stats returns how many distinct data packets were sent, and how many extra
// transmissions beyond the first those packets cost.
func (c *countingPath) stats() (distinct, retransmits int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.sentSeqs {
		distinct++
		retransmits += n - 1
	}
	return distinct, retransmits
}

// TestLossDetection_CleanPathNeverRetransmits is the strict invariant: on a
// link that delivers every datagram in order, a correct sender re-sends nothing
// at all. Any retransmission here is a packet sent again while the original was
// still perfectly in flight.
//
// This is the test that earns its keep. It caught two separate faults in its own
// harness — a goroutine per datagram, which let the scheduler shuffle delivery
// and made the "undisturbed" case look 36% lossy, and a per-lane sleep that
// serialised the link into 100 packets a second. Both produced plausible,
// entirely fictional numbers.
func TestLossDetection_CleanPathNeverRetransmits(t *testing.T) {
	if testing.Short() {
		t.Skip("moves a megabyte; skipped under -short")
	}

	distinct, retransmits, reordered := measureRetransmissions(t, 0, 0)
	if reordered != 0 {
		t.Fatalf("%d datagrams were displaced on a path configured not to displace any", reordered)
	}
	if retransmits != 0 {
		t.Fatalf("re-sent %d of %d packets on a path that dropped and reordered nothing",
			retransmits, distinct)
	}
	t.Logf("%d packets, 0 retransmissions", distinct)
}

// TestLossDetection_ReorderingCostIsBounded records what reordering actually
// costs, and holds it to a ceiling.
//
// The RFC 9002 thresholds do not rescue this, and it is worth being precise
// about why rather than assuming they should. kPacketThreshold tolerates three
// packets of displacement; on a fast link even a sub-millisecond delay displaces
// far more than three, so the threshold is exceeded and the packet is declared
// lost regardless. Measured against this harness the thresholds moved the cost
// by less than a point at every displacement tried, from 200us to 5ms. They are
// implemented because declaring every gap lost on sight is wrong in principle,
// not because they were observed to pay.
func TestLossDetection_ReorderingCostIsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("moves several megabytes; skipped under -short")
	}

	for _, tc := range []struct {
		name    string
		rate    float64
		ceiling float64
	}{
		{"2pct", 0.02, 5},
		{"5pct", 0.05, 10},
		{"20pct", 0.20, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			distinct, retransmits, reordered := measureRetransmissions(t, tc.rate, 5*time.Millisecond)
			if reordered == 0 {
				t.Fatal("no datagrams were displaced; the test proves nothing")
			}
			pct := float64(retransmits) / float64(distinct) * 100
			t.Logf("%.0f%% reordered: %d of %d packets re-sent (%.1f%%)",
				tc.rate*100, retransmits, distinct, pct)
			if pct > tc.ceiling {
				t.Fatalf("reordering %.0f%% of datagrams cost %.1f%% retransmission, "+
					"above the %.0f%% ceiling", tc.rate*100, pct, tc.ceiling)
			}
		})
	}
}

// measureRetransmissions runs a fixed transfer over a link with the given
// propagation delay and displacement, and reports what the sender put on the
// wire.
func measureRetransmissions(t *testing.T, jitterRate float64, jitter time.Duration) (distinct, retransmits int, reordered int64) {
	t.Helper()

	const total = 1 << 20
	const baseDelay = 10 * time.Millisecond // a 20ms round trip

	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	clientPath := newCountingPath(clientUDP, baseDelay, jitter, jitterRate, 4242)
	serverPath := newCountingPath(serverUDP, baseDelay, jitter, jitterRate, 99)

	server := NewMultiplexer(serverPath, RealClock{})
	client := NewMultiplexer(clientPath, RealClock{})
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	srvConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	read := make(chan error, 1)
	go func() {
		srvStream, err := srvConn.AcceptStream(ctx)
		if err != nil {
			read <- err
			return
		}
		got, err := drain(srvStream, total, 90*time.Second)
		if err != nil {
			read <- err
			return
		}
		if len(got) != total {
			read <- fmt.Errorf("read %d of %d bytes", len(got), total)
			return
		}
		read <- nil
	}()

	s.SetWriteDeadline(time.Now().Add(90 * time.Second))
	if n, err := s.Write(pattern(total)); err != nil {
		t.Fatalf("write stalled after %d of %d bytes: %v", n, total, err)
	}
	if err := <-read; err != nil {
		t.Fatal(err)
	}

	distinct, retransmits = clientPath.stats()
	if distinct == 0 {
		t.Fatal("no data packets were observed; the measurement is broken")
	}
	return distinct, retransmits, atomic.LoadInt64(&clientPath.reordered)
}

// TestDetectLostPackets_HonoursTheReorderThreshold drives the detector directly,
// so the boundary is exact rather than inferred from a transfer.
func TestDetectLostPackets_HonoursTheReorderThreshold(t *testing.T) {
	pm := newPacketManagerWithRTT(50 * time.Millisecond)
	now := pm.clock.Now()

	// Ten packets outstanding, all sent just now so the time threshold cannot
	// be what decides anything.
	for seq := uint32(0); seq < 10; seq++ {
		pm.sentPackets[seq] = &SentPacket{Sequence: seq, SentTime: now, Size: 1200}
	}

	// Acknowledge 9 and 8, leaving 7 and below in a gap. Only packets at least
	// LossReorderThreshold behind 9 may be declared lost, so 7 (one behind)
	// must survive while 6 and below do not.
	lost := pm.DetectLostPackets(&AckFrame{
		LargestAcked:        9,
		FirstAckRangeLength: 2, // 9 and 8
		AckRanges:           []AckRange{{Gap: 4, AckRangeLength: 1}},
	})

	for _, seq := range lost {
		if behind := 9 - int(seq); behind < LossReorderThreshold {
			t.Errorf("sequence %d is only %d behind the largest acked and was "+
				"declared lost; reordering still explains it", seq, behind)
		}
	}
	if len(lost) == 0 {
		t.Fatal("nothing was declared lost; the threshold cannot be this permissive")
	}
	t.Logf("declared lost: %v", lost)
}

// TestDetectLostPackets_TimeThresholdCatchesOldPackets is the other half: a
// packet too close behind the largest acked to trip the packet threshold must
// still be recovered once it is plainly too old to be in flight. Without this,
// a gap near the end of a transfer would wait for the RTO.
func TestDetectLostPackets_TimeThresholdCatchesOldPackets(t *testing.T) {
	pm := newPacketManagerWithRTT(50 * time.Millisecond)
	now := pm.clock.Now()

	// Sequence 2 is only one behind the largest acked, so the packet threshold
	// alone would spare it — but it was sent far longer ago than lossDelay.
	pm.sentPackets[2] = &SentPacket{
		Sequence: 2,
		SentTime: now.Add(-10 * pm.lossDelay()),
		Size:     1200,
	}

	lost := pm.DetectLostPackets(&AckFrame{
		LargestAcked:        3,
		FirstAckRangeLength: 1,
		AckRanges:           []AckRange{{Gap: 2, AckRangeLength: 1}},
	})

	var found bool
	for _, seq := range lost {
		if seq == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("a packet %v older than the loss delay was not recovered; "+
			"only the RTO would eventually catch it", 10*pm.lossDelay())
	}
}

// TestLossDelay_UsesTheLargerRTT guards the choice of input. After a sudden
// increase in delay the smoothed estimate lags, and a loss delay derived from it
// alone would declare a window of packets lost for merely being slower than
// before — exactly when the path can least afford duplicates.
func TestLossDelay_UsesTheLargerRTT(t *testing.T) {
	pm := newPacketManagerWithRTT(20 * time.Millisecond)

	smoothed := pm.cc.SmoothedRtt()
	// One much slower sample: latest jumps, smoothed barely moves.
	pm.cc.OnPacketAcked(1200, pm.clock.Now().Add(-400*time.Millisecond), 0, true, 2)

	latest := pm.cc.LatestRtt()
	if latest <= smoothed {
		t.Fatalf("latest RTT %v did not exceed smoothed %v; the test proves nothing", latest, smoothed)
	}

	want := latest * LossTimeThresholdNumerator / LossTimeThresholdDenominator
	if got := pm.lossDelay(); got != want {
		t.Fatalf("loss delay is %v, want %v (9/8 of the larger RTT %v)", got, want, latest)
	}
}

// TestLossDelay_HasAFloor keeps a near-zero RTT estimate — loopback, or before
// any sample — from collapsing the time threshold to nothing and undoing the
// packet threshold.
func TestLossDelay_HasAFloor(t *testing.T) {
	pm := NewPacketManager(RealClock{}, NewCongestionController(RealClock{}, func() int { return -1 }))
	pm.cc.mu.Lock()
	pm.cc.smoothedRtt = 0
	pm.cc.latestRtt = 0
	pm.cc.mu.Unlock()

	if got := pm.lossDelay(); got < LossTimerGranularity {
		t.Fatalf("loss delay collapsed to %v with a zero RTT; want at least %v",
			got, LossTimerGranularity)
	}
}
