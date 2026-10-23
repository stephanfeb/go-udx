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

	// Recovery state
	inRecovery     bool
	recoveryEndSeq int

	// PTO (RFC 9002 section 6.2): one timer per connection, armed from the
	// last ack-eliciting send, backing off by ptoCount until an ACK arrives.
	// There is no retry cap: a path that never answers is ended by the
	// connection's idle timeout, not by the probing stopping.
	ptoTimer     *time.Timer
	ptoCount     int
	lastSendTime time.Time

	// OnProbe is called when the PTO fires: the connection sends a probe or
	// two (the oldest outstanding data under fresh sequences), without
	// touching the window.
	OnProbe func()

	// Pacer
	Pacer *PacingController

	// For recovery: need to know last sent sequence
	lastSentSeqFn func() int
}

// NewCongestionController creates a new congestion controller.
func NewCongestionController(clock Clock, lastSentSeqFn func() int) *CongestionController {
	cc := &CongestionController{
		clock: clock,
		cwnd:  InitialCwnd,
		// Slow start runs until the first loss (RFC 9002 section 7.3.1).
		// This was 65535, so slow start ended at 64 KB, 44 packets, and
		// the window crawled up from there at CUBIC's pace: on a 40 ms
		// path 4 MB took two seconds with no loss at all.
		ssthresh:       math.MaxInt,
		smoothedRtt:    100 * time.Millisecond,
		rttVar:         50 * time.Millisecond,
		minRtt:         time.Second,
		firstRttSample: true,
		recoveryEndSeq: -1,
		Pacer:          NewPacingController(clock),
		lastSentSeqFn:  lastSentSeqFn,
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

// LatestRtt returns the most recent RTT sample. Loss detection needs it
// alongside the smoothed value: after a sudden increase in delay the smoothed
// estimate lags, and using it alone would declare packets lost that are simply
// taking longer than they used to (RFC 9002 section 6.1.2).
func (cc *CongestionController) LatestRtt() time.Duration {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.latestRtt
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

// PTO returns the base Probe Timeout (RFC 9002 section 6.2.1):
// smoothed_rtt + max(4 * rttvar, kGranularity) + max_ack_delay, within
// [MinPTO, MaxPTO]. The peer delays its ACKs by up to MaxAckDelay, so a PTO
// without that term fires on a delayed ACK alone.
func (cc *CongestionController) PTO() time.Duration {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.pto()
}

func (cc *CongestionController) pto() time.Duration {
	v := 4 * cc.rttVar
	if v < LossTimerGranularity {
		v = LossTimerGranularity
	}
	pto := cc.smoothedRtt + v + MaxAckDelay
	if pto < MinPTO {
		pto = MinPTO
	}
	if pto > MaxPTO {
		pto = MaxPTO
	}
	return pto
}

// PTOCount is how many PTOs have fired since the last ACK; the next PTO
// waits pto << PTOCount.
func (cc *CongestionController) PTOCount() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.ptoCount
}

// nextPTO is the wait for the next probe: the base PTO backed off by the
// number of PTOs that have fired without an ACK. Uncapped: the idle timeout
// ends the connection before this grows absurd.
func (cc *CongestionController) nextPTO() time.Duration {
	shift := cc.ptoCount
	if shift > 20 {
		shift = 20
	}
	return cc.pto() << uint(shift)
}

// CanSend returns true if we can send the given number of bytes.
func (cc *CongestionController) CanSend(bytes int) bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.inflight+bytes <= cc.cwnd
}

// Available is how many more bytes the window admits right now.
func (cc *CongestionController) Available() int {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.inflight >= cc.cwnd {
		return 0
	}
	return cc.cwnd - cc.inflight
}

// OnPacketSent is called when a packet is sent.
func (cc *CongestionController) OnPacketSent(bytes int) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.inflight += bytes
	cc.lastSendTime = cc.clock.Now()
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

	// An ACK is proof the path answers: the PTO backoff starts over
	// (RFC 9002 section 6.2.1), measured from the last send.
	if cc.ptoTimer != nil {
		cc.ptoTimer.Stop()
		cc.ptoTimer = nil
	}
	cc.ptoCount = 0

	if cc.inflight > 0 {
		cc.armPtoTimer()
	}
	cc.updatePacingRate()
}

