package udx

import (
	"testing"
	"time"
)

func newTestCC(t *testing.T) (*CongestionController, *MockClock) {
	t.Helper()
	clk := NewMockClock(time.Now())
	lastSeq := 0
	cc := NewCongestionController(clk, func() int { return lastSeq })
	return cc, clk
}

func TestCongestionController_Initial(t *testing.T) {
	cc, _ := newTestCC(t)
	defer cc.Destroy()

	if cc.Cwnd() != InitialCwnd {
		t.Fatalf("initial cwnd: got %d, want %d", cc.Cwnd(), InitialCwnd)
	}
	if !cc.CanSend(1000) {
		t.Fatal("should be able to send with empty inflight")
	}
}

func TestCongestionController_SlowStart(t *testing.T) {
	cc, clk := newTestCC(t)
	defer cc.Destroy()

	initialCwnd := cc.Cwnd()

	// Send and ACK a packet — should grow cwnd in slow start
	sentTime := clk.Now()
	cc.OnPacketSent(1000)
	clk.Advance(50 * time.Millisecond)
	cc.OnPacketAcked(1000, sentTime, 0, true, 1)

	if cc.Cwnd() <= initialCwnd {
		t.Fatalf("cwnd should grow in slow start: got %d, initial %d", cc.Cwnd(), initialCwnd)
	}
	// Slow start: cwnd += bytes acked
	expected := initialCwnd + 1000
	if cc.Cwnd() != expected {
		t.Fatalf("cwnd: got %d, want %d", cc.Cwnd(), expected)
	}
}

func TestCongestionController_RTTFirstSample(t *testing.T) {
	cc, clk := newTestCC(t)
	defer cc.Destroy()

	sentTime := clk.Now()
	cc.OnPacketSent(1000)
	clk.Advance(80 * time.Millisecond)
	cc.OnPacketAcked(1000, sentTime, 5*time.Millisecond, true, 1)

	// First sample: smoothedRtt = latestRtt = 80ms - 5ms = 75ms
	sRtt := cc.SmoothedRtt()
	if sRtt != 75*time.Millisecond {
		t.Fatalf("smoothedRtt: got %v, want 75ms", sRtt)
	}
	// rttVar = latestRtt / 2 = 37.5ms
	rVar := cc.RttVar()
	expected := 75 * time.Millisecond / 2
	if rVar != expected {
		t.Fatalf("rttVar: got %v, want %v", rVar, expected)
	}
}

func TestCongestionController_RTTSubsequentSample(t *testing.T) {
	cc, clk := newTestCC(t)
	defer cc.Destroy()

	// First sample
	sent1 := clk.Now()
	cc.OnPacketSent(1000)
	clk.Advance(80 * time.Millisecond)
	cc.OnPacketAcked(1000, sent1, 0, true, 1)

	firstSmoothed := cc.SmoothedRtt()

	// Second sample with different RTT
	sent2 := clk.Now()
	cc.OnPacketSent(1000)
	clk.Advance(100 * time.Millisecond)
	cc.OnPacketAcked(1000, sent2, 0, true, 2)

	// Should use EWMA, not overwrite
	if cc.SmoothedRtt() == firstSmoothed {
		t.Fatal("smoothedRtt should change with second sample")
	}
}

func TestCongestionController_AckDelayCappping(t *testing.T) {
	cc, clk := newTestCC(t)
	defer cc.Destroy()

	sentTime := clk.Now()
	cc.OnPacketSent(1000)
	clk.Advance(100 * time.Millisecond)
	// ackDelay larger than MaxAckDelay should be capped
	cc.OnPacketAcked(1000, sentTime, 50*time.Millisecond, true, 1)

	// latestRtt = 100ms - 25ms (capped) = 75ms
	sRtt := cc.SmoothedRtt()
	if sRtt != 75*time.Millisecond {
		t.Fatalf("smoothedRtt: got %v, want 75ms (ack delay should be capped)", sRtt)
	}
}

func TestCongestionController_OnPacketLost(t *testing.T) {
	cc, clk := newTestCC(t)
	defer cc.Destroy()

	// Grow cwnd a bit via slow start
	for i := 0; i < 5; i++ {
		sent := clk.Now()
		cc.OnPacketSent(1000)
		clk.Advance(50 * time.Millisecond)
		cc.OnPacketAcked(1000, sent, 0, true, i+1)
	}

	cwndBefore := cc.Cwnd()
	cc.OnPacketLost(1000)

	if !cc.InRecovery() {
		t.Fatal("should be in recovery after loss")
	}
	if cc.Cwnd() >= cwndBefore {
		t.Fatalf("cwnd should decrease on loss: got %d, was %d", cc.Cwnd(), cwndBefore)
	}
	// cwnd = ssthresh = wMax * betaCubic
	expected := int(float64(cwndBefore) * BetaCubic)
	if cc.Cwnd() != expected {
		t.Fatalf("cwnd: got %d, want %d", cc.Cwnd(), expected)
	}
}

