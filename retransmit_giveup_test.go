package udx

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage for what happens when retransmission runs out of attempts.
//
// A reliable ordered stream cannot survive an abandoned packet: its bytes are a
// hole in the middle of the byte stream, so the peer's reader waits on a
// sequence that will never be offered again. The packet used to be dropped from
// the tracking table with the stream left in place, and since MaxIdleTimeout is
// declared but never enforced, nothing else would ever surface the failure —
// the application simply saw the connection go quiet. Downstream that appeared
// as 40s yamux keepalive timeouts rather than an actionable error.
//
// Two changes are pinned here: the backoff is capped so the attempts are spent
// on trying rather than waiting, and exhausting them resets the stream.

// newPacketManagerWithRTT builds a PacketManager whose RTO reflects the given
// smoothed RTT, by feeding the congestion controller a real sample.
func newPacketManagerWithRTT(rtt time.Duration) *PacketManager {
	clk := RealClock{}
	cc := NewCongestionController(clk, func() int { return -1 })
	if rtt > 0 {
		cc.OnPacketAcked(1200, time.Now().Add(-rtt), 0, true, 1)
	}
	return NewPacketManager(clk, cc)
}

// TestRetransmitBackoff_IsCappedAndNeverBelowRTO covers both halves of the cap.
//
// It grows exponentially, saturates rather than running away, and — the part
// that is easy to get wrong — the ceiling never drops below the RTO. Capping
// below the round trip would retransmit before the peer could possibly have
// answered, so a slow path would be answered with a flood of duplicates.
func TestRetransmitBackoff_IsCappedAndNeverBelowRTO(t *testing.T) {
	t.Run("fast path saturates at the cap", func(t *testing.T) {
		pm := newPacketManagerWithRTT(0) // no samples: RTO sits at the 200ms floor
		rto := pm.retransmitTimeout()

		var prev time.Duration
		for attempt := 1; attempt <= MaxRetransmitRetries; attempt++ {
			got := pm.retransmitBackoff(attempt)
			if got > MaxRetransmitBackoff {
				t.Fatalf("attempt %d: backoff %v exceeds the %v cap", attempt, got, MaxRetransmitBackoff)
			}
			if got < rto {
				t.Fatalf("attempt %d: backoff %v is below the RTO %v", attempt, got, rto)
			}
			if got < prev {
				t.Fatalf("attempt %d: backoff went backwards, %v after %v", attempt, got, prev)
			}
			prev = got
		}
		if prev != MaxRetransmitBackoff {
			t.Fatalf("backoff saturated at %v, want the %v cap", prev, MaxRetransmitBackoff)
		}
	})

	t.Run("slow path never retransmits faster than the round trip", func(t *testing.T) {
		// An RTT well above the cap: the RTO must win, not MaxRetransmitBackoff.
		pm := newPacketManagerWithRTT(3 * time.Second)
		rto := pm.retransmitTimeout()
		if rto <= MaxRetransmitBackoff {
			t.Fatalf("RTO %v did not exceed the cap %v; the test proves nothing", rto, MaxRetransmitBackoff)
		}

		for attempt := 1; attempt <= MaxRetransmitRetries; attempt++ {
			if got := pm.retransmitBackoff(attempt); got < rto {
				t.Fatalf("attempt %d: backoff %v is shorter than the RTO %v — "+
					"this retransmits before the peer could have answered", attempt, got, rto)
			}
		}
	})
}

// TestRetransmitBudget_OnAHealthyPathFitsInsideTheIdleTimeout pins the reason
// the numbers are what they are. On a slow path the budget stretches with the
// RTO by design, so this measures the floor-RTO case — the one that has to stay
// inside MaxIdleTimeout. The old schedule spent 10 attempts over ~111s, most of
// it waiting.
func TestRetransmitBudget_OnAHealthyPathFitsInsideTheIdleTimeout(t *testing.T) {
	pm := newPacketManagerWithRTT(0)

	total := pm.retransmitTimeout() // the first timer, before any attempt
	for attempt := 1; attempt <= MaxRetransmitRetries; attempt++ {
		total += pm.retransmitBackoff(attempt)
	}

	if total >= MaxIdleTimeout {
		t.Fatalf("retransmission budget is %v, which does not fit inside the %v idle timeout",
			total, MaxIdleTimeout)
	}
	if total < MaxIdleTimeout/2 {
		t.Fatalf("retransmission budget is only %v against a %v idle timeout; "+
			"giving up this early wastes recovery time that costs nothing", total, MaxIdleTimeout)
	}
	t.Logf("%d attempts over %v, inside the %v idle timeout", MaxRetransmitRetries, total, MaxIdleTimeout)
}

// TestPermanentLoss_ResetsTheStream is the behavioural change: abandoning a
// packet must fail the stream rather than leave it silently stalled. Driven
// through the callback the connection installs, so it covers the wiring too.
func TestPermanentLoss_ResetsTheStream(t *testing.T) {
	c, s, _ := connWithStream(t)

	// A reader blocked on a stream whose data was abandoned must be woken with
	// an error. Before this it waited out its deadline, or forever without one.
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := s.Read(buf)
		readErr <- err
	}()

	// Give the reader time to park on the condition variable.
	time.Sleep(50 * time.Millisecond)

	c.pm.OnPacketPermanentLoss(&SentPacket{
		Sequence:            7,
		Size:                1200,
		SourceStreamID:      s.ID,
		DestinationStreamID: s.RemoteID,
		Frames:              []Frame{&StreamFrame{Data: []byte("lost")}},
	})

	select {
	case err := <-readErr:
		if !errors.Is(err, ErrStreamReset) {
			t.Fatalf("blocked reader got %v, want ErrStreamReset", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked reader was never woken; abandoning a packet left the stream stalled")
	}

	if got := s.State(); got != StreamStateReset {
		t.Fatalf("stream state is %v, want StreamStateReset", got)
	}

	// Writes after the reset must fail rather than queue into a dead stream.
	if _, err := s.Write([]byte("more")); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("write after reset returned %v, want ErrStreamReset", err)
	}
}

