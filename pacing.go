package udx

import (
	"math"
	"sync"
	"time"
)

// PacingController smooths packet sending to prevent bursts.
type PacingController struct {
	clock Clock
	mu    sync.Mutex

	pacingGain         float64
	pacingRateBytesPS  float64   // bytes per second
	nextSendTime       time.Time
}

// NewPacingController creates a new pacer.
func NewPacingController(clock Clock) *PacingController {
	return &PacingController{
		clock:             clock,
		pacingGain:        PacingGain,
		pacingRateBytesPS: math.Inf(1),
		nextSendTime:      clock.Now(),
	}
}

// UpdateRate recalculates the pacing rate from cwnd and minRtt.
func (p *PacingController) UpdateRate(cwnd int, minRtt time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if minRtt == 0 {
		p.pacingRateBytesPS = math.Inf(1)
		return
	}
	minRttSec := float64(minRtt.Microseconds()) / 1e6
	p.pacingRateBytesPS = (p.pacingGain * float64(cwnd)) / minRttSec
}

// TimeUntilSend returns how long to wait before the next send.
// Returns 0 if sending is allowed immediately.
func (p *PacingController) TimeUntilSend() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := p.clock.Now()
	if now.Before(p.nextSendTime) {
		return p.nextSendTime.Sub(now)
	}
	return 0
}

// OnPacketSent updates the next send time after sending a packet.
func (p *PacingController) OnPacketSent(packetSize int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !math.IsInf(p.pacingRateBytesPS, 1) {
		intervalUs := float64(packetSize) / p.pacingRateBytesPS * 1e6
		interval := time.Duration(intervalUs) * time.Microsecond

		now := p.clock.Now()
		if now.After(p.nextSendTime) {
			p.nextSendTime = now.Add(interval)
		} else {
			p.nextSendTime = p.nextSendTime.Add(interval)
		}
	}
}

// PacingRate returns the current pacing rate in bytes/sec.
func (p *PacingController) PacingRate() float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pacingRateBytesPS
}
