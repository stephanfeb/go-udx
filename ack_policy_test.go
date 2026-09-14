package udx

import (
	"net"
	"testing"
	"time"
)

// newAckTestConn is newTestConn with the datagrams it sends exposed.
func newAckTestConn(t *testing.T) (*Connection, *MockClock, *[][]byte) {
	t.Helper()
	clk := NewMockClock(time.Now())
	localCID, _ := RandomConnectionID(DefaultCIDLength)
	remoteCID, _ := RandomConnectionID(DefaultCIDLength)
	localAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
	remoteAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5678}
	sent := &[][]byte{}
	c := NewConnection(localCID, remoteCID, localAddr, remoteAddr, false, clk, func(data []byte, addr net.Addr) error {
		cp := make([]byte, len(data))
		copy(cp, data)
		*sent = append(*sent, cp)
		return nil
	})
	c.addrValidated = true
	t.Cleanup(func() { c.Close() })
	return c, clk, sent
}

// acksIn returns the ACK frames among the sent datagrams, in order.
func acksIn(t *testing.T, sent [][]byte) []*AckFrame {
	t.Helper()
	var acks []*AckFrame
	for _, d := range sent {
		pkt, err := UnmarshalPacket(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range pkt.Frames {
			if a, ok := f.(*AckFrame); ok {
				acks = append(acks, a)
			}
		}
	}
	return acks
}

// dataPacket is a one-byte stream packet with the given sequence.
func ackDataPacket(seq uint32, syn, fin bool) *Packet {
	return &Packet{
		Version:             VersionCurrent,
		Sequence:            seq,
		DestinationStreamID: 1,
		SourceStreamID:      1,
		Frames:              []Frame{&StreamFrame{IsSyn: syn, IsFin: fin, Offset: uint64(seq - 1), Data: []byte{'x'}}},
	}
}

// TestAck_EverySecondInOrderPacket is the policy's steady state: in-order
// data draws one ACK per AckElicitingThreshold packets, and a lone trailing
// packet waits for the timer rather than answering at once.
func TestAck_EverySecondInOrderPacket(t *testing.T) {
	c, _, sent := newAckTestConn(t)
	for seq := uint32(1); seq <= 4; seq++ {
		c.HandlePacket(ackDataPacket(seq, false, false))
	}
	acks := acksIn(t, *sent)
	if len(acks) != 2 {
		t.Fatalf("4 in-order packets drew %d ACKs, want 2 (one per %d)", len(acks), AckElicitingThreshold)
	}
	if acks[1].LargestAcked != 4 || acks[1].FirstAckRangeLength != 4 {
		t.Fatalf("second ACK = largest %d run %d, want 4/4", acks[1].LargestAcked, acks[1].FirstAckRangeLength)
	}

	c.HandlePacket(ackDataPacket(5, false, false))
	if got := len(acksIn(t, *sent)); got != 2 {
		t.Fatalf("a fifth packet was acknowledged at once (%d ACKs); it should wait for the timer", got)
	}
	c.ackMu.Lock()
	pending, armed := c.ackPending, c.ackTimer != nil
	c.ackMu.Unlock()
	if pending != 1 || !armed {
		t.Fatalf("pending=%d timer armed=%v after an odd packet, want 1/true", pending, armed)
	}
}

// TestAck_ImmediateOnOutOfOrder: a gap, and later the packet that fills it,
// are each acknowledged on arrival so the sender sees the loss and its
// repair without waiting.
func TestAck_ImmediateOnOutOfOrder(t *testing.T) {
	c, _, sent := newAckTestConn(t)
	c.HandlePacket(ackDataPacket(1, false, false))
	c.HandlePacket(ackDataPacket(2, false, false)) // threshold: ACK #1
	c.HandlePacket(ackDataPacket(4, false, false)) // gap at 3: ACK #2 at once
	acks := acksIn(t, *sent)
	if len(acks) != 2 {
		t.Fatalf("packet 4 arriving after 2 drew %d ACKs in total, want 2 (the gap must be reported at once)", len(acks))
	}
	if a := acks[1]; a.LargestAcked != 4 || a.FirstAckRangeLength != 1 || len(a.AckRanges) != 1 || a.AckRanges[0].Gap != 1 || a.AckRanges[0].AckRangeLength != 2 {
		t.Fatalf("gap ACK = %+v, want largest 4, run 1, one range gap 1 len 2", a)
	}
	c.HandlePacket(ackDataPacket(3, false, false)) // fills the gap: ACK #3 at once
	acks = acksIn(t, *sent)
	if len(acks) != 3 {
		t.Fatalf("the packet filling the gap drew no immediate ACK (%d total)", len(acks))
	}
	if a := acks[2]; a.LargestAcked != 4 || a.FirstAckRangeLength != 4 || len(a.AckRanges) != 0 {
		t.Fatalf("filled ACK = %+v, want largest 4, run 4, no ranges", a)
	}
}

// TestAck_ImmediateOnSynAndFin: stream edges are acknowledged at once; a
// dial or stream open is waiting on the SYN's ACK.
func TestAck_ImmediateOnSynAndFin(t *testing.T) {
	c, _, sent := newAckTestConn(t)
	c.HandlePacket(ackDataPacket(1, true, false))
	if got := len(acksIn(t, *sent)); got != 1 {
		t.Fatalf("SYN drew %d ACKs, want 1 at once", got)
	}
	c.HandlePacket(ackDataPacket(2, false, true))
	if got := len(acksIn(t, *sent)); got != 2 {
		t.Fatalf("FIN drew %d ACKs in total, want 2", got)
	}
}

// TestAck_TimerFlushesWithHonestDelay: the timer sends what is pending and
// the frame says how long the largest packet waited, so the sender's RTT
// sample can subtract it.
func TestAck_TimerFlushesWithHonestDelay(t *testing.T) {
	c, clk, sent := newAckTestConn(t)
	c.HandlePacket(ackDataPacket(1, false, false))
	if got := len(acksIn(t, *sent)); got != 0 {
		t.Fatalf("a lone packet drew %d ACKs before the timer", got)
	}
	clk.Advance(7 * time.Millisecond)
	c.ackTimerFired(1, 1)
	acks := acksIn(t, *sent)
	if len(acks) != 1 {
		t.Fatalf("timer sent %d ACKs, want 1", len(acks))
	}
	if acks[0].LargestAcked != 1 || acks[0].AckDelay != 7 {
		t.Fatalf("timer ACK = largest %d delay %dms, want 1 / 7ms", acks[0].LargestAcked, acks[0].AckDelay)
	}
	c.ackMu.Lock()
	pending, armed := c.ackPending, c.ackTimer != nil
	c.ackMu.Unlock()
	if pending != 0 || armed {
		t.Fatalf("after the timer pending=%d armed=%v, want 0/false", pending, armed)
	}
	// Nothing pending: a spurious timer sends nothing.
	c.ackTimerFired(1, 1)
	if got := len(acksIn(t, *sent)); got != 1 {
		t.Fatalf("an idle timer sent an ACK (%d total)", got)
	}
}

// TestAck_ImmediateAckReportsNoDelay: an ACK sent on arrival says so.
func TestAck_ImmediateAckReportsNoDelay(t *testing.T) {
	c, clk, sent := newAckTestConn(t)
	c.HandlePacket(ackDataPacket(1, false, false))
	clk.Advance(3 * time.Millisecond)
	c.HandlePacket(ackDataPacket(2, false, false))
	acks := acksIn(t, *sent)
	if len(acks) != 1 || acks[0].AckDelay != 0 {
		t.Fatalf("threshold ACK = %+v, want one frame with AckDelay 0 (the largest packet arrived just now)", acks)
	}
}

// TestAck_WindowGrowsByEveryByteAcked: one ACK covering two packets grows the
// window by both, not by the largest alone.
func TestAck_WindowGrowsByEveryByteAcked(t *testing.T) {
	c, _, sent := newAckTestConn(t)
	before := c.cc.Cwnd()
	payload := make([]byte, 1000)
	c.sendPacket(1, 1, []Frame{&StreamFrame{Offset: 0, Data: payload}})
	c.sendPacket(1, 1, []Frame{&StreamFrame{Offset: 1000, Data: payload}})
	if len(*sent) != 2 {
		t.Fatalf("sent %d datagrams, want 2", len(*sent))
	}
	both := len((*sent)[0]) + len((*sent)[1])
	second, err := UnmarshalPacket((*sent)[1])
	if err != nil {
		t.Fatal(err)
	}
	c.handleAckFrame(&AckFrame{LargestAcked: second.Sequence, FirstAckRangeLength: 2})
	if got := c.cc.Cwnd() - before; got != both {
		t.Fatalf("cwnd grew by %d for an ACK of two packets totalling %d bytes", got, both)
	}
	if c.cc.Inflight() != 0 {
		t.Fatalf("inflight %d after everything was acked", c.cc.Inflight())
	}
}

// TestRecvTracker_ForgetsBeyondHistory: the run below the largest stops at the
// history, and a sequence that far back is neither remembered nor
// acknowledged when it turns up late.
func TestRecvTracker_ForgetsBeyondHistory(t *testing.T) {
	var r recvTracker
	for seq := uint32(1); seq <= ackHistory+100; seq++ {
		if r.add(seq) {
			t.Fatalf("in-order seq %d reported out of order", seq)
		}
	}
	f := r.frame(0)
	if f.LargestAcked != ackHistory+100 || f.FirstAckRangeLength != ackHistory || len(f.AckRanges) != 0 {
		t.Fatalf("frame = largest %d run %d ranges %d, want %d/%d/0", f.LargestAcked, f.FirstAckRangeLength, len(f.AckRanges), ackHistory+100, ackHistory)
	}
	if r.has(50) {
		t.Fatal("seq 50 is still remembered beyond the history")
	}
	if !r.add(50) {
		t.Fatal("a late packet beyond the history is not reported out of order")
	}
	if r.has(50) {
		t.Fatal("a late packet beyond the history was recorded (it cannot be expressed in a frame)")
	}
}

// TestRecvTracker_JumpClearsTheSlotsItPasses: sequences the largest jumps
// over read as missing even though their bitmap slots held older sequences.
func TestRecvTracker_JumpClearsTheSlotsItPasses(t *testing.T) {
	var r recvTracker
	for seq := uint32(1); seq <= 10; seq++ {
		r.add(seq)
	}
	// Jump by exactly the history: slots for 11..522 alias 11-512.. i.e.
	// every slot, including those holding 1..10.
	if !r.add(10 + ackHistory) {
		t.Fatal("a jump was not reported out of order")
	}
	f := r.frame(0)
	if f.FirstAckRangeLength != 1 || len(f.AckRanges) != 0 {
		t.Fatalf("after a jump the frame = run %d ranges %d; stale slots are being read as received", f.FirstAckRangeLength, len(f.AckRanges))
	}
	// A short jump: 1..10, then 14. 11..13 must read as a gap of 3.
	var s recvTracker
	for seq := uint32(1); seq <= 10; seq++ {
		s.add(seq)
	}
	s.add(14)
	g := s.frame(0)
	if g.FirstAckRangeLength != 1 || len(g.AckRanges) != 1 || g.AckRanges[0].Gap != 3 || g.AckRanges[0].AckRangeLength != 10 {
		t.Fatalf("frame after 1..10,14 = %+v, want run 1, gap 3, run 10", g)
	}
}