// TestPermanentLoss_ResetIsIdempotent covers a burst: losing several packets on
// one stream is ordinary on a broken path and must not send a RESET_STREAM per
// packet, nor panic on an already-reset stream.
func TestPermanentLoss_ResetIsIdempotent(t *testing.T) {
	c, s, sink := connWithStream(t)

	for i := 0; i < 5; i++ {
		c.pm.OnPacketPermanentLoss(&SentPacket{
			Sequence:            uint32(i),
			Size:                1200,
			SourceStreamID:      s.ID,
			DestinationStreamID: s.RemoteID,
		})
	}

	if got := s.State(); got != StreamStateReset {
		t.Fatalf("stream state is %v, want StreamStateReset", got)
	}
	if resets := sink.count(); resets != 1 {
		t.Fatalf("5 lost packets on one stream sent %d RESET_STREAM frames, want 1", resets)
	}
}

// TestPermanentLoss_UnknownStreamIsIgnored guards the lookup: a packet for a
// stream that has already gone away must not panic or reset something else.
func TestPermanentLoss_UnknownStreamIsIgnored(t *testing.T) {
	c, s, _ := connWithStream(t)

	c.pm.OnPacketPermanentLoss(&SentPacket{
		Sequence:            1,
		Size:                1200,
		SourceStreamID:      s.ID + 1000,
		DestinationStreamID: s.RemoteID + 1000,
	})

	if got := s.State(); got == StreamStateReset {
		t.Fatal("a lost packet for an unknown stream reset an unrelated stream")
	}
}

// TestWriter_OnADeadPathFailsRatherThanHanging is the end-to-end statement of
// the whole change, over real sockets: when the path stops carrying anything,
// a blocked writer must come back with an error in bounded time.
//
// Before this it hung indefinitely. Retransmission gave up after ~111s and then
// merely dropped the packet, and MaxIdleTimeout is not enforced, so there was
// nothing left to end the wait — the write simply never returned.
func TestWriter_OnADeadPathFailsRatherThanHanging(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the retransmission budget; skipped under -short")
	}

	var dropping int32
	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	server := NewMultiplexer(serverUDP, RealClock{})
	client := NewMultiplexer(&blackholePacketConn{PacketConn: clientUDP, dropping: &dropping}, RealClock{})
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Establish the stream while the path still works, then cut it.
	s.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatalf("initial write over a working path: %v", err)
	}
	atomic.StoreInt32(&dropping, 1)

	// Enough to outrun the congestion window, so the writer really blocks.
	done := make(chan error, 1)
	go func() {
		s.SetWriteDeadline(time.Now().Add(110 * time.Second))
		_, err := s.Write(make([]byte, 1<<20))
		done <- err
	}()

	started := time.Now()
	select {
	case err := <-done:
		elapsed := time.Since(started)
		if !errors.Is(err, ErrStreamReset) {
			t.Fatalf("writer on a dead path returned %v after %v, want ErrStreamReset",
				err, elapsed.Round(time.Second))
		}
		if got := s.State(); got != StreamStateReset {
			t.Fatalf("stream state is %v, want StreamStateReset", got)
		}
		t.Logf("writer failed cleanly after %v", elapsed.Round(time.Second))
	case <-time.After(100 * time.Second):
		t.Fatal("writer never returned; a dead path still hangs the caller")
	}
}

// blackholePacketConn silently discards everything once dropping is set, which
// is what a path that has gone away looks like from the sender: no delivery, no
// acknowledgment, and no error either.
type blackholePacketConn struct {
	net.PacketConn
	dropping *int32
}

func (c *blackholePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if atomic.LoadInt32(c.dropping) == 1 {
		return len(b), nil
	}
	return c.PacketConn.WriteTo(b, addr)
}

// connWithStream builds a connection whose datagrams are counted and discarded,
// with one open stream on it.
func connWithStream(t *testing.T) (*Connection, *Stream, *resetCountingSink) {
	t.Helper()

	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	local, err := NewConnectionID([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewConnectionID([]byte{8, 7, 6, 5, 4, 3, 2, 1})
	if err != nil {
		t.Fatal(err)
	}

	sink := &resetCountingSink{}
	c := NewConnection(local, remote, addr, addr, false, RealClock{}, sink.send)
	c.addrValidated = true
	t.Cleanup(func() { c.Close() })

	s := NewStream(4, 2, NewStreamFlowController(1<<20, 1<<20))
	s.conn = c
	s.state = StreamStateOpen
	c.streams[4] = s
	return c, s, sink
}

// resetCountingSink discards datagrams while counting the RESET_STREAM frames
// among them, so a test can assert how many actually went out.
type resetCountingSink struct {
	mu     sync.Mutex
	resets int
}

func (r *resetCountingSink) send(data []byte, _ net.Addr) error {
	pkt, err := UnmarshalPacket(data)
	if err != nil {
		return nil
	}
	for _, f := range pkt.Frames {
		if _, ok := f.(*ResetStreamFrame); ok {
			r.mu.Lock()
			r.resets++
			r.mu.Unlock()
		}
	}
	return nil
}

func (r *resetCountingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resets
}
