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

	if len(acked) != 1 || acked[0].Sequence != seq {
		t.Fatalf("acked: got %v, want [%d]", acked, seq)
	}
	// The acked packet must come back with its size and send time intact: the
	// congestion controller needs both, and they are unrecoverable once the
	// entry is deleted from the manager.
	if acked[0].Size != 1000 {
		t.Fatalf("acked packet size: got %d, want 1000", acked[0].Size)
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
	// Packets 0-4 and 7 are still pending: loss is declared by
	// DetectLostPackets, which the connection runs after booking the ACK.
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

// Loss is declared from acknowledgements (RFC 9002 section 6.1): a packet
// below the largest acknowledged is lost once it is LossReorderThreshold
// behind it, or older than the loss delay. What is lost leaves tracking and
// goes to OnPacketsLost, in sequence order.
func TestPacketManager_LossFromAckGaps(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()
	var lost []*SentPacket
	pm.OnPacketsLost = func(l []*SentPacket) { lost = append(lost, l...) }

	// Send packets 0-9
	for i := 0; i < 10; i++ {
		seq := pm.NextSequence()
		pm.SendPacket(&SentPacket{Sequence: seq, Size: 100, Frames: []Frame{&PingFrame{}}})
	}

	// Acknowledge 5-9 and 0-2, leaving 3 and 4: both are at least three
	// behind 9, so both are lost on this ACK.
	pm.HandleAckFrame(&AckFrame{
		LargestAcked:        9,
		FirstAckRangeLength: 5, // acks 9,8,7,6,5
		AckRanges: []AckRange{
			{Gap: 2, AckRangeLength: 3}, // skip 4,3 then ack 2,1,0
		},
	})
	pm.DetectLostPackets()

	if len(lost) != 2 || lost[0].Sequence != 3 || lost[1].Sequence != 4 {
		t.Fatalf("lost = %v, want packets 3 and 4 in order", seqsOf(lost))
	}
	if pm.PendingCount() != 0 {
		t.Fatalf("%d packets still tracked after everything was acked or lost", pm.PendingCount())
	}
}

// A receiver that acknowledges one packet at a time sends no ranges. The
// packets below its largest are still declared lost: by the packet threshold
// when far enough behind, by the time threshold otherwise. This is the
// signal a go-udx sender never got from a dart-udx receiver.
func TestPacketManager_LossFromSinglePacketAcks(t *testing.T) {
	pm, _, clk := newTestPM(t)
	defer pm.Destroy()
	var lost []*SentPacket
	pm.OnPacketsLost = func(l []*SentPacket) { lost = append(lost, l...) }

	for i := 0; i < 5; i++ {
		pm.SendPacket(&SentPacket{Sequence: pm.NextSequence(), Size: 100, Frames: []Frame{&PingFrame{}}})
	}

	// Only 4 is acknowledged. 0 and 1 are three or more behind: lost now.
	// 2 and 3 are not, and were just sent: they wait on the loss timer.
	pm.HandleAckFrame(&AckFrame{LargestAcked: 4, FirstAckRangeLength: 1})
	pm.DetectLostPackets()
	if got := seqsOf(lost); len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("lost after the ACK = %v, want [0 1]", got)
	}
	if !pm.LossTimerArmed() {
		t.Fatal("packets 2 and 3 are behind the largest acked but too young; the loss timer must be armed")
	}

	// Once they are older than the loss delay, the timer declares them lost
	// without another ACK.
	lost = nil
	clk.Advance(pm.lossDelay() + time.Millisecond)
	pm.onLossTimer()
	if got := seqsOf(lost); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("lost after the loss delay = %v, want [2 3]", got)
	}
	if pm.LossTimerArmed() {
		t.Fatal("nothing is waiting; the loss timer must not be armed")
	}
}

// A packet that was re-sent is tracked under its fresh sequence, above the
// largest acknowledged, so the same ACK cannot declare it lost twice.
func TestPacketManager_ResentPacketIsNotLostAgainBySameAck(t *testing.T) {
	pm, _, _ := newTestPM(t)
	defer pm.Destroy()
	var lost []*SentPacket
	pm.OnPacketsLost = func(l []*SentPacket) { lost = append(lost, l...) }

	for i := 0; i < 5; i++ {
		pm.SendPacket(&SentPacket{Sequence: pm.NextSequence(), Size: 100, Frames: []Frame{&PingFrame{}}})
	}
	pm.HandleAckFrame(&AckFrame{LargestAcked: 4, FirstAckRangeLength: 1})
	pm.DetectLostPackets()
	if len(lost) != 2 {
		t.Fatalf("lost = %v, want packets 0 and 1", seqsOf(lost))
	}
	pkt := lost[0]
	seq, ok := pm.Resend(pkt)
	if !ok || seq != 5 {
		t.Fatalf("Resend = (%d, %v), want the fresh sequence 5", seq, ok)
	}
	lost = nil
	pm.HandleAckFrame(&AckFrame{LargestAcked: 4, FirstAckRangeLength: 1})
	pm.DetectLostPackets()
	if len(lost) != 0 {
		t.Fatalf("the re-sent packet (now sequence 5) was declared lost by an ACK of 4: %v", seqsOf(lost))
	}
	if pm.GetPacket(5) != pkt {
		t.Fatal("the re-sent packet is not tracked under its fresh sequence")
	}
}

func seqsOf(pkts []*SentPacket) []uint32 {
	out := make([]uint32, 0, len(pkts))
	for _, p := range pkts {
		out = append(out, p.Sequence)
	}
	return out
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