// updatePacingRate tells the pacer the window and RTT it paces by, once
// an RTT sample exists; before one the pacer is unlimited and the initial
// window is the burst. Called after every change to either. Caller holds
// cc.mu.
func (cc *CongestionController) updatePacingRate() {
	if cc.firstRttSample {
		return
	}
	cc.Pacer.UpdateRate(cc.cwnd, cc.smoothedRtt, cc.cwnd < cc.ssthresh)
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

	if cc.latestRtt < cc.minRtt {
		cc.minRtt = cc.latestRtt
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

// OnPacketLost is called when a packet is declared lost: its bytes leave
// flight (RFC 9002 section 7.3) and the window contracts, once per recovery
// epoch. When the data is re-sent it is booked again by OnPacketSent, as the
// new packet it is.
func (cc *CongestionController) OnPacketLost(bytes int) {
	cc.mu.Lock()
	cc.inflight -= bytes
	if cc.inflight < 0 {
		cc.inflight = 0
	}
	cc.mu.Unlock()

	cc.OnCongestionEvent()
}

// OnPersistentCongestion is the RFC 9002 section 7.6 reaction: the window
// collapses to the minimum and recovery ends, so sending restarts in slow
// start, as after an outage long enough that everything sent in three PTOs
// was lost. Called after OnPacketLost for the same loss event.
func (cc *CongestionController) OnPersistentCongestion() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.cwnd = MinCwnd
	cc.inRecovery = false
	cc.recoveryEndSeq = -1
	cc.epochStart = time.Time{}
	cc.updatePacingRate()
}

// PersistentCongestionDuration is how long a span of loss must cover before
// it counts as persistent congestion (RFC 9002 section 7.6.1): three PTOs,
// with the max_ack_delay term, as the PTO here already includes it.
func (cc *CongestionController) PersistentCongestionDuration() time.Duration {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.pto() * PersistentCongestionThreshold
}

// HasRttSample reports whether any RTT sample has been taken; persistent
// congestion is only declared once one has (section 7.6.2).
func (cc *CongestionController) HasRttSample() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return !cc.firstRttSample
}

// OnCongestionEvent contracts the congestion window in response to detected
// loss, WITHOUT altering inflight. It is the reaction to loss that is being
// recovered by retransmission (RFC 9002 section 7): the lost bytes are re-sent,
// not abandoned, so they remain accounted in flight until the retransmission is
// acknowledged. Double-counting them out of inflight here would open the window
// spuriously.
//
// Idempotent within a recovery epoch (RFC 9002 section 7.3.1): the first loss
// halves the window and the rest of the same round-trip's losses are absorbed,
// so calling it once per lost packet still reduces the window only once.
func (cc *CongestionController) OnCongestionEvent() {
	cc.mu.Lock()
	defer cc.mu.Unlock()

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
	cc.updatePacingRate()
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

// armPtoTimer sets the PTO timer to fire nextPTO after the last
// ack-eliciting send (RFC 9002 section 6.2.1). Caller holds cc.mu.
func (cc *CongestionController) armPtoTimer() {
	if cc.ptoTimer != nil {
		cc.ptoTimer.Stop()
	}
	wait := cc.nextPTO()
	if !cc.lastSendTime.IsZero() {
		if remaining := cc.lastSendTime.Add(wait).Sub(cc.clock.Now()); remaining < wait {
			wait = remaining
		}
	}
	if wait < LossTimerGranularity {
		wait = LossTimerGranularity
	}
	cc.ptoTimer = cc.clock.AfterFunc(wait, cc.onPtoTimeout)
}

// onPtoTimeout fires when nothing has been acknowledged for a PTO: the
// connection sends a probe (OnProbe), and the next PTO waits twice as long.
// The window is untouched; loss is declared only once an ACK proves it.
func (cc *CongestionController) onPtoTimeout() {
	cc.mu.Lock()
	cc.ptoCount++
	cc.ptoTimer = nil
	cc.mu.Unlock()

	if cc.OnProbe != nil {
		cc.OnProbe()
	}

	cc.mu.Lock()
	if cc.inflight > 0 && cc.ptoTimer == nil {
		// Measured from now: the probe just sent is the last send.
		cc.lastSendTime = cc.clock.Now()
		cc.ptoTimer = cc.clock.AfterFunc(cc.nextPTO(), cc.onPtoTimeout)
	}
	cc.mu.Unlock()
}

// FirePTO runs the PTO expiry by hand, for tests on a clock whose timers do
// not fire.
func (cc *CongestionController) FirePTO() { cc.onPtoTimeout() }

// Destroy cancels all timers.
func (cc *CongestionController) Destroy() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.ptoTimer != nil {
		cc.ptoTimer.Stop()
		cc.ptoTimer = nil
	}
}
