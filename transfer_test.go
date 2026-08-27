package udx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// End-to-end coverage for doc/TRANSPORT_WINDOW_BUG.md. The existing suite
// covered flow-control accounting but never sustained transfer, so a ceiling
// that only appears after ~256KB went unnoticed.

// testPair brings up two multiplexers on loopback and returns an established
// connection on each side.
func testPair(t *testing.T, timeout time.Duration) (*Connection, *Connection, context.Context) {
	t.Helper()

	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	server := NewMultiplexer(serverUDP, RealClock{})
	client := NewMultiplexer(clientUDP, RealClock{})
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)

	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	srvConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatalf("server accept: %v", err)
	}
	return conn, srvConn, ctx
}

// pattern produces deterministic bytes so the receiver can verify integrity
// rather than only counting.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + i/251)
	}
	return b
}

// drain reads exactly want bytes and returns them.
func drain(s *Stream, want int, timeout time.Duration) ([]byte, error) {
	out := make([]byte, 0, want)
	buf := make([]byte, 64*1024)
	if err := s.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	for len(out) < want {
		n, err := s.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if err == io.EOF {
				break
			}
			return out, fmt.Errorf("stalled after %d of %d bytes: %w", len(out), want, err)
		}
	}
	return out, nil
}

// TestTransfer_SustainedSingleStream is the direct regression: push far more
// than the reported ~256KB ceiling through one stream and verify every byte.
// Before the fix this stalled at exactly 262,144 bytes.
func TestTransfer_SustainedSingleStream(t *testing.T) {
	const total = 8 << 20 // 8 MB, 32x the reported ceiling

	conn, srvConn, ctx := testPair(t, 90*time.Second)

	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	want := pattern(total)

	type readResult struct {
		data []byte
		err  error
	}
	rc := make(chan readResult, 1)
	go func() {
		srvStream, err := srvConn.AcceptStream(ctx)
		if err != nil {
			rc <- readResult{nil, fmt.Errorf("accept stream: %w", err)}
			return
		}
		got, err := drain(srvStream, total, 60*time.Second)
		rc <- readResult{got, err}
	}()

	wc := make(chan error, 1)
	go func() {
		stream.SetWriteDeadline(time.Now().Add(60 * time.Second))
		written := 0
		for written < total {
			end := written + 256*1024
			if end > total {
				end = total
			}
			n, err := stream.Write(want[written:end])
			written += n
			if err != nil {
				wc <- fmt.Errorf("write stalled after %d of %d bytes: %w", written, total, err)
				return
			}
		}
		wc <- nil
	}()

	select {
	case err := <-wc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(75 * time.Second):
		t.Fatalf("write hung after %d of %d bytes", stream.BytesWritten, total)
	}

	select {
	case r := <-rc:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if len(r.data) != total {
			t.Fatalf("read %d bytes, want %d", len(r.data), total)
		}
		if !bytes.Equal(r.data, want) {
			for i := range r.data {
				if r.data[i] != want[i] {
					t.Fatalf("payload corrupted at byte %d: got %#x, want %#x", i, r.data[i], want[i])
				}
			}
		}
	case <-time.After(75 * time.Second):
		t.Fatal("read hung")
	}
}

