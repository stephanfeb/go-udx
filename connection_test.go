package udx

import (
	"context"
	"net"
	"testing"
	"time"
)

func newTestConn(t *testing.T, isInitiator bool) (*Connection, *MockClock) {
	t.Helper()
	clk := NewMockClock(time.Now())
	localCID, _ := RandomConnectionID(DefaultCIDLength)
	remoteCID, _ := RandomConnectionID(DefaultCIDLength)
	localAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
	remoteAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5678}

	var sent [][]byte
	sendFn := func(data []byte, addr net.Addr) error {
		cp := make([]byte, len(data))
		copy(cp, data)
		sent = append(sent, cp)
		return nil
	}

	c := NewConnection(localCID, remoteCID, localAddr, remoteAddr, isInitiator, clk, sendFn)
	c.addrValidated = true // skip anti-amplification for tests
	return c, clk
}

func TestConnection_OpenStream(t *testing.T) {
	c, _ := newTestConn(t, true)
	defer c.Close()

	ctx := context.Background()
	s, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != 1 {
		t.Fatalf("stream ID: got %d, want 1 (initiator starts odd)", s.ID)
	}

	s2, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s2.ID != 3 {
		t.Fatalf("stream ID: got %d, want 3", s2.ID)
	}

	if c.StreamCount() != 2 {
		t.Fatalf("stream count: got %d, want 2", c.StreamCount())
	}
}

func TestConnection_ResponderStreamIDs(t *testing.T) {
	c, _ := newTestConn(t, false)
	defer c.Close()

	ctx := context.Background()
	s, err := c.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != 2 {
		t.Fatalf("stream ID: got %d, want 2 (responder starts even)", s.ID)
	}
}

func TestConnection_MaxStreams(t *testing.T) {
	c, _ := newTestConn(t, true)
	defer c.Close()

	ctx := context.Background()
	for i := 0; i < InitialMaxStreams; i++ {
		_, err := c.OpenStream(ctx)
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}

	_, err := c.OpenStream(ctx)
	if err != ErrMaxStreams {
		t.Fatalf("expected ErrMaxStreams, got %v", err)
	}
}

func TestConnection_AcceptStream(t *testing.T) {
	c, _ := newTestConn(t, false)
	defer c.Close()

	// Simulate incoming SYN
	pkt := &Packet{
		Version:             VersionV2,
		DestinationCID:      c.localCID,
		SourceCID:           c.remoteCID,
		Sequence:            0,
		DestinationStreamID: 0,
		SourceStreamID:      5,
		Frames:              []Frame{&StreamFrame{IsSyn: true, Data: []byte("hello")}},
	}
	c.HandlePacket(pkt)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	s, err := c.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Server assigns its own local ID (2 for responder), remote ID is the sender's stream ID
	if s.ID != 2 {
		t.Fatalf("accepted stream local ID: got %d, want 2", s.ID)
	}
	if s.RemoteID != 5 {
		t.Fatalf("accepted stream remote ID: got %d, want 5", s.RemoteID)
	}
}

func TestConnection_Close(t *testing.T) {
	c, _ := newTestConn(t, true)

	err := c.CloseWithError(ErrorNoError, "bye")
	if err != nil {
		t.Fatal(err)
	}
	if c.State() != ConnStateClosed {
		t.Fatalf("state: got %d, want Closed", c.State())
	}

	// Opening stream after close should fail
	_, err = c.OpenStream(context.Background())
	if err != ErrConnectionClosed {
		t.Fatalf("expected ErrConnectionClosed, got %v", err)
	}
}

func TestConnection_HandleConnectionClose(t *testing.T) {
	c, _ := newTestConn(t, true)

	pkt := &Packet{
		Version:        VersionV2,
		DestinationCID: c.localCID,
		SourceCID:      c.remoteCID,
		Frames:         []Frame{&ConnectionCloseFrame{ErrorCode: ErrorNoError, ReasonPhrase: "done"}},
	}
	c.HandlePacket(pkt)

	if c.State() != ConnStateClosed {
		t.Fatalf("state: got %d, want Closed", c.State())
	}
}

func TestConnection_StreamCount(t *testing.T) {
	c, _ := newTestConn(t, true)
	defer c.Close()

	if c.StreamCount() != 0 {
		t.Fatalf("initial stream count: %d", c.StreamCount())
	}

	c.OpenStream(context.Background())
	if c.StreamCount() != 1 {
		t.Fatalf("stream count: %d", c.StreamCount())
	}
}
