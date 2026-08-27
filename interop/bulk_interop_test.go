package interop

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	udx "github.com/stephanfeb/go-udx"
)

// Bulk Go<->Dart interop. The rest of the interop suite moves 2,000 and 4,096
// bytes and so never reaches a window update, which is how a 256KB transfer
// ceiling survived in a project that already had a Docker + netem harness.
// These tests move megabytes and exercise the slow-consumer path, which is
// where the two implementations' flow-control semantics either agree or do not.

const dartPeerDir = "dartpeer"

// requireDartPeer skips unless the Dart toolchain and the peer package are both
// usable, so the suite stays runnable without a Dart install.
func requireDartPeer(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("dart"); err != nil {
		t.Skip("dart not on PATH; skipping Go<->Dart bulk interop")
	}
	out, err := exec.Command("dart", "pub", "get", "--directory", dartPeerDir).CombinedOutput()
	if err != nil {
		t.Skipf("dart pub get failed for %s: %v\n%s", dartPeerDir, err, out)
	}
}

type peerResult struct {
	bytes int
	err   error
	log   string
}

// startDartPeer launches the bulk peer and reports what it managed to transfer.
func startDartPeer(t *testing.T, ctx context.Context, mode string, port, total int) <-chan peerResult {
	t.Helper()

	script := filepath.Join(dartPeerDir, "bin", "bulk_peer.dart")
	cmd := exec.CommandContext(ctx, "dart", "run", script, mode, strconv.Itoa(port), strconv.Itoa(total))

	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	out := make(chan peerResult, 1)
	go func() {
		var sb strings.Builder
		result := -1
		corrupt := false
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			sb.WriteString(line)
			sb.WriteString("\n")
			switch {
			case strings.HasPrefix(line, "RESULT "):
				result, _ = strconv.Atoi(strings.TrimPrefix(line, "RESULT "))
			case line == "CORRUPT":
				corrupt = true
			}
		}
		waitErr := cmd.Wait()
		if corrupt {
			waitErr = fmt.Errorf("dart peer reported a corrupted payload")
		}
		out <- peerResult{bytes: result, err: waitErr, log: sb.String()}
	}()
	return out
}

// goServer brings up a Go multiplexer and hands back the first accepted stream.
func goServer(t *testing.T, ctx context.Context) (*udx.Multiplexer, int, <-chan *udx.Stream) {
	t.Helper()

	mux, err := udx.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr, ok := mux.Addr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type %T", mux.Addr())
	}

	streams := make(chan *udx.Stream, 1)
	go func() {
		conn, err := mux.Accept(ctx)
		if err != nil {
			return
		}
		s, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		streams <- s
	}()
	return mux, addr.Port, streams
}

// patternByte mirrors the generator in the Dart peer so either side can verify.
func patternByte(i int) byte { return byte(i*31 + i/251) }

func firstMismatch(b []byte, offset int) int {
	for i := range b {
		if b[i] != patternByte(offset+i) {
			return offset + i
		}
	}
	return -1
}

func acceptOrFail(t *testing.T, streams <-chan *udx.Stream) *udx.Stream {
	t.Helper()
	select {
	case s := <-streams:
		return s
	case <-time.After(60 * time.Second):
		t.Fatal("Go never accepted a stream from the Dart peer")
		return nil
	}
}

// TestBulk_DartToGoUpload moves several megabytes Dart -> Go. Any payload past
// ~300KB would have caught the original ceiling on day one.
func TestBulk_DartToGoUpload(t *testing.T) {
	requireDartPeer(t)

	const total = 4 << 20

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	mux, port, streams := goServer(t, ctx)
	defer mux.Close()

	peer := startDartPeer(t, ctx, "send", port, total)
	s := acceptOrFail(t, streams)

	read := 0
	buf := make([]byte, 64*1024)
	s.SetReadDeadline(time.Now().Add(150 * time.Second))
	for read < total {
		n, err := s.Read(buf)
		if bad := firstMismatch(buf[:n], read); bad >= 0 {
			t.Fatalf("payload corrupted at byte %d", bad)
		}
		read += n
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("Go read stalled after %d of %d bytes: %v", read, total, err)
		}
	}

	res := <-peer
	t.Logf("dart peer:\n%s", res.log)
	if read < total {
		t.Fatalf("Go received %d of %d bytes", read, total)
	}
	if res.err != nil {
		t.Fatalf("dart peer failed: %v", res.err)
	}
}

