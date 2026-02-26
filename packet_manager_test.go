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

func TestPacketManager_DetectLostPackets(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()

	// Send packets 0-9
	for i := 0; i < 10; i++ {
		seq := pm.NextSequence()
		pm.SendPacket(&SentPacket{Sequence: seq, Size: 100, Frames: []Frame{&PingFrame{}}})
	}

	// ACK frame acknowledges {5,6,7,8,9} and {0,1,2} with gap at {3,4}.
	// First range: 9 down to 5 (length 5)
	// Gap: 2 (packets 4,3 missing)
	// Second range: 2,1,0 (length 3)
	ackFrame := &AckFrame{
		LargestAcked:        9,
		AckDelay:            0,
		FirstAckRangeLength: 5, // acks 9,8,7,6,5
		AckRanges: []AckRange{
			{Gap: 2, AckRangeLength: 3}, // skip 4,3 then ack 2,1,0
		},
	}

	lost := pm.DetectLostPackets(ackFrame)
	if len(lost) != 2 {
		t.Fatalf("lost count: got %d, want 2", len(lost))
	}

	// Should detect packets 4 and 3 as lost.
	lostMap := map[uint32]bool{}
	for _, seq := range lost {
		lostMap[seq] = true
	}
	if !lostMap[3] || !lostMap[4] {
		t.Fatalf("expected lost packets 3,4 but got %v", lost)
	}
}

func TestPacketManager_DetectLostPackets_SkipsRecentRetransmit(t *testing.T) {
	pm, _, clk := newTestPM(t)
	defer pm.Destroy()

	// Send packets 0-4
	for i := 0; i < 5; i++ {
		seq := pm.NextSequence()
		pm.SendPacket(&SentPacket{Sequence: seq, Size: 100, Frames: []Frame{&PingFrame{}}})
	}

	// Mark packet 2 as recently retransmitted.
	pkt := pm.GetPacket(2)
	pkt.RetransmitCount = 1
	pkt.LastRetransmit = clk.Now()

	// ACK frame: acks {3,4}, gap at {2}, acks {0,1}
	ackFrame := &AckFrame{
		LargestAcked:        4,
		AckDelay:            0,
		FirstAckRangeLength: 2, // acks 4,3
		AckRanges: []AckRange{
			{Gap: 1, AckRangeLength: 2}, // skip 2, ack 1,0
		},
	}

	lost := pm.DetectLostPackets(ackFrame)
	if len(lost) != 0 {
		t.Fatalf("expected no lost packets (recently retransmitted), got %v", lost)
	}

	// Advance time past RTO, should now detect it.
	clk.Advance(6 * time.Second)
	lost = pm.DetectLostPackets(ackFrame)
	if len(lost) != 1 || lost[0] != 2 {
		t.Fatalf("expected lost [2] after RTO, got %v", lost)
	}
}

func TestPacketManager_DetectLostPackets_NoRanges(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()

	pm.SendPacket(&SentPacket{Sequence: 0, Size: 100, Frames: []Frame{&PingFrame{}}})

	// Simple ACK with no additional ranges — no gaps.
	ackFrame := &AckFrame{
		LargestAcked:        0,
		AckDelay:            0,
		FirstAckRangeLength: 1,
	}

	lost := pm.DetectLostPackets(ackFrame)
	if len(lost) != 0 {
		t.Fatalf("expected no lost packets, got %v", lost)
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
