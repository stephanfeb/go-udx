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

// TestStreamReassembly_ReleasesContiguousRun covers the buffer-and-flush path
// directly: bytes that arrive ahead of a gap wait, and are released in one go
// once the gap closes.
func TestStreamReassembly_ReleasesContiguousRun(t *testing.T) {
	s, _ := newTestStream(t)

	// "world" arrives before "hello", so nothing is readable yet.
	s.DeliverData(5, []byte("world"))
	s.mu.Lock()
	readable := len(s.recvBuf)
	s.mu.Unlock()
	if readable != 0 {
		t.Fatalf("%d bytes readable while the opening bytes are missing", readable)
	}

	s.DeliverData(0, []byte("hello"))

	buf := make([]byte, 32)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "helloworld" {
		t.Fatalf("read %q, want %q", buf[:n], "helloworld")
	}
}

// TestStreamReassembly_DeliversEachByteExactlyOnce is the guarantee the Noise
// layer above depends on: its nonce counter is sequential, so a byte delivered
// twice is a MAC failure, not a duplicated read. Retransmissions arrive as
// exact repeats and as partial overlaps, and neither may reach the reader.
func TestStreamReassembly_DeliversEachByteExactlyOnce(t *testing.T) {
	s, _ := newTestStream(t)

	s.DeliverData(0, []byte("hello"))
	s.DeliverData(0, []byte("hello"))       // exact retransmission
	s.DeliverData(3, []byte("lo world"))    // overlaps the delivered tail
	s.DeliverData(0, []byte("hello world")) // wholly overlapping repeat

	buf := make([]byte, 64)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "hello world" {
		t.Fatalf("read %q, want %q", buf[:n], "hello world")
	}
}

// TestStreamReassembly_BufferIsCapped stops a peer that ignores its flow-control
// limit from growing the out-of-order buffer without end. Anything refused is
// still tracked by that peer and will be retransmitted, so overflow costs
// throughput rather than correctness.
func TestStreamReassembly_BufferIsCapped(t *testing.T) {
	s, _ := newTestStream(t)

	// Everything lands ahead of a gap at offset 0, so none of it can be
	// released and it all has to be held.
	chunk := make([]byte, 64<<10)
	offset := uint64(1)
	for s.oooBytes+len(chunk) <= maxStreamRecvOOO {
		s.DeliverData(offset, chunk)
		offset += uint64(len(chunk))
	}
	before := s.oooBytes
	s.DeliverData(offset, chunk)
	if s.oooBytes != before {
		t.Fatalf("buffer grew past the %d-byte backstop: %d bytes held", maxStreamRecvOOO, s.oooBytes)
	}

	// Overrunning the backstop is the peer's fault and must be loud. Silently
	// dropping the bytes strands the stream: the packet was acknowledged on
	// arrival, so the sender will never send them again.
	if s.State() != StreamStateReset {
		t.Fatal("overrunning the out-of-order backstop did not fail the stream; " +
			"the discarded bytes are never re-sent, so it would hang forever")
	}
}

// TestStreamReassembly_FinDoesNotTruncateTheTail pins the reason a FIN carries
// the stream's final size rather than acting as EOF on arrival.
//
// A FIN is a flag on a frame that can overtake data still in flight. Closing
// the stream when it lands would drop whatever had not caught up, silently
// truncating the transfer at whatever offset happened to have arrived.
func TestStreamReassembly_FinDoesNotTruncateTheTail(t *testing.T) {
	s, _ := newTestStream(t)

	// The FIN arrives first, announcing 11 bytes; only the tail has landed.
	s.DeliverData(5, []byte(" world"))
	s.DeliverFin(11)

	if s.finReceived {
		t.Fatal("stream reported complete while the opening bytes were still missing")
	}
	buf := make([]byte, 64)
	s.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := s.Read(buf); err != ErrDeadlineExceeded {
		t.Fatalf("read returned %v; a premature EOF would have truncated the stream", err)
	}

	s.DeliverData(0, []byte("hello"))

	s.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "hello world" {
		t.Fatalf("read %q, want %q", buf[:n], "hello world")
	}
	if !s.finReceived {
		t.Fatal("stream did not complete once every byte up to the final size arrived")
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

// TestMultiStream_OneStreamStallDoesNotBlockAnother is the property the byte
// offsets were added for.
//
// Delivery order used to come from the packet sequence number, which is
// allocated per connection. A gap in it stalled the connection's reassembly, so
// every stream waited on the missing packet regardless of which stream it
// belonged to — one slow or lossy stream held up all its siblings. With STREAM
// frames carrying their own offsets, a stream is delayed only by its own bytes.
//
// The path here blocks one stream completely for a fixed period. The other must
// finish well inside it.
func TestMultiStream_OneStreamStallDoesNotBlockAnother(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a deliberate stall; skipped under -short")
	}

	const stalled = 8 << 10   // small, so the blocked stream does not eat the window
	const flowing = 256 << 10 // large enough that finishing early is meaningful
	const blockFor = 3 * time.Second

	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	// Stream IDs are odd for the initiator, so the client's first stream is 1.
	path := &streamBlockingPath{PacketConn: clientUDP, blockStream: 1, until: time.Now().Add(blockFor)}

	server := NewMultiplexer(serverUDP, RealClock{})
	client := NewMultiplexer(path, RealClock{})
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	srvConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}

	blocked, err := conn.OpenStream(ctx) // stream 1, the one the path holds
	if err != nil {
		t.Fatal(err)
	}
	open, err := conn.OpenStream(ctx) // stream 3, unimpeded
	if err != nil {
		t.Fatal(err)
	}

	// Each stream announces itself by size, since the server accepts them in
	// whatever order their first packet lands.
	done := make(chan int, 2)
	go func() {
		for i := 0; i < 2; i++ {
			s, err := srvConn.AcceptStream(ctx)
			if err != nil {
				return
			}
			go func(s *Stream) {
				got, err := drain(s, flowing, 40*time.Second)
				if err != nil && len(got) != stalled {
					done <- -1
					return
				}
				done <- len(got)
			}(s)
		}
	}()

	started := time.Now()
	go func() {
		blocked.SetWriteDeadline(time.Now().Add(40 * time.Second))
		blocked.Write(pattern(stalled))
	}()
	go func() {
		open.SetWriteDeadline(time.Now().Add(40 * time.Second))
		open.Write(pattern(flowing))
	}()

	// The first stream to finish must be the unimpeded one, and it must finish
	// before the block is lifted. Waiting for the block to expire would mean the
	// two streams are still coupled.
	select {
	case n := <-done:
		elapsed := time.Since(started)
		if n != flowing {
			t.Fatalf("first stream to complete carried %d bytes, want the unblocked stream's %d", n, flowing)
		}
		if elapsed >= blockFor {
			t.Fatalf("the unblocked stream took %v to finish, longer than the %v stall on its "+
				"sibling — the streams are still coupled", elapsed.Round(time.Millisecond), blockFor)
		}
		t.Logf("unblocked stream completed in %v while its sibling was stalled for %v",
			elapsed.Round(time.Millisecond), blockFor)
	case <-time.After(40 * time.Second):
		t.Fatal("neither stream completed")
	}
}

