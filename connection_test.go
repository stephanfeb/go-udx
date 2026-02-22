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

func TestConnection_BuildAckFrame_SimpleContiguous(t *testing.T) {
	c, _ := newTestConn(t, true)
	defer c.Close()

	// Simulate receiving packets 1, 2, 3 in order
	c.recvdDataSeqs[1] = struct{}{}
	c.recvdDataSeqs[2] = struct{}{}
	c.recvdDataSeqs[3] = struct{}{}

	ack := c.buildAckFrame(3)
	if ack.LargestAcked != 3 {
		t.Fatalf("LargestAcked: got %d, want 3", ack.LargestAcked)
	}
	if ack.FirstAckRangeLength != 3 {
		t.Fatalf("FirstAckRangeLength: got %d, want 3", ack.FirstAckRangeLength)
	}
	if len(ack.AckRanges) != 0 {
		t.Fatalf("AckRanges: got %d ranges, want 0", len(ack.AckRanges))
	}
}

func TestConnection_BuildAckFrame_SingleGap(t *testing.T) {
	c, _ := newTestConn(t, true)
	defer c.Close()

	// Received 1, 3 (packet 2 lost)
	c.recvdDataSeqs[1] = struct{}{}
	c.recvdDataSeqs[3] = struct{}{}

	ack := c.buildAckFrame(3)
	if ack.LargestAcked != 3 {
		t.Fatalf("LargestAcked: got %d, want 3", ack.LargestAcked)
	}
	if ack.FirstAckRangeLength != 1 {
		t.Fatalf("FirstAckRangeLength: got %d, want 1", ack.FirstAckRangeLength)
	}
	if len(ack.AckRanges) != 1 {
		t.Fatalf("AckRanges: got %d ranges, want 1", len(ack.AckRanges))
	}
	if ack.AckRanges[0].Gap != 1 {
		t.Fatalf("AckRanges[0].Gap: got %d, want 1", ack.AckRanges[0].Gap)
	}
	if ack.AckRanges[0].AckRangeLength != 1 {
		t.Fatalf("AckRanges[0].AckRangeLength: got %d, want 1", ack.AckRanges[0].AckRangeLength)
	}
}

func TestConnection_BuildAckFrame_MultipleGaps(t *testing.T) {
	c, _ := newTestConn(t, true)
	defer c.Close()

	// Received: 1, 3, 5, 6, 7 (missing 2, 4)
	c.recvdDataSeqs[1] = struct{}{}
	c.recvdDataSeqs[3] = struct{}{}
	c.recvdDataSeqs[5] = struct{}{}
	c.recvdDataSeqs[6] = struct{}{}
	c.recvdDataSeqs[7] = struct{}{}

	ack := c.buildAckFrame(7)
	if ack.LargestAcked != 7 {
		t.Fatalf("LargestAcked: got %d, want 7", ack.LargestAcked)
	}
	// First range: 7, 6, 5 (length 3)
	if ack.FirstAckRangeLength != 3 {
		t.Fatalf("FirstAckRangeLength: got %d, want 3", ack.FirstAckRangeLength)
	}
	if len(ack.AckRanges) != 2 {
		t.Fatalf("AckRanges: got %d ranges, want 2", len(ack.AckRanges))
	}
	// Gap of 1 (seq 4), then range of 1 (seq 3)
	if ack.AckRanges[0].Gap != 1 || ack.AckRanges[0].AckRangeLength != 1 {
		t.Fatalf("AckRanges[0]: gap=%d len=%d, want gap=1 len=1",
			ack.AckRanges[0].Gap, ack.AckRanges[0].AckRangeLength)
	}
	// Gap of 1 (seq 2), then range of 1 (seq 1)
	if ack.AckRanges[1].Gap != 1 || ack.AckRanges[1].AckRangeLength != 1 {
		t.Fatalf("AckRanges[1]: gap=%d len=%d, want gap=1 len=1",
			ack.AckRanges[1].Gap, ack.AckRanges[1].AckRangeLength)
	}
}

func TestConnection_BuildAckFrame_MatchesDartParsing(t *testing.T) {
	c, _ := newTestConn(t, true)
	defer c.Close()

	// Test case from Dart ack_handling_test.dart:
	// Received: 6, 7, 10, 11, 12, 14, 15 (gaps at 8, 9, 13)
	for _, seq := range []uint32{6, 7, 10, 11, 12, 14, 15} {
		c.recvdDataSeqs[seq] = struct{}{}
	}

	ack := c.buildAckFrame(15)

	if ack.LargestAcked != 15 {
		t.Fatalf("LargestAcked: got %d, want 15", ack.LargestAcked)
	}
	// First range: 15, 14 (length 2)
	if ack.FirstAckRangeLength != 2 {
		t.Fatalf("FirstAckRangeLength: got %d, want 2", ack.FirstAckRangeLength)
	}
	if len(ack.AckRanges) < 2 {
		t.Fatalf("AckRanges: got %d ranges, want at least 2", len(ack.AckRanges))
	}
	// Gap of 1 (seq 13), then range of 3 (12, 11, 10)
	if ack.AckRanges[0].Gap != 1 || ack.AckRanges[0].AckRangeLength != 3 {
		t.Fatalf("AckRanges[0]: gap=%d len=%d, want gap=1 len=3",
			ack.AckRanges[0].Gap, ack.AckRanges[0].AckRangeLength)
	}
	// Gap of 2 (seqs 9, 8), then range of 2 (7, 6)
	if ack.AckRanges[1].Gap != 2 || ack.AckRanges[1].AckRangeLength != 2 {
		t.Fatalf("AckRanges[1]: gap=%d len=%d, want gap=2 len=2",
			ack.AckRanges[1].Gap, ack.AckRanges[1].AckRangeLength)
	}

	// Verify the ACK frame round-trips through marshal/unmarshal
	data := ack.Marshal()
	parsed, _, err := unmarshalAckFrame(data, 0)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.LargestAcked != ack.LargestAcked ||
		parsed.FirstAckRangeLength != ack.FirstAckRangeLength ||
		len(parsed.AckRanges) != len(ack.AckRanges) {
		t.Fatalf("round-trip mismatch: got largest=%d first=%d ranges=%d",
			parsed.LargestAcked, parsed.FirstAckRangeLength, len(parsed.AckRanges))
	}
}

func TestConnection_BuildAckFrame_ZeroSeq(t *testing.T) {
	c, _ := newTestConn(t, true)
	defer c.Close()

	// seq=0 (control packet) should return simple ACK
	ack := c.buildAckFrame(0)
	if ack.LargestAcked != 0 {
		t.Fatalf("LargestAcked: got %d, want 0", ack.LargestAcked)
	}
	if ack.FirstAckRangeLength != 1 {
		t.Fatalf("FirstAckRangeLength: got %d, want 1", ack.FirstAckRangeLength)
	}
}
