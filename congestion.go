package udx

import (
	"math"
	"sync"
	"time"
)

// CongestionController implements QUIC-style congestion control based on RFC 9002
// with CUBIC congestion avoidance.
type CongestionController struct {
	clock Clock
	mu    sync.Mutex

	// Congestion window in bytes
	cwnd int

	// Slow start threshold
	ssthresh int

	// CUBIC state
	wMax       int     // Window size before last congestion event (bytes)
	k          float64 // Time for window to grow back to wMax (seconds)
	epochStart time.Time

	// RTT estimation (RFC 9002 Section 5)
	smoothedRtt    time.Duration
	rttVar         time.Duration
	minRtt         time.Duration
	latestRtt      time.Duration
	firstRttSample bool

	// In-flight tracking
	inflight int

	// Duplicate ACK tracking
	dupAcks              int
	lastAckedForDupCount int
	highestCumulativeAck int

	// Recovery state
	inRecovery            bool
	recoveryEndSeq        int
	lostPacketInRecovery  int

	// PTO
	ptoTimer      *time.Timer
	ptoRetryCount int
	MaxPtoRetries int

	// Callbacks
	OnProbe          func()
	OnFastRetransmit func(seq int)

	// Pacer
	Pacer *PacingController

	// For recovery: need to know last sent sequence
	lastSentSeqFn func() int
}

// NewCongestionController creates a new congestion controller.
func NewCongestionController(clock Clock, lastSentSeqFn func() int) *CongestionController {
	cc := &CongestionController{
		clock:                clock,
		cwnd:                 InitialCwnd,
		ssthresh:             65535,
		smoothedRtt:          100 * time.Millisecond,
		rttVar:               50 * time.Millisecond,
		minRtt:               time.Second,
		firstRttSample:       true,
		lastAckedForDupCount: -1,
		highestCumulativeAck: -1,
		recoveryEndSeq:       -1,
		lostPacketInRecovery: -1,
		MaxPtoRetries:        10,
		Pacer:                NewPacingController(clock),
		lastSentSeqFn:        lastSentSeqFn,
	}
	return cc
}

// Cwnd returns the current congestion window.
func (cc *CongestionController) Cwnd() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.cwnd
}

// Ssthresh returns the slow start threshold.
func (cc *CongestionController) Ssthresh() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.ssthresh
}

// SmoothedRtt returns the smoothed RTT estimate.
func (cc *CongestionController) SmoothedRtt() time.Duration {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.smoothedRtt
}

// RttVar returns the RTT variation.
func (cc *CongestionController) RttVar() time.Duration {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.rttVar
}

// MinRtt returns the minimum observed RTT.
func (cc *CongestionController) MinRtt() time.Duration {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.minRtt
}

// Inflight returns bytes currently in flight.
func (cc *CongestionController) Inflight() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.inflight
}

// InRecovery returns whether the controller is in fast recovery.
func (cc *CongestionController) InRecovery() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.inRecovery
}

// PTO returns the current Probe Timeout duration.
// PTO = smoothed_rtt + max(4 * rttvar, 1ms) + max_ack_delay
func (cc *CongestionController) PTO() time.Duration {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.pto()
}

func (cc *CongestionController) pto() time.Duration {
	ptoMs := cc.smoothedRtt.Milliseconds() + 4*cc.rttVar.Milliseconds()
	if ptoMs < 200 {
		ptoMs = 200
	}
	if ptoMs > 5000 {
		ptoMs = 5000
	}
	return time.Duration(ptoMs) * time.Millisecond
}

// CanSend returns true if we can send the given number of bytes.
func (cc *CongestionController) CanSend(bytes int) bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.inflight+bytes <= cc.cwnd
}

// OnPacketSent is called when a packet is sent.
func (cc *CongestionController) OnPacketSent(bytes int) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.inflight += bytes
	cc.armPtoTimer()
}

// OnPacketAcked is called when a packet is acknowledged.
func (cc *CongestionController) OnPacketAcked(bytes int, sentTime time.Time, ackDelay time.Duration, isNewCumulativeAck bool, largestAcked int) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cc.inflight -= bytes
	if cc.inflight < 0 {
		cc.inflight = 0
	}

	if isNewCumulativeAck {
		cc.updateRtt(sentTime, ackDelay)
		cc.highestCumulativeAck = largestAcked
		cc.dupAcks = 0
		cc.lastAckedForDupCount = -1

		if cc.inRecovery {
			if largestAcked > cc.recoveryEndSeq {
				cc.inRecovery = false
			}
		}

		if !cc.inRecovery {
			if cc.cwnd < cc.ssthresh {
				// Slow start
				cc.cwnd += bytes
			} else {
				// CUBIC congestion avoidance
				cc.cubicUpdate(bytes)
			}
		}
	}

	// Reset PTO on ACK
	if cc.ptoTimer != nil {
		cc.ptoTimer.Stop()
		cc.ptoTimer = nil
	}
	cc.ptoRetryCount = 0

	if cc.inflight > 0 {
		cc.armPtoTimer()
	}
}

