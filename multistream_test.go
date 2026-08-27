package udx

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage for concurrent streams on one connection.
//
// A connection allocates one sequence number per packet, shared by every
// stream on it. Reassembly therefore has to happen at the connection: it is
// the only layer that sees a dense run of those numbers. Stream.DeliverData
// used to reorder on them directly, which is indistinguishable from correct
// while a connection carries a single stream and fails completely the moment
// it carries two — each stream sees a sparse subsequence and waits forever for
// gaps that belong to its sibling.
//
// dart-udx has always reassembled at the connection (socket.dart,
// _nextExpectedSeq + _connectionReceiveBuffer), so these tests pin Go to the
// protocol as already deployed.

// TestMultiStream_InterleavedTransfersAllComplete is the minimal regression:
// two streams, each large enough to need many packets. Before the fix each
// stream delivered exactly one packet (1372 bytes) and then stalled forever.
func TestMultiStream_InterleavedTransfersAllComplete(t *testing.T) {
	const streams = 4
	const perStream = 64 * 1024

	conn, srvConn, ctx := testPair(t, 30*time.Second)
	payload := pattern(perStream)

	errs := make(chan error, streams*2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		var inner sync.WaitGroup
		for i := 0; i < streams; i++ {
			s, err := srvConn.AcceptStream(ctx)
			if err != nil {
				errs <- fmt.Errorf("accept %d: %w", i, err)
				return
			}
			inner.Add(1)
			go func(s *Stream, idx int) {
				defer inner.Done()
				got, err := drain(s, perStream, 20*time.Second)
				if err != nil {
					errs <- fmt.Errorf("stream %d: %w", idx, err)
					return
				}
				if !bytes.Equal(got, payload) {
					errs <- fmt.Errorf("stream %d: payload corrupted", idx)
				}
			}(s, i)
		}
		inner.Wait()
	}()

	for i := 0; i < streams; i++ {
		s, err := conn.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(s *Stream, idx int) {
			defer wg.Done()
			s.SetWriteDeadline(time.Now().Add(20 * time.Second))
			if _, err := s.Write(payload); err != nil {
				errs <- fmt.Errorf("stream %d write: %w", idx, err)
			}
		}(s, i)
	}

	waitOrFail(t, &wg, 25*time.Second, "interleaved multi-stream transfer hung")
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestMultiStream_StreamsCarryDistinctPayloads guards against the failure mode
// that byte-count assertions cannot see: reassembly that delivers the right
// number of bytes to the wrong stream. Each stream carries a payload keyed to
// its index, so a misrouted packet corrupts a comparison rather than a total.
func TestMultiStream_StreamsCarryDistinctPayloads(t *testing.T) {
	const streams = 6
	const perStream = 48 * 1024

	conn, srvConn, ctx := testPair(t, 30*time.Second)

	// Each stream's first byte identifies it, so cross-delivery is detectable
	// even if lengths happen to match.
	payloads := make([][]byte, streams)
	for i := range payloads {
		p := pattern(perStream)
		for j := 0; j < len(p); j += 512 {
			p[j] = byte(i)
		}
		payloads[i] = p
	}

	// The receiver does not know which stream is which until it reads, so it
	// identifies the payload by its marker byte and checks the whole thing.
	errs := make(chan error, streams*2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		var inner sync.WaitGroup
		var seenMu sync.Mutex
		seen := make(map[byte]bool)

		for i := 0; i < streams; i++ {
			s, err := srvConn.AcceptStream(ctx)
			if err != nil {
				errs <- fmt.Errorf("accept %d: %w", i, err)
				return
			}
			inner.Add(1)
			go func(s *Stream) {
				defer inner.Done()
				got, err := drain(s, perStream, 20*time.Second)
				if err != nil {
					errs <- err
					return
				}
				marker := got[0]
				if int(marker) >= streams {
					errs <- fmt.Errorf("unrecognised marker %d: streams were interleaved", marker)
					return
				}
				if !bytes.Equal(got, payloads[marker]) {
					errs <- fmt.Errorf("stream %d: payload does not match its own marker", marker)
					return
				}
				seenMu.Lock()
				defer seenMu.Unlock()
				if seen[marker] {
					errs <- fmt.Errorf("marker %d delivered to two streams", marker)
				}
				seen[marker] = true
			}(s)
		}
		inner.Wait()

		seenMu.Lock()
		defer seenMu.Unlock()
		if len(seen) != streams {
			errs <- fmt.Errorf("received %d distinct payloads, want %d", len(seen), streams)
		}
	}()

	for i := 0; i < streams; i++ {
		s, err := conn.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(s *Stream, idx int) {
			defer wg.Done()
			s.SetWriteDeadline(time.Now().Add(20 * time.Second))
			if _, err := s.Write(payloads[idx]); err != nil {
				errs <- fmt.Errorf("stream %d write: %w", idx, err)
			}
		}(s, i)
	}

	waitOrFail(t, &wg, 25*time.Second, "distinct-payload transfer hung")
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestMultiStream_SurvivesLossAndReordering exercises the part of the change
// that unit tests cannot reach: the connection buffers packets ahead of a gap
// and releases them in order. Loss forces gaps, reordering fills the buffer,
// and several streams share the sequence space at once.
func TestMultiStream_SurvivesLossAndReordering(t *testing.T) {
	if testing.Short() {
		t.Skip("lossy multi-stream transfer is slow; skipped under -short")
	}

	const streams = 3
	const perStream = 128 * 1024

	var dropped, reordered int64
	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	newPath := func(pc net.PacketConn, seed int64) net.PacketConn {
		return &disorderlyPacketConn{
			PacketConn: pc,
			dropRate:   0.02,
			delayRate:  0.10,
			delay:      12 * time.Millisecond,
			rng:        rand.New(rand.NewSource(seed)),
			dropped:    &dropped,
			reordered:  &reordered,
		}
	}

	server := NewMultiplexer(newPath(serverUDP, 11), RealClock{})
	client := NewMultiplexer(newPath(clientUDP, 977), RealClock{})
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	srvConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}

	payload := pattern(perStream)
	errs := make(chan error, streams*2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		var inner sync.WaitGroup
		for i := 0; i < streams; i++ {
			s, err := srvConn.AcceptStream(ctx)
			if err != nil {
				errs <- fmt.Errorf("accept %d: %w", i, err)
				return
			}
			inner.Add(1)
			go func(s *Stream, idx int) {
				defer inner.Done()
				got, err := drain(s, perStream, 60*time.Second)
				if err != nil {
					errs <- fmt.Errorf("stream %d: %w", idx, err)
					return
				}
				// Byte-for-byte: a reassembler that drops or transposes a
				// packet under loss produces the right length and wrong bytes.
				if !bytes.Equal(got, payload) {
					errs <- fmt.Errorf("stream %d: payload corrupted under loss", idx)
				}
			}(s, i)
		}
		inner.Wait()
	}()

	for i := 0; i < streams; i++ {
		s, err := conn.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(s *Stream, idx int) {
			defer wg.Done()
			s.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if _, err := s.Write(payload); err != nil {
				errs <- fmt.Errorf("stream %d write: %w", idx, err)
			}
		}(s, i)
	}

	waitOrFail(t, &wg, 75*time.Second, "multi-stream transfer stalled under loss and reordering")
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	if atomic.LoadInt64(&dropped) == 0 || atomic.LoadInt64(&reordered) == 0 {
		t.Fatalf("path was not adverse: %d dropped, %d reordered — the test proved nothing",
			atomic.LoadInt64(&dropped), atomic.LoadInt64(&reordered))
	}
	t.Logf("completed with %d datagrams dropped, %d delayed out of order",
		atomic.LoadInt64(&dropped), atomic.LoadInt64(&reordered))
}

// TestMultiStream_OneStreamClosingLeavesOthersRunning covers lifecycle overlap:
// reassembly is connection-wide, so a stream finishing mid-transfer must not
// disturb the sequence its siblings still depend on.
func TestMultiStream_OneStreamClosingLeavesOthersRunning(t *testing.T) {
	const perStream = 96 * 1024

	conn, srvConn, ctx := testPair(t, 30*time.Second)
	short := pattern(1024)
	long := pattern(perStream)

	accepted := make(chan *Stream, 2)
	go func() {
		defer close(accepted)
		for i := 0; i < 2; i++ {
			s, err := srvConn.AcceptStream(ctx)
			if err != nil {
				return
			}
			accepted <- s
		}
	}()

	// The short stream is opened and finished first; the long one must keep
	// making progress across its sibling's FIN.
	shortStream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	longStream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, 4)
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		shortStream.SetWriteDeadline(time.Now().Add(20 * time.Second))
		if _, err := shortStream.Write(short); err != nil {
			errs <- fmt.Errorf("short write: %w", err)
			return
		}
		if err := shortStream.Close(); err != nil {
			errs <- fmt.Errorf("short close: %w", err)
		}
	}()
	go func() {
		defer wg.Done()
		longStream.SetWriteDeadline(time.Now().Add(20 * time.Second))
		if _, err := longStream.Write(long); err != nil {
			errs <- fmt.Errorf("long write: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var inner sync.WaitGroup
		for s := range accepted {
			inner.Add(1)
			go func(s *Stream) {
				defer inner.Done()
				// Read whichever payload this stream carries; the long one is
				// the assertion that survives its sibling's close.
				got, err := drain(s, perStream, 20*time.Second)
				if err != nil && len(got) != len(short) {
					errs <- fmt.Errorf("read: %w", err)
					return
				}
				switch len(got) {
				case len(short):
					if !bytes.Equal(got, short) {
						errs <- fmt.Errorf("short stream corrupted")
					}
				case perStream:
					if !bytes.Equal(got, long) {
						errs <- fmt.Errorf("long stream corrupted")
					}
				default:
					errs <- fmt.Errorf("unexpected length %d", len(got))
				}
			}(s)
		}
		inner.Wait()
	}()

	waitOrFail(t, &wg, 25*time.Second, "transfer hung after a sibling stream closed")
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// newReassemblyConn builds the minimum Connection state reassemble touches.
func newReassemblyConn() *Connection {
	return &Connection{recvOOO: make(map[uint32]*Packet)}
}

func dataPacket(seq uint32) *Packet {
	return &Packet{Sequence: seq, Frames: []Frame{&StreamFrame{Data: []byte{byte(seq)}}}}
}

func seqsOf(pkts []*Packet) []uint32 {
	out := make([]uint32, len(pkts))
	for i, p := range pkts {
		out[i] = p.Sequence
	}
	return out
}

// TestReassemble_ReleasesContiguousRunInOrder covers the buffer-and-flush path
// directly, including the baseline being the peer's first sequence rather than
// zero — the handshake may consume sequences before any stream data.
func TestReassemble_ReleasesContiguousRunInOrder(t *testing.T) {
	c := newReassemblyConn()

	if got := seqsOf(c.reassemble(dataPacket(7))); len(got) != 1 || got[0] != 7 {
		t.Fatalf("first packet: released %v, want [7]", got)
	}
	// 9 and 10 arrive before 8 and must be held.
	for _, seq := range []uint32{9, 10} {
		if got := c.reassemble(dataPacket(seq)); got != nil {
			t.Fatalf("seq %d released %v while 8 was missing", seq, seqsOf(got))
		}
	}
	got := seqsOf(c.reassemble(dataPacket(8)))
	want := []uint32{8, 9, 10}
	if len(got) != len(want) {
		t.Fatalf("filling the gap released %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("filling the gap released %v, want %v", got, want)
		}
	}
	if len(c.recvOOO) != 0 {
		t.Fatalf("%d packets left buffered after the gap closed", len(c.recvOOO))
	}
}

// TestReassemble_DeliversEachSequenceExactlyOnce is the guarantee the Noise
// layer above depends on: its nonce counter is sequential, so a byte delivered
// twice is a MAC failure, not a duplicated read.
func TestReassemble_DeliversEachSequenceExactlyOnce(t *testing.T) {
	c := newReassemblyConn()

	c.reassemble(dataPacket(0))
	if got := c.reassemble(dataPacket(0)); got != nil {
		t.Fatalf("re-delivered an already-released sequence: %v", seqsOf(got))
	}

	// A duplicate arriving while still buffered must not displace the original
	// or be released twice when the gap closes.
	c.reassemble(dataPacket(2))
	c.reassemble(dataPacket(2))
	if got := seqsOf(c.reassemble(dataPacket(1))); len(got) != 2 {
		t.Fatalf("gap close released %v, want exactly [1 2]", got)
	}
}

// TestReassemble_BufferIsCapped keeps the out-of-order buffer bounded. Anything
// refused here is still tracked by the peer's packet manager and retransmitted,
// so overflow costs throughput rather than correctness — but only because
// HandlePacket will accept that retransmission, which is what the next test
// pins.
func TestReassemble_BufferIsCapped(t *testing.T) {
	c := newReassemblyConn()
	c.reassemble(dataPacket(0)) // baseline; next expected is 1

	for seq := uint32(2); len(c.recvOOO) < maxConnRecvOOO; seq++ {
		c.reassemble(dataPacket(seq))
	}
	overflow := uint32(maxConnRecvOOO + 100)
	c.reassemble(dataPacket(overflow))
	if _, buffered := c.recvOOO[overflow]; buffered {
		t.Fatalf("buffer grew past its %d-packet cap", maxConnRecvOOO)
	}
}

// TestHandlePacket_RetransmitOfAnUndeliveredPacketIsAccepted pins the reason
// the old (sequence, marshaledSize) dedupe table had to go.
//
// That table was consulted in HandlePacket, before reassembly could refuse
// anything, and recorded every packet on arrival. So a packet dropped for want
// of reassembly buffer was already marked seen, and its retransmission — by
// then the only surviving copy — was discarded as a duplicate. The stream
// waited on a sequence that would never be offered again. The table was also
// trimmed above 1000 entries, so it could not be relied on in the other
// direction either. Sequence order answers the question exactly.
//
// This drives HandlePacket rather than reassemble because the defect was in the
// gate ahead of it: a test on reassemble alone passes with the gate restored.
// It has to overflow the buffer for real, because that is the only way a packet
// gets recorded as seen and then discarded without being delivered.
func TestHandlePacket_RetransmitOfAnUndeliveredPacketIsAccepted(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	local, _ := NewConnectionID([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	remote, _ := NewConnectionID([]byte{8, 7, 6, 5, 4, 3, 2, 1})
	c := NewConnection(local, remote, addr, addr, false, RealClock{},
		func([]byte, net.Addr) error { return nil })
	c.addrValidated = true

	s := NewStream(4, 2, NewStreamFlowController(1<<20, 1<<20))
	s.conn = c
	s.state = StreamStateOpen
	c.streams[4] = s

	deliver := func(seq uint32) {
		c.HandlePacket(&Packet{
			Sequence:            seq,
			DestinationStreamID: 4,
			SourceStreamID:      2,
			Frames:              []Frame{&StreamFrame{Data: []byte{byte(seq)}}},
		})
	}

	// Sequence 0 establishes the baseline and is delivered; 1 is lost, so
	// everything above it must be held.
	deliver(0)
	const overflow = uint32(maxConnRecvOOO + 2)
	for seq := uint32(2); seq < overflow; seq++ {
		deliver(seq)
	}
	if len(c.recvOOO) != maxConnRecvOOO {
		t.Fatalf("buffer holds %d packets, expected it full at %d", len(c.recvOOO), maxConnRecvOOO)
	}

	// This one arrives with the buffer full and is discarded — but it was still
	// recorded on arrival by the old gate.
	deliver(overflow)

	// The gap closes and everything buffered drains, leaving the discarded
	// packet as the next one expected.
	deliver(1)
	if c.nextExpectSeq != overflow {
		t.Fatalf("next expected sequence is %d, want %d", c.nextExpectSeq, overflow)
	}

	// The peer retransmits it. This is the only surviving copy.
	deliver(overflow)

	want := int(overflow) + 1 // sequences 0..overflow, one byte each
	got := make([]byte, 0, want)
	buf := make([]byte, 8192)
	s.SetReadDeadline(time.Now().Add(2 * time.Second))
	for len(got) < want {
		n, err := s.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("stalled after %d of %d bytes — the retransmission was refused "+
				"and the stream can never advance: %v", len(got), want, err)
		}
	}
	if last, wantLast := got[len(got)-1], byte(overflow%256); last != wantLast {
		t.Fatalf("last byte is %d, want %d", last, wantLast)
	}
}

// waitOrFail fails the test if wg does not finish within timeout. A hung
// multi-stream transfer is the exact symptom under test, so every case needs a
// bound rather than the package timeout.
func waitOrFail(t *testing.T, wg *sync.WaitGroup, timeout time.Duration, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal(msg)
	}
}

// disorderlyPacketConn drops some datagrams outright and delays others, which
// delivers them behind datagrams sent later. Reordering is what fills the
// connection's out-of-order buffer, and it is not exercised by loss alone.
//
// WriteTo runs on several goroutines and rand.Rand is not safe for concurrent
// use, so the source is guarded.
type disorderlyPacketConn struct {
	net.PacketConn
	dropRate  float64
	delayRate float64
	delay     time.Duration
	mu        sync.Mutex
	rng       *rand.Rand
	dropped   *int64
	reordered *int64
}

func (c *disorderlyPacketConn) roll() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rng.Float64()
}

func (c *disorderlyPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	switch r := c.roll(); {
	case r < c.dropRate:
		atomic.AddInt64(c.dropped, 1)
		return len(b), nil // silently discard, as a lossy path would
	case r < c.dropRate+c.delayRate:
		atomic.AddInt64(c.reordered, 1)
		buf := make([]byte, len(b))
		copy(buf, b)
		go func() {
			time.Sleep(c.delay)
			c.PacketConn.WriteTo(buf, addr)
		}()
		return len(b), nil
	default:
		return c.PacketConn.WriteTo(b, addr)
	}
}
