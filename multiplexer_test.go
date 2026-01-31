package udx

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestMultiplexer_ListenAndDial(t *testing.T) {
	// Create server
	serverAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverConn, err := net.ListenUDP("udp", serverAddr)
	if err != nil {
		t.Fatal(err)
	}

	clk := RealClock{}
	server := NewMultiplexer(serverConn, clk)
	defer server.Close()

	// Create client
	clientAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	clientConn, err := net.ListenUDP("udp", clientAddr)
	if err != nil {
		t.Fatal(err)
	}

	client := NewMultiplexer(clientConn, clk)
	defer client.Close()

	// Dial
	ctx := context.Background()
	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if conn == nil {
		t.Fatal("expected connection")
	}

	if client.ConnectionCount() != 1 {
		t.Fatalf("client conn count: got %d, want 1", client.ConnectionCount())
	}
}

func TestMultiplexer_Close(t *testing.T) {
	addr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	conn, _ := net.ListenUDP("udp", addr)
	m := NewMultiplexer(conn, RealClock{})

	err := m.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Close is idempotent
	err = m.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestListen(t *testing.T) {
	mux, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	addr := mux.Addr()
	if addr == nil {
		t.Fatal("expected non-nil address")
	}
}

func TestDial(t *testing.T) {
	// Start a listener first
	mux, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mux.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	conn, err := Dial(ctx, mux.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if conn == nil {
		t.Fatal("expected connection")
	}
}

func TestMultiplexer_DialAcceptStream(t *testing.T) {
	// Server
	serverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	serverUDP, _ := net.ListenUDP("udp4", serverAddr)
	server := NewMultiplexer(serverUDP, RealClock{})
	defer server.Close()

	// Client
	clientAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	clientUDP, _ := net.ListenUDP("udp4", clientAddr)
	client := NewMultiplexer(clientUDP, RealClock{})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Dial
	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}

	// Accept on server
	srvConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatalf("server accept: %v", err)
	}

	t.Logf("client conn state: %d, server conn state: %d", conn.State(), srvConn.State())
	t.Logf("server connections: %d", server.ConnectionCount())
}

func TestMultiplexer_DialAcceptStreamEcho(t *testing.T) {
	// Server
	serverAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	serverUDP, _ := net.ListenUDP("udp4", serverAddr)
	server := NewMultiplexer(serverUDP, RealClock{})
	defer server.Close()

	// Client
	clientAddr, _ := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	clientUDP, _ := net.ListenUDP("udp4", clientAddr)
	client := NewMultiplexer(clientUDP, RealClock{})
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _ := client.Dial(ctx, server.Addr())
	srvConn, _ := server.Accept(ctx)

	// Open stream on client
	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Write data
	_, err = stream.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	stream.CloseWrite()

	// Accept stream on server
	srvStream, err := srvConn.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("accept stream: %v", err)
	}

	buf := make([]byte, 100)
	n, _ := srvStream.Read(buf)
	t.Logf("server read: %q", buf[:n])

	// Echo back
	srvStream.Write(buf[:n])
	srvStream.CloseWrite()

	// Client read echo
	buf2 := make([]byte, 100)
	n2, _ := stream.Read(buf2)
	t.Logf("client read echo: %q", buf2[:n2])

	if string(buf2[:n2]) != "hello" {
		t.Fatalf("echo mismatch: got %q", buf2[:n2])
	}
}
