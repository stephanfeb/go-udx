package interop

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	udx "github.com/stephanfeb/go-udx"
)

// TestDartMuxToGoMux tests Dart's UDXMultiplexer dialing into Go's Multiplexer.
// This validates the full Connection + Stream lifecycle across implementations,
// not just raw packet echo.
func TestDartMuxToGoMux(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	mux := udx.NewMultiplexer(conn, udx.RealClock{})
	defer mux.Close()

	port := conn.LocalAddr().(*net.UDPAddr).Port
	t.Logf("Go UDX server on port %d", port)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	serverDone := make(chan error, 1)
	go func() {
		t.Log("Go: waiting for Accept...")
		udxConn, err := mux.Accept(ctx)
		if err != nil {
			serverDone <- fmt.Errorf("accept conn: %w", err)
			return
		}
		t.Logf("Go: connection accepted from %s", udxConn.RemoteAddr())

		t.Log("Go: waiting for AcceptStream...")
		stream, err := udxConn.AcceptStream(ctx)
		if err != nil {
			serverDone <- fmt.Errorf("accept stream: %w", err)
			return
		}
		t.Logf("Go: stream accepted ID=%d RemoteID=%d", stream.ID, stream.RemoteID)

		buf := make([]byte, 4096)
		n, err := stream.Read(buf)
		if err != nil && err != io.EOF {
			serverDone <- fmt.Errorf("read: %w (got %d bytes)", err, n)
			return
		}
		t.Logf("Go: read %d bytes: %q", n, buf[:n])

		_, err = stream.Write(buf[:n])
		if err != nil {
			serverDone <- fmt.Errorf("write echo: %w", err)
			return
		}
		stream.Close()
		serverDone <- nil
	}()

	// Launch Dart mux echo client
	dartCmd := exec.CommandContext(ctx, "dart", "run", "bin/mux_echo_client.dart", fmt.Sprintf("%d", port))
	dartCmd.Dir = "../../dart-udx"

	output, err := dartCmd.CombinedOutput()
	t.Logf("[dart] %s", string(output))

	if err != nil {
		t.Logf("dart client error: %v", err)
	}

	select {
	case srvErr := <-serverDone:
		if srvErr != nil {
			t.Fatal("Go server:", srvErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for Go server")
	}

	if err != nil {
		t.Fatal("dart client:", err)
	}
	t.Log("Dart → Go Multiplexer-level interop PASSED")
}

// TestGoMuxToDartMux tests Go's Multiplexer dialing into Dart's UDXMultiplexer.
func TestGoMuxToDartMux(t *testing.T) {
	// Start Dart echo server
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dartServer := exec.CommandContext(ctx, "dart", "run", "bin/interop_server.dart")
	dartServer.Dir = "../../dart-udx"

	stderr, err := dartServer.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := dartServer.Start(); err != nil {
		t.Fatal("start dart server:", err)
	}
	defer func() {
		dartServer.Process.Kill()
		dartServer.Wait()
	}()

	// Wait for READY <port>
	var serverPort int
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			t.Logf("[dart-server] %s", line)
			if strings.HasPrefix(line, "READY ") {
				port, err := strconv.Atoi(strings.TrimPrefix(line, "READY "))
				if err == nil {
					serverPort = port
					close(ready)
				}
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("dart server did not become ready in 15s")
	}

	t.Logf("Dart server ready on port %d", serverPort)

	// Create Go UDX multiplexer and dial
	clientConn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		t.Fatal(err)
	}
	clientMux := udx.NewMultiplexer(clientConn, udx.RealClock{})
	defer clientMux.Close()

	serverAddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", serverPort))

	t.Log("Go: dialing Dart server...")
	udxConn, err := clientMux.Dial(ctx, serverAddr)
	if err != nil {
		t.Fatal("dial:", err)
	}

	t.Log("Go: opening stream...")
	stream, err := udxConn.OpenStream(ctx)
	if err != nil {
		t.Fatal("open stream:", err)
	}
	t.Logf("Go: stream ID=%d RemoteID=%d", stream.ID, stream.RemoteID)

	testData := []byte("hello from go mux")
	_, err = stream.Write(testData)
	if err != nil {
		t.Fatal("write:", err)
	}
	stream.Close()

	buf := make([]byte, 4096)
	n, err := stream.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal("read echo:", err)
	}
	if string(buf[:n]) != string(testData) {
		t.Fatalf("echo mismatch: got %q, want %q", buf[:n], testData)
	}

	t.Log("Go → Dart Multiplexer-level interop PASSED")
}