// updateRtt updates RTT estimates based on a new sample.
func (cc *CongestionController) updateRtt(sentTime time.Time, ackDelay time.Duration) {
	now := cc.clock.Now()

	// Cap ack_delay per RFC 9002
	cappedDelay := ackDelay
	if cappedDelay > MaxAckDelay {
		cappedDelay = MaxAckDelay
	}

	cc.latestRtt = now.Sub(sentTime)
	if cc.latestRtt > cappedDelay {
		cc.latestRtt -= cappedDelay
	}

	// Update min RTT
	if cc.latestRtt < cc.minRtt {
		cc.minRtt = cc.latestRtt
		cc.Pacer.UpdateRate(cc.cwnd, cc.minRtt)
	}

	// RFC 9002 Section 5.1
	if cc.firstRttSample {
		cc.firstRttSample = false
		cc.smoothedRtt = cc.latestRtt
		cc.rttVar = cc.latestRtt / 2
	} else {
		rttVarSample := cc.smoothedRtt - cc.latestRtt
		if rttVarSample < 0 {
			rttVarSample = -rttVarSample
		}
		// beta = 1/4, alpha = 1/8
		cc.rttVar = (3*cc.rttVar + rttVarSample) / 4
		cc.smoothedRtt = (7*cc.smoothedRtt + cc.latestRtt) / 8
	}
}

// cubicUpdate performs CUBIC window increase.
func (cc *CongestionController) cubicUpdate(bytes int) {
	if cc.epochStart.IsZero() {
		cc.epochStart = cc.clock.Now()
	}

	t := cc.clock.Now().Sub(cc.epochStart).Seconds()

	// CUBIC: W(t) = C*(t-K)^3 + wMax/MSS, result in MSS units
	wCubic := CubicC*math.Pow(t-cc.k, 3) + float64(cc.wMax)/float64(MaxDatagramSize)
	targetCwndBytes := int(wCubic * float64(MaxDatagramSize))

	// TCP-friendly estimate
	wTcp := cc.cwnd + (MaxDatagramSize*bytes)/cc.cwnd

	if targetCwndBytes < wTcp {
		cc.cwnd = wTcp
	} else if cc.cwnd < targetCwndBytes {
		increase := (targetCwndBytes - cc.cwnd) * MaxDatagramSize / cc.cwnd
		cc.cwnd += increase
	} else {
		cc.cwnd += (MaxDatagramSize * bytes) / cc.cwnd
	}
}

// OnPacketLost is called when packets are considered lost.
func (cc *CongestionController) OnPacketLost(bytes int) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cc.inflight -= bytes
	if cc.inflight < 0 {
		cc.inflight = 0
	}

	if cc.inRecovery {
		return
	}

	cc.epochStart = cc.clock.Now()
	cc.inRecovery = true
	if cc.lastSentSeqFn != nil {
		cc.recoveryEndSeq = cc.lastSentSeqFn()
	}

	cc.wMax = cc.cwnd
	cc.ssthresh = int(float64(cc.wMax) * BetaCubic)
	if cc.ssthresh < MinCwnd {
		cc.ssthresh = MinCwnd
	}

	// Pre-calculate K
	wMaxMSS := float64(cc.wMax) / float64(MaxDatagramSize)
	diff := wMaxMSS * (1 - BetaCubic) / CubicC
	cc.k = math.Cbrt(diff)

	cc.cwnd = cc.ssthresh
	cc.dupAcks = 0
	cc.Pacer.UpdateRate(cc.cwnd, cc.minRtt)
}

// ProcessDuplicateAck handles duplicate ACK processing for fast retransmit.
func (cc *CongestionController) ProcessDuplicateAck(frameLargestAcked int) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if frameLargestAcked == cc.lastAckedForDupCount {
		cc.dupAcks++
	} else {
		cc.dupAcks = 1
		cc.lastAckedForDupCount = frameLargestAcked
	}

	if cc.dupAcks >= 3 {
		if !cc.inRecovery {
			cc.inRecovery = true
			cc.lostPacketInRecovery = cc.lastAckedForDupCount + 1
			if cc.lastSentSeqFn != nil {
				cc.recoveryEndSeq = cc.lastSentSeqFn()
			}

			cc.ssthresh = cc.cwnd / 2
			if cc.ssthresh < MinCwnd {
				cc.ssthresh = MinCwnd
			}
			cc.cwnd = cc.ssthresh
			cc.Pacer.UpdateRate(cc.cwnd, cc.minRtt)

			if cc.OnFastRetransmit != nil {
				cc.OnFastRetransmit(cc.lostPacketInRecovery)
			}
		}
	}
}

// SetSmoothedRtt sets the smoothed RTT (for testing).
func (cc *CongestionController) SetSmoothedRtt(d time.Duration) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.smoothedRtt = d
}

// SetRttVar sets the RTT variation (for testing).
func (cc *CongestionController) SetRttVar(d time.Duration) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.rttVar = d
}

func (cc *CongestionController) armPtoTimer() {
	if cc.ptoTimer != nil {
		cc.ptoTimer.Stop()
	}
	cc.ptoTimer = cc.clock.AfterFunc(cc.pto(), cc.onPtoTimeout)
}

func (cc *CongestionController) onPtoTimeout() {
	cc.mu.Lock()
	cc.ptoRetryCount++
	count := cc.ptoRetryCount
	cc.mu.Unlock()

	if count <= cc.MaxPtoRetries {
		if cc.OnProbe != nil {
			cc.OnProbe()
		}
		cc.mu.Lock()
		nextPto := cc.pto() * 2
		cc.ptoTimer = cc.clock.AfterFunc(nextPto, cc.onPtoTimeout)
		cc.mu.Unlock()
	} else {
		cc.mu.Lock()
		cc.ptoRetryCount = 0
		backoff := cc.pto() * 4
		cc.ptoTimer = cc.clock.AfterFunc(backoff, cc.onPtoTimeout)
		cc.mu.Unlock()
	}
}

// Destroy cancels all timers.
func (cc *CongestionController) Destroy() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.ptoTimer != nil {
		cc.ptoTimer.Stop()
		cc.ptoTimer = nil
	}
}