// streamBlockingPath discards every data packet belonging to one stream until a
// deadline, leaving all other traffic untouched. Retransmissions are discarded
// too, so the stream is genuinely stalled rather than merely delayed.
type streamBlockingPath struct {
	net.PacketConn
	blockStream uint32
	until       time.Time
}

func (c *streamBlockingPath) WriteTo(b []byte, addr net.Addr) (int, error) {
	if time.Now().Before(c.until) {
		if pkt, err := UnmarshalPacket(b); err == nil &&
			pkt.SourceStreamID == c.blockStream && isDataBearing(pkt) {
			return len(b), nil
		}
	}
	return c.PacketConn.WriteTo(b, addr)
}

// TestHandleStreamFrame_DataBeforeSynOpensTheStream pins the second thing that
// broke when packets stopped being reordered before delivery.
//
// A stream used to be opened only by a frame carrying SYN, which was safe only
// because connection-level reassembly guaranteed the SYN — the lowest sequence
// number — was processed first. Handling frames on arrival lets a reordered
// data packet beat it, and there was nothing to attach the bytes to: they were
// dropped, having already been acknowledged, so the sender never re-sent them
// and the stream stalled at that offset forever. It cost roughly one transfer
// in four at 25% reordering, always after exactly one packet.
func TestHandleStreamFrame_DataBeforeSynOpensTheStream(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	local, _ := NewConnectionID([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	remote, _ := NewConnectionID([]byte{8, 7, 6, 5, 4, 3, 2, 1})
	c := NewConnection(local, remote, addr, addr, false, RealClock{},
		func([]byte, net.Addr) error { return nil })
	c.addrValidated = true
	t.Cleanup(func() { c.Close() })

	send := func(offset uint64, payload string, syn bool) {
		c.HandlePacket(&Packet{
			Version:             VersionCurrent,
			Sequence:            uint32(offset) + 1,
			DestinationStreamID: 0, // peer has not learned our id yet
			SourceStreamID:      7,
			Frames:              []Frame{&StreamFrame{IsSyn: syn, Offset: offset, Data: []byte(payload)}},
		})
	}

	// The second packet overtakes the SYN.
	send(5, "world", false)
	send(0, "hello", true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := c.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("no stream was opened by data that arrived before the SYN: %v", err)
	}

	got, err := drain(s, len("helloworld"), 2*time.Second)
	if err != nil {
		t.Fatalf("stalled: the bytes that arrived before the SYN were dropped "+
			"after being acknowledged, so they are never re-sent: %v", err)
	}
	if string(got) != "helloworld" {
		t.Fatalf("read %q, want %q", got, "helloworld")
	}
}

// TestHandleStreamFrame_BareFinDoesNotOpenAStream is the limit on the above. A
// FIN carries nothing to deliver, so opening a stream for one invents a stream
// the application never had — which is what a FIN for an already-closed stream
// would otherwise do.
func TestHandleStreamFrame_BareFinDoesNotOpenAStream(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	local, _ := NewConnectionID([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	remote, _ := NewConnectionID([]byte{8, 7, 6, 5, 4, 3, 2, 1})
	c := NewConnection(local, remote, addr, addr, false, RealClock{},
		func([]byte, net.Addr) error { return nil })
	c.addrValidated = true
	t.Cleanup(func() { c.Close() })

	c.HandlePacket(&Packet{
		Version:             VersionCurrent,
		Sequence:            1,
		DestinationStreamID: 0,
		SourceStreamID:      7,
		Frames:              []Frame{&StreamFrame{IsFin: true, Offset: 0}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if s, err := c.AcceptStream(ctx); err == nil {
		t.Fatalf("a bare FIN opened stream %d", s.ID)
	}
}