func TestCongestionController_DuplicateAckFastRetransmit(t *testing.T) {
	cc, _ := newTestCC(t)
	defer cc.Destroy()

	retransmitted := -1
	cc.OnFastRetransmit = func(seq int) {
		retransmitted = seq
	}

	// 3 duplicate ACKs for same largestAcked should trigger fast retransmit
	cc.ProcessDuplicateAck(5)
	cc.ProcessDuplicateAck(5)
	cc.ProcessDuplicateAck(5)

	if retransmitted != 6 { // largestAcked+1
		t.Fatalf("expected fast retransmit for seq 6, got %d", retransmitted)
	}
	if !cc.InRecovery() {
		t.Fatal("should be in recovery after 3 dup ACKs")
	}
}

func TestCongestionController_NoGrowthDuringRecovery(t *testing.T) {
	cc, clk := newTestCC(t)
	defer cc.Destroy()

	// Enter recovery via loss
	cc.OnPacketSent(1000)
	cc.OnPacketLost(1000)

	cwndInRecovery := cc.Cwnd()

	// ACK during recovery should not grow cwnd
	sent := clk.Now()
	cc.OnPacketSent(1000)
	clk.Advance(50 * time.Millisecond)
	// isNewCumulativeAck=true but largestAcked <= recoveryEndSeq
	cc.OnPacketAcked(1000, sent, 0, true, 0)

	if cc.Cwnd() != cwndInRecovery {
		t.Fatalf("cwnd should not grow during recovery: got %d, was %d", cc.Cwnd(), cwndInRecovery)
	}
}

func TestCongestionController_PTO(t *testing.T) {
	cc, _ := newTestCC(t)
	defer cc.Destroy()

	pto := cc.PTO()
	if pto < 200*time.Millisecond || pto > 5*time.Second {
		t.Fatalf("PTO out of range: %v", pto)
	}
}

func TestCongestionController_CanSend(t *testing.T) {
	cc, _ := newTestCC(t)
	defer cc.Destroy()

	cwnd := cc.Cwnd()
	cc.OnPacketSent(cwnd)

	if cc.CanSend(1) {
		t.Fatal("should not be able to send when cwnd is full")
	}
}

func TestCongestionController_MinRttUpdates(t *testing.T) {
	cc, clk := newTestCC(t)
	defer cc.Destroy()

	// First ACK with 80ms RTT
	sent1 := clk.Now()
	cc.OnPacketSent(1000)
	clk.Advance(80 * time.Millisecond)
	cc.OnPacketAcked(1000, sent1, 0, true, 1)

	if cc.MinRtt() != 80*time.Millisecond {
		t.Fatalf("minRtt: got %v, want 80ms", cc.MinRtt())
	}

	// Second ACK with 60ms RTT — should update minRtt
	sent2 := clk.Now()
	cc.OnPacketSent(1000)
	clk.Advance(60 * time.Millisecond)
	cc.OnPacketAcked(1000, sent2, 0, true, 2)

	if cc.MinRtt() != 60*time.Millisecond {
		t.Fatalf("minRtt: got %v, want 60ms", cc.MinRtt())
	}
}

func TestPacingController_InitialRate(t *testing.T) {
	clk := NewMockClock(time.Now())
	p := NewPacingController(clk)

	// Initially infinite rate, can send immediately
	if d := p.TimeUntilSend(); d != 0 {
		t.Fatalf("should be able to send immediately, got %v", d)
	}
}

func TestPacingController_UpdateRate(t *testing.T) {
	clk := NewMockClock(time.Now())
	p := NewPacingController(clk)

	p.UpdateRate(14720, 100*time.Millisecond)
	rate := p.PacingRate()
	// Expected: 2.88 * 14720 / 0.1 = 423936 bytes/sec
	expected := PacingGain * float64(14720) / 0.1
	if rate != expected {
		t.Fatalf("pacing rate: got %f, want %f", rate, expected)
	}
}

func TestPacingController_OnPacketSent(t *testing.T) {
	clk := NewMockClock(time.Now())
	p := NewPacingController(clk)

	p.UpdateRate(14720, 100*time.Millisecond)
	p.OnPacketSent(1472)

	// Should now have to wait
	delay := p.TimeUntilSend()
	if delay == 0 {
		t.Fatal("should have a pacing delay after sending")
	}

	// Advance past the delay
	clk.Advance(delay + time.Millisecond)
	if d := p.TimeUntilSend(); d != 0 {
		t.Fatalf("should be able to send after waiting, got %v", d)
	}
}

func TestPacingController_ZeroMinRtt(t *testing.T) {
	clk := NewMockClock(time.Now())
	p := NewPacingController(clk)

	p.UpdateRate(14720, 0)
	// Zero minRtt => infinite rate
	if d := p.TimeUntilSend(); d != 0 {
		t.Fatalf("should send immediately with zero minRtt, got %v", d)
	}
}