// TestBulk_GoToDartDownload is the same in reverse.
func TestBulk_GoToDartDownload(t *testing.T) {
	requireDartPeer(t)

	const total = 4 << 20

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	mux, port, streams := goServer(t, ctx)
	defer mux.Close()

	peer := startDartPeer(t, ctx, "recv", port, total)
	s := acceptOrFail(t, streams)

	payload := make([]byte, total)
	for i := range payload {
		payload[i] = patternByte(i)
	}

	s.SetWriteDeadline(time.Now().Add(150 * time.Second))
	if n, err := s.Write(payload); err != nil {
		t.Fatalf("Go write stalled after %d of %d bytes: %v", n, total, err)
	}
	s.CloseWrite()

	res := <-peer
	t.Logf("dart peer:\n%s", res.log)
	if res.err != nil {
		t.Fatalf("dart peer failed: %v", res.err)
	}
	if res.bytes < total {
		t.Fatalf("dart peer received %d of %d bytes", res.bytes, total)
	}
}

// TestBulk_DartToGoSlowConsumer settles the Go/Dart flow-control semantics
// question, and is the reason this file exists.
//
// Go advertises an absolute offset (dataConsumed + recvWindow). dart-udx's
// sender gates on `inflight < _remoteReceiveWindow` (stream.dart:170, :273,
// :344) — bytes currently outstanding, not cumulative — so against an offset
// that grows for the life of the stream that comparison stops binding almost
// immediately, leaving cwnd as the only limit on a Dart -> Go upload.
//
// With a deliberately slow Go reader, a sender that respects the offset stalls
// and Go's receive buffer stays bounded. A sender that does not will run far
// ahead of what the application has consumed. If this fails, the fix belongs in
// dart-udx's sender, not here.
func TestBulk_DartToGoSlowConsumer(t *testing.T) {
	requireDartPeer(t)

	const total = 8 << 20

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	mux, port, streams := goServer(t, ctx)
	defer mux.Close()

	peer := startDartPeer(t, ctx, "send", port, total)
	s := acceptOrFail(t, streams)

	// Drain slowly and watch how far the sender runs ahead of consumption.
	var consumed int64
	buf := make([]byte, 4096)
	s.SetReadDeadline(time.Now().Add(30 * time.Second))
	for i := 0; i < 60; i++ {
		n, err := s.Read(buf)
		atomic.AddInt64(&consumed, int64(n))
		if err != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	got := atomic.LoadInt64(&consumed)
	buffered := s.BufferedBytes()
	window := s.RecvWindow()

	cancel()
	res := <-peer
	t.Logf("dart peer:\n%s", res.log)
	t.Logf("Go consumed %d bytes; %d buffered unread; advertised window %d", got, buffered, window)

	// The exact invariant: we advertise dataConsumed + recvWindow as an absolute
	// offset, so a sender that respects it can never leave more than recvWindow
	// bytes outstanding and unconsumed.
	if buffered > window {
		t.Fatalf("Go buffered %d bytes unread against an advertised window of %d — the sender ran %.1fx "+
			"past the offset it was granted.\n\n"+
			"The Dart sender is not respecting the advertised offset. dart-udx stream.dart "+
			"(:170, :273, :344) gates on `inflight < _remoteReceiveWindow`, comparing bytes "+
			"currently outstanding against an offset that grows for the life of the stream, so the "+
			"comparison stops binding almost immediately and cwnd becomes the only limit. It must "+
			"track cumulative bytes sent on the stream and compare that instead. Note `inflight` is "+
			"also the connection-level counter, so it is the wrong quantity for a per-stream limit "+
			"regardless.\n\n"+
			"See RECOMMENDATIONS_FROM_GO_RICOCHET.md P0 and doc/DART_SENDER_PATCH.md.",
			buffered, window, float64(buffered)/float64(window))
	}
}
