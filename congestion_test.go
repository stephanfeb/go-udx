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
