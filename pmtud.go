package udx

import "sync"

// PMTUDState represents the state of Path MTU Discovery.
type PMTUDState int

const (
	PMTUDInitial    PMTUDState = iota
	PMTUDSearching
	PMTUDValidated
)

// PMTUDController implements Datagram Packetization Layer PMTUD (RFC 8899).
type PMTUDController struct {
	mu sync.Mutex

	minMTU     int
	maxMTU     int
	currentMTU int
	probeMTU   int
	state      PMTUDState

	// Track in-flight probes: sequence -> probe size
	inFlightProbes map[uint32]int
}

// NewPMTUDController creates a new PMTUD controller.
func NewPMTUDController() *PMTUDController {
	return &PMTUDController{
		minMTU:         MinMTU,
		maxMTU:         MaxMTU,
		currentMTU:     MinMTU,
		state:          PMTUDInitial,
		inFlightProbes: make(map[uint32]int),
	}
}

// CurrentMTU returns the validated MTU.
func (p *PMTUDController) CurrentMTU() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentMTU
}

// State returns the current PMTUD state.
func (p *PMTUDController) State() PMTUDState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// ShouldSendProbe returns true if a probe should be sent.
func (p *PMTUDController) ShouldSendProbe() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == PMTUDSearching && len(p.inFlightProbes) == 0
}

// StartProbe initiates a probe and returns the probe size and an MTUProbeFrame.
// The headerSize is the packet header overhead to subtract from probe MTU.
func (p *PMTUDController) StartProbe(seq uint32, headerSize int) (probeSize int, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.state == PMTUDInitial {
		p.probeMTU = p.minMTU
		p.state = PMTUDSearching
	}

	frameSize := p.probeMTU - headerSize
	if frameSize <= 0 {
		return 0, false
	}

	p.inFlightProbes[seq] = p.probeMTU
	return frameSize, true
}

// OnProbeAcked handles a successful MTU probe acknowledgment.
func (p *PMTUDController) OnProbeAcked(seq uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	ackedSize, ok := p.inFlightProbes[seq]
	if !ok {
		return
	}
	delete(p.inFlightProbes, seq)

	p.currentMTU = ackedSize
	nextProbe := (p.currentMTU + p.maxMTU + 1) / 2

	if nextProbe > p.currentMTU {
		p.probeMTU = nextProbe
		p.state = PMTUDSearching
	} else {
		p.state = PMTUDValidated
	}
}

// OnProbeLost handles a lost MTU probe.
func (p *PMTUDController) OnProbeLost(seq uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()

	lostSize, ok := p.inFlightProbes[seq]
	if !ok {
		return
	}
	delete(p.inFlightProbes, seq)

	newMax := lostSize - 1
	p.probeMTU = (p.currentMTU + newMax) / 2

	if p.probeMTU <= p.currentMTU {
		p.state = PMTUDValidated
	} else {
		p.state = PMTUDSearching
	}
}
