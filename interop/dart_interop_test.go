package interop

import (
	"bufio"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	udx "github.com/stephanfeb/go-udx"
)

// TestGoDartInterop starts the Dart echo server, sends a UDX v2 packet, and verifies the echo.
func TestGoDartInterop(t *testing.T) {
	// Start Dart interop server
	dartServer := exec.Command("dart", "run", "bin/interop_server.dart")
	dartServer.Dir = "../../dart-udx"

	stderr, err := dartServer.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}

	if err := dartServer.Start(); err != nil {
		t.Fatalf("start dart server: %v", err)
	}
	defer func() {
		dartServer.Process.Kill()
		dartServer.Wait()
	}()

	// Wait for READY <port>
	scanner := bufio.NewScanner(stderr)
	var serverPort int
	ready := make(chan struct{})
	go func() {
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
	case <-time.After(10 * time.Second):
		t.Fatal("dart server did not become ready in 10s")
	}

	t.Logf("Dart server ready on port %d", serverPort)

	// Create UDP client
	clientAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp", clientAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	serverAddr, _ := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", serverPort))

	// Build a UDX v2 packet with a STREAM frame
	dcid, _ := udx.NewConnectionID([]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08})
	scid, _ := udx.NewConnectionID([]byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18})

	testData := []byte("Hello from Go to Dart!")

	pkt := &udx.Packet{
		Version:             udx.VersionV2,
		DestinationCID:      dcid,
		SourceCID:           scid,
		Sequence:            42,
		DestinationStreamID: 1,
		SourceStreamID:      2,
		Frames: []udx.Frame{
			&udx.StreamFrame{Data: testData},
		},
	}

	pktBytes := udx.MarshalPacket(pkt)
	t.Logf("Sending %d byte packet to Dart server", len(pktBytes))

	_, err = conn.WriteToUDP(pktBytes, serverAddr)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	// Read response
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2000)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}

	t.Logf("Received %d byte response from Dart server", n)

	// Parse response
	resp, err := udx.UnmarshalPacket(buf[:n])
	if err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// Verify
	if resp.Version != udx.VersionV2 {
		t.Fatalf("version: got %d, want %d", resp.Version, udx.VersionV2)
	}
	if !resp.DestinationCID.Equal(scid) {
		t.Fatalf("dest CID: got %s, want %s", resp.DestinationCID, scid)
	}
	if !resp.SourceCID.Equal(dcid) {
		t.Fatalf("src CID: got %s, want %s", resp.SourceCID, dcid)
	}
	if resp.Sequence != 43 {
		t.Fatalf("sequence: got %d, want 43", resp.Sequence)
	}
	if resp.DestinationStreamID != 2 {
		t.Fatalf("dst stream: got %d, want 2", resp.DestinationStreamID)
	}
	if resp.SourceStreamID != 1 {
		t.Fatalf("src stream: got %d, want 1", resp.SourceStreamID)
	}

	if len(resp.Frames) != 1 {
		t.Fatalf("frames: got %d, want 1", len(resp.Frames))
	}
	sf, ok := resp.Frames[0].(*udx.StreamFrame)
	if !ok {
		t.Fatalf("frame type: got %T, want StreamFrame", resp.Frames[0])
	}
	if string(sf.Data) != string(testData) {
		t.Fatalf("data: got %q, want %q", sf.Data, testData)
	}

	t.Log("Go → Dart interop test PASSED")
}
