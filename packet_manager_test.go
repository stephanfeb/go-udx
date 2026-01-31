package udx

import (
	"testing"
	"time"
)

func newTestPM(t *testing.T) (*PacketManager, *CongestionController, *MockClock) {
	t.Helper()
	clk := NewMockClock(time.Now())
	lastSeq := -1
	cc := NewCongestionController(clk, func() int { return lastSeq })
	pm := NewPacketManager(clk, cc)
	return pm, cc, clk
}

func TestPacketManager_SendAndAck(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()

	seq := pm.NextSequence()
	pm.SendPacket(&SentPacket{Sequence: seq, Size: 1000, Frames: []Frame{&PingFrame{}}})

	if pm.PendingCount() != 1 {
		t.Fatalf("pending: got %d, want 1", pm.PendingCount())
	}

	acked := pm.HandleAckFrame(&AckFrame{
		LargestAcked:       seq,
		AckDelay:           0,
		FirstAckRangeLength: 1,
	})

	if len(acked) != 1 || acked[0] != seq {
		t.Fatalf("acked: got %v, want [%d]", acked, seq)
	}
	if pm.PendingCount() != 0 {
		t.Fatalf("pending after ack: got %d, want 0", pm.PendingCount())
	}
}

func TestPacketManager_AckWithRanges(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()

	// Send packets 0-9
	for i := 0; i < 10; i++ {
		seq := pm.NextSequence()
		pm.SendPacket(&SentPacket{Sequence: seq, Size: 100, Frames: []Frame{&PingFrame{}}})
	}

	if pm.PendingCount() != 10 {
		t.Fatalf("pending: got %d, want 10", pm.PendingCount())
	}

	// ACK packets 8-9 (first range) and 5-6 (second range, gap of 1)
	acked := pm.HandleAckFrame(&AckFrame{
		LargestAcked:       9,
		AckDelay:           0,
		FirstAckRangeLength: 2, // acks 8,9
		AckRanges: []AckRange{
			{Gap: 1, AckRangeLength: 2}, // skip 7, ack 6,5
		},
	})

	if len(acked) != 4 {
		t.Fatalf("acked count: got %d, want 4", len(acked))
	}
	// Packets 0-4 and 7 should still be pending
	if pm.PendingCount() != 6 {
		t.Fatalf("pending after partial ack: got %d, want 6", pm.PendingCount())
	}
}

func TestPacketManager_SequenceIncrement(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()

	s0 := pm.NextSequence()
	s1 := pm.NextSequence()
	s2 := pm.NextSequence()

	if s0 != 0 || s1 != 1 || s2 != 2 {
		t.Fatalf("sequences: got %d,%d,%d", s0, s1, s2)
	}
}

func TestPacketManager_LastSentSeq(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()

	if pm.LastSentSeq() != -1 {
		t.Fatalf("lastSentSeq initial: got %d, want -1", pm.LastSentSeq())
	}

	pm.SendPacket(&SentPacket{Sequence: 42, Size: 100, Frames: []Frame{&PingFrame{}}})
	if pm.LastSentSeq() != 42 {
		t.Fatalf("lastSentSeq: got %d, want 42", pm.LastSentSeq())
	}
}

func TestPacketManager_GetPacket(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()

	pm.SendPacket(&SentPacket{Sequence: 5, Size: 200, Frames: []Frame{&PingFrame{}}})

	pkt := pm.GetPacket(5)
	if pkt == nil {
		t.Fatal("expected packet 5")
	}
	if pkt.Size != 200 {
		t.Fatalf("size: got %d, want 200", pkt.Size)
	}

	if pm.GetPacket(99) != nil {
		t.Fatal("expected nil for unknown sequence")
	}
}

func TestPacketManager_Destroy(t *testing.T) {
	pm, _, _ := newTestPM(t)

	pm.SendPacket(&SentPacket{Sequence: 0, Size: 100, Frames: []Frame{&PingFrame{}}})
	pm.SendPacket(&SentPacket{Sequence: 1, Size: 100, Frames: []Frame{&PingFrame{}}})

	pm.Destroy()
	if pm.PendingCount() != 0 {
		t.Fatalf("pending after destroy: got %d", pm.PendingCount())
	}
}