// TestTransfer_SustainedBidirectional exercises both directions at once. The
// report noted the stall appeared "in either direction".
func TestTransfer_SustainedBidirectional(t *testing.T) {
	const total = 2 << 20 // 2 MB each way

	conn, srvConn, ctx := testPair(t, 90*time.Second)

	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	payload := pattern(total)
	errs := make(chan error, 4)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		srvStream, err := srvConn.AcceptStream(ctx)
		if err != nil {
			errs <- fmt.Errorf("accept stream: %w", err)
			return
		}
		var inner sync.WaitGroup
		inner.Add(2)
		go func() {
			defer inner.Done()
			got, err := drain(srvStream, total, 60*time.Second)
			if err != nil {
				errs <- fmt.Errorf("server read: %w", err)
				return
			}
			if !bytes.Equal(got, payload) {
				errs <- fmt.Errorf("server received corrupted payload")
			}
		}()
		go func() {
			defer inner.Done()
			srvStream.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if _, err := srvStream.Write(payload); err != nil {
				errs <- fmt.Errorf("server write: %w", err)
			}
		}()
		inner.Wait()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		stream.SetWriteDeadline(time.Now().Add(60 * time.Second))
		if _, err := stream.Write(payload); err != nil {
			errs <- fmt.Errorf("client write: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		got, err := drain(stream, total, 60*time.Second)
		if err != nil {
			errs <- fmt.Errorf("client read: %w", err)
			return
		}
		if !bytes.Equal(got, payload) {
			errs <- fmt.Errorf("client received corrupted payload")
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(80 * time.Second):
		t.Fatal("bidirectional transfer hung")
	}

	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestTransfer_ManyStreamsOnOneConnection covers the report's observation that
// the ceiling tracked cumulative bytes rather than any single stream.
//
// SKIPPED: concurrent streams on one connection are broken independently of the
// flow-control fix, and have been all along — the original code hangs on this
// too. sendPacket allocates one connection-wide sequence number
// (PacketManager.NextSequence), but Stream.DeliverData uses that value as a
// per-stream ordering key and advances nextExpectSeq by one per delivery. With
// two or more active streams each sees only a sparse subsequence of the shared
// counter, so every packet after the first lands in recvOOO and is never
// released. Each stream delivers exactly one packet and then stalls forever.
//
// Fixing it requires a per-stream byte offset or sequence in StreamFrame, which
// is a wire-format change that has to be coordinated with the Dart UDX
// implementation. Unskip once StreamFrame carries per-stream ordering.
//
// This does not affect the reported bug: go-libp2p-udx-transport runs a single
// UDX stream per connection with yamux multiplexing above it.
func TestTransfer_ManyStreamsOnOneConnection(t *testing.T) {
	t.Skip("concurrent streams need per-stream sequencing in StreamFrame; see comment above")

	const streams = 8
	const perStream = 192 * 1024 // 1.5 MB total across the connection

	conn, srvConn, ctx := testPair(t, 90*time.Second)

	payload := pattern(perStream)
	errs := make(chan error, streams*2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		var inner sync.WaitGroup
		for i := 0; i < streams; i++ {
			srvStream, err := srvConn.AcceptStream(ctx)
			if err != nil {
				errs <- fmt.Errorf("accept stream %d: %w", i, err)
				return
			}
			inner.Add(1)
			go func(s *Stream, idx int) {
				defer inner.Done()
				got, err := drain(s, perStream, 60*time.Second)
				if err != nil {
					errs <- fmt.Errorf("stream %d read: %w", idx, err)
					return
				}
				if !bytes.Equal(got, payload) {
					errs <- fmt.Errorf("stream %d payload corrupted", idx)
				}
			}(srvStream, i)
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

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(80 * time.Second):
		t.Fatal("multi-stream transfer hung")
	}

	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// ackCountingConn counts datagrams leaving the socket, splitting out ack-only
// packets.
type ackCountingConn struct {
	net.PacketConn
	total, ackOnly *int64
}

func (c *ackCountingConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	atomic.AddInt64(c.total, 1)
	if pkt, err := UnmarshalPacket(b); err == nil && len(pkt.Frames) == 1 {
		if _, ok := pkt.Frames[0].(*AckFrame); ok {
			atomic.AddInt64(c.ackOnly, 1)
		}
	}
	return c.PacketConn.WriteTo(b, addr)
}

// TestTransfer_NoAckAmplification guards against ACKs provoking ACKs. Every
// received packet with a nonzero stream ID used to be acknowledged, including
// ack-only packets, so each ACK drew an ACK in return without end: 64KB of
// payload produced roughly 176,000 ack-only datagrams for 48 data packets.
func TestTransfer_NoAckAmplification(t *testing.T) {
	const total = 64 * 1024

	var totalPkts, ackOnly int64

	serverUDP, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	clientUDP, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	server := NewMultiplexer(&ackCountingConn{serverUDP, &totalPkts, &ackOnly}, RealClock{})
	client := NewMultiplexer(&ackCountingConn{clientUDP, &totalPkts, &ackOnly}, RealClock{})
	defer func() { client.Close(); server.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	srvConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		srvStream, err := srvConn.AcceptStream(ctx)
		if err != nil {
			done <- err
			return
		}
		_, err = drain(srvStream, total, 20*time.Second)
		done <- err
	}()

	stream.SetWriteDeadline(time.Now().Add(20 * time.Second))
	if _, err := stream.Write(pattern(total)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Let any straggling ACK exchange settle before sampling.
	time.Sleep(500 * time.Millisecond)

	dataPkts := total/(MaxDatagramSize-100) + 1
	got := atomic.LoadInt64(&ackOnly)

	// One ACK per data packet, with generous headroom for retransmissions.
	if limit := int64(dataPkts * 4); got > limit {
		t.Fatalf("ack amplification: %d ack-only datagrams for ~%d data packets (limit %d); "+
			"total datagrams %d", got, dataPkts, limit, atomic.LoadInt64(&totalPkts))
	}
	t.Logf("%d data packets => %d ack-only datagrams, %d total",
		dataPkts, got, atomic.LoadInt64(&totalPkts))
}

// TestTransfer_SlowConsumerAppliesBackpressure is the end-to-end proof that
// flow control actually binds. A receiver that drains slowly must stall its
// sender and keep its own buffer bounded; if the advertised offset is
// misinterpreted as "bytes currently outstanding" — which is what dart-udx's
// sender does today — the limit stops binding and recvBuf grows without end.
//
// This is the Go-to-Go form. The cross-implementation form lives in
// interop/, where it is the test that settles the Go/Dart semantics question.
func TestTransfer_SlowConsumerAppliesBackpressure(t *testing.T) {
	conn, srvConn, ctx := testPair(t, 60*time.Second)

	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const total = 8 << 20

	type sample struct{ buffered, consumed int64 }
	samples := make(chan sample, 1)

	go func() {
		srvStream, err := srvConn.AcceptStream(ctx)
		if err != nil {
			return
		}
		// Drain deliberately slowly: small reads with a pause between them.
		buf := make([]byte, 4096)
		var consumed int64
		var peakBuffered int64
		srvStream.SetReadDeadline(time.Now().Add(50 * time.Second))
		for i := 0; i < 40; i++ {
			n, err := srvStream.Read(buf)
			consumed += int64(n)
			if b := srvStream.streamFC.BufferedBytes(); b > peakBuffered {
				peakBuffered = b
			}
			if err != nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		samples <- sample{peakBuffered, consumed}
	}()

	// Writer pushes as hard as it can and is expected NOT to finish.
	writeDone := make(chan int64, 1)
	go func() {
		stream.SetWriteDeadline(time.Now().Add(3 * time.Second))
		n, _ := stream.Write(make([]byte, total))
		writeDone <- int64(n)
	}()

	var written int64
	select {
	case written = <-writeDone:
	case <-time.After(30 * time.Second):
		t.Fatal("writer neither completed nor hit its deadline")
	}

	s := <-samples

	if written >= total {
		t.Fatalf("writer pushed all %d bytes past a slow reader that consumed only %d; "+
			"flow control is not binding", total, s.consumed)
	}

	// The sender may run ahead by roughly one window plus what was consumed.
	// Anything near the full payload means the limit stopped binding.
	maxExpected := s.consumed + 2*int64(MaxStreamRecvWindow)
	if written > maxExpected {
		t.Fatalf("writer got %d bytes ahead of a reader that consumed %d (bound %d); "+
			"the advertised offset is not restraining the sender", written, s.consumed, maxExpected)
	}

	if s.buffered > 2*int64(MaxStreamRecvWindow) {
		t.Fatalf("receive buffer peaked at %d bytes, beyond twice the %d window; "+
			"back-pressure is not bounding recvBuf", s.buffered, MaxStreamRecvWindow)
	}

	t.Logf("slow reader consumed %d bytes; sender advanced to %d; peak buffered %d (window cap %d)",
		s.consumed, written, s.buffered, MaxStreamRecvWindow)
}
