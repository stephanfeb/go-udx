package interop

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	udx "github.com/stephanfeb/go-udx"
)

// Go<->Dart interop for concurrent streams on one connection.
//
// Both implementations allocate sequence numbers per connection and therefore
// must reassemble there. dart-udx always has (socket.dart, _nextExpectedSeq +
// _connectionReceiveBuffer); go-udx used to reorder inside Stream on the same
// connection-wide number, which is indistinguishable from correct while a
// connection carries one stream and fails completely with two.
//
// The rest of the interop suite is single-stream, so it could not see that. It
// also matters beyond go-udx's own API: dart-libp2p uses UDX as its libp2p
// stream multiplexer directly (udx_transport.dart, streamMultiplexer:
// '/udx/1.0.0'), opening one UDX stream per libp2p stream, where the Go stack
// runs yamux over a single UDX stream. Concurrent streams are a production
// path for one half of the ecosystem and unexercised by the other.
//
// Every stream carries a marker byte at each 512-byte boundary so the receiver
// verifies which stream a payload belongs to. Byte counts alone cannot catch
// bytes delivered intact to the wrong stream.

const markerStride = 512

// markedPayload mirrors markedPattern in the Dart peer.
func markedPayload(n, marker int) []byte {
	b := make([]byte, n)
	for i := range b {
		if i%markerStride == 0 {
			b[i] = byte(marker)
		} else {
			b[i] = patternByte(i)
		}
	}
	return b
}

// goServerStreams accepts exactly n streams from one incoming connection.
func goServerStreams(t *testing.T, ctx context.Context, n int) (*udx.Multiplexer, int, <-chan *udx.Stream) {
	t.Helper()

	mux, err := udx.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := mux.Addr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", mux.Addr())
	}

	streams := make(chan *udx.Stream, n)
	go func() {
		defer close(streams)
		conn, err := mux.Accept(ctx)
		if err != nil {
			return
		}
		for i := 0; i < n; i++ {
			s, err := conn.AcceptStream(ctx)
			if err != nil {
				return
			}
			streams <- s
		}
	}()
	return mux, addr.Port, streams
}

// collectStreams gathers n streams or fails; a hang here is the symptom under
// test, so it must not rely on the package timeout.
func collectStreams(t *testing.T, ch <-chan *udx.Stream, n int, timeout time.Duration) []*udx.Stream {
	t.Helper()
	out := make([]*udx.Stream, 0, n)
	deadline := time.After(timeout)
	for len(out) < n {
		select {
		case s, ok := <-ch:
			if !ok {
				t.Fatalf("Go accepted only %d of %d streams before the peer gave up", len(out), n)
			}
			out = append(out, s)
		case <-deadline:
			t.Fatalf("Go accepted only %d of %d streams within %s", len(out), n, timeout)
		}
	}
	return out
}

// TestMultiStream_DartToGo is the direct interop check on go-udx's connection
// reassembly: real Dart traffic, several streams interleaved in one sequence
// space, verified per stream rather than in aggregate.
func TestMultiStream_DartToGo(t *testing.T) {
	requireDartPeer(t)

	const streams = 4
	const perStream = 512 << 10

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	mux, port, accepted := goServerStreams(t, ctx, streams)
	defer mux.Close()

	peer := startDartPeer(t, ctx, "sendmulti", port, perStream, streams)
	got := collectStreams(t, accepted, streams, 90*time.Second)

	type result struct {
		marker int
		err    error
	}
	results := make(chan result, streams)
	var wg sync.WaitGroup

	for _, s := range got {
		wg.Add(1)
		go func(s *udx.Stream) {
			defer wg.Done()
			payload, err := readN(s, perStream, 180*time.Second)
			if err != nil {
				results <- result{marker: -1, err: err}
				return
			}
			marker := int(payload[0])
			if marker < 0 || marker >= streams {
				results <- result{marker: -1,
					err: fmt.Errorf("unrecognised marker %d: streams were interleaved", marker)}
				return
			}
			if !bytes.Equal(payload, markedPayload(perStream, marker)) {
				results <- result{marker: marker,
					err: fmt.Errorf("stream %d: payload does not match its own marker", marker)}
				return
			}
			results <- result{marker: marker}
		}(s)
	}
	wg.Wait()
	close(results)

	seen := map[int]bool{}
	for r := range results {
		if r.err != nil {
			t.Error(r.err)
			continue
		}
		if seen[r.marker] {
			t.Errorf("marker %d arrived on two streams", r.marker)
		}
		seen[r.marker] = true
	}
	if len(seen) != streams {
		t.Errorf("received %d distinct payloads, want %d", len(seen), streams)
	}

	res := <-peer
	t.Logf("dart peer:\n%s", res.log)
	if res.err != nil {
		t.Fatalf("dart peer failed: %v", res.err)
	}
	if want := perStream * streams; res.bytes < want {
		t.Fatalf("dart peer sent %d of %d bytes", res.bytes, want)
	}
}

// TestMultiStream_GoToDart is the same in reverse, and covers dart-libp2p's
// production shape: many concurrent UDX streams carrying independent payloads
// over one connection.
func TestMultiStream_GoToDart(t *testing.T) {
	requireDartPeer(t)

	const streams = 4
	const perStream = 512 << 10

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	mux, port, accepted := goServerStreams(t, ctx, streams)
	defer mux.Close()

	peer := startDartPeer(t, ctx, "recvmulti", port, perStream, streams)
	got := collectStreams(t, accepted, streams, 90*time.Second)

	errs := make(chan error, streams)
	var wg sync.WaitGroup
	for i, s := range got {
		wg.Add(1)
		go func(s *udx.Stream, marker int) {
			defer wg.Done()
			s.SetWriteDeadline(time.Now().Add(180 * time.Second))
			if n, err := s.Write(markedPayload(perStream, marker)); err != nil {
				errs <- fmt.Errorf("stream %d write stalled after %d of %d bytes: %w",
					marker, n, perStream, err)
				return
			}
			s.CloseWrite()
		}(s, i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	res := <-peer
	t.Logf("dart peer:\n%s", res.log)
	if res.err != nil {
		t.Fatalf("dart peer failed: %v", res.err)
	}
	if want := perStream * streams; res.bytes < want {
		t.Fatalf("dart peer received %d of %d bytes", res.bytes, want)
	}
}

// readN reads exactly n bytes or reports how far it got.
func readN(s *udx.Stream, n int, timeout time.Duration) ([]byte, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 64*1024)
	if err := s.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	for len(out) < n {
		r, err := s.Read(buf)
		out = append(out, buf[:r]...)
		if err != nil {
			if err == io.EOF {
				break
			}
			return out, fmt.Errorf("stalled after %d of %d bytes: %w", len(out), n, err)
		}
	}
	if len(out) < n {
		return out, fmt.Errorf("received %d of %d bytes", len(out), n)
	}
	return out, nil
}
