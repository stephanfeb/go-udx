package udx

import (
	"math"
	"sync"
	"time"
)

// PacingController spreads a connection's sends over the round trip instead
// of letting a window's worth go out at once (RFC 9002 section 7.7).
//
// The rate is gain * cwnd / smoothed_rtt, the gain PacingGainSlowStart in
// slow start and PacingGain after it, as most QUIC stacks pace. The
// congestion controller recomputes it on every change to the window or the
// RTT estimate (UpdateRate). It used to be computed from min_rtt, and only
// when min_rtt fell or the window contracted, so the rate stayed at what the
// initial window allowed however far the window grew: 4 MB over a 40 ms path
// took fourteen seconds.
//
// Sends draw on a budget that refills at the rate and holds at most a burst:
// PacingBurstPackets datagrams, or what MinPacingDelay of the rate is worth,
// whichever is more. A sender need not sleep between every two packets, and
// a timer that can only wait about a millisecond still reaches the rate. The
// budget may go negative: a batch that overshoots it is paid for by the wait
// after it. Until the controller has an RTT sample the rate is unlimited and
// the initial window is the burst.
type PacingController struct {
	clock Clock
	mu    sync.Mutex

	rate             float64 // bytes per second; +Inf is unpaced
	budgetAtLastSend float64 // bytes the budget held after the last send
	lastSendTime     time.Time
}

// NewPacingController creates a new pacer, unpaced until UpdateRate.
func NewPacingController(clock Clock) *PacingController {
	return &PacingController{
		clock: clock,
		rate:  math.Inf(1),
	}
}

// UpdateRate sets the rate from the congestion window and the smoothed RTT:
// gain * cwnd / srtt, the gain depending on whether the window is in slow
// start. A zero srtt (loopback, or a test clock that does not move) means
// unpaced.
func (p *PacingController) UpdateRate(cwnd int, srtt time.Duration, slowStart bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if srtt <= 0 {
		p.rate = math.Inf(1)
		return
	}
	gain := PacingGain
	if slowStart {
		gain = PacingGainSlowStart
	}
	p.rate = gain * float64(cwnd) / srtt.Seconds()
}

// maxBurst is the most the budget holds. Caller holds p.mu.
func (p *PacingController) maxBurst() float64 {
	burst := float64(PacingBurstPackets * MaxDatagramSize)
	if !math.IsInf(p.rate, 1) {
		if byDelay := p.rate * MinPacingDelay.Seconds(); byDelay > burst {
			burst = byDelay
		}
	}
	return burst
}

// budget is what may be sent at now without waiting. Caller holds p.mu and
// the rate is finite.
func (p *PacingController) budget(now time.Time) float64 {
	if p.lastSendTime.IsZero() {
		return p.maxBurst()
	}
	b := p.budgetAtLastSend + p.rate*now.Sub(p.lastSendTime).Seconds()
	if max := p.maxBurst(); b > max {
		b = max
	}
	return b
}

// TimeUntilSend returns how long to wait before the next send: zero when
// the budget holds a full datagram, otherwise the time it takes to, and at
// least MinPacingDelay.
func (p *PacingController) TimeUntilSend() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	if math.IsInf(p.rate, 1) {
		return 0
	}
	short := float64(MaxDatagramSize) - p.budget(p.clock.Now())
	if short <= 0 {
		return 0
	}
	wait := time.Duration(math.Ceil(short / p.rate * float64(time.Second)))
	if wait < MinPacingDelay {
		wait = MinPacingDelay
	}
	return wait
}

// OnPacketSent draws a sent packet from the budget.
func (p *PacingController) OnPacketSent(packetSize int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if math.IsInf(p.rate, 1) {
		return
	}
	now := p.clock.Now()
	p.budgetAtLastSend = p.budget(now) - float64(packetSize)
	p.lastSendTime = now
}

// PacingRate returns the current pacing rate in bytes/sec, +Inf when unpaced.
func (p *PacingController) PacingRate() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rate
}

// Budget is how many bytes may be sent now without waiting (negative after
// an overshoot), or +Inf when unpaced.
func (p *PacingController) Budget() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if math.IsInf(p.rate, 1) {
		return p.rate
	}
	return p.budget(p.clock.Now())
}
