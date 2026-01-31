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
