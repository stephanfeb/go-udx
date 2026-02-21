package udx

import (
	"sync"
	"time"
)

// SentPacket tracks a packet that was sent and awaits acknowledgment.
type SentPacket struct {
	Sequence uint32
	SentTime time.Time
	Size     int
	IsAcked  bool
	Frames   []Frame
}

// PacketManager tracks sent packets, handles ACK processing, and schedules retransmissions.
type PacketManager struct {
	clock Clock
	mu    sync.Mutex

	// Sequence tracking
	nextSeq          uint32
	lastSentSeq      int // -1 if nothing sent

	// Sent packets awaiting ACK
	sentPackets map[uint32]*SentPacket

	// Retransmission timers
	retransmitTimers   map[uint32]*time.Timer
	retransmitAttempts map[uint32]int

	// Congestion controller reference (for RTO calculation)
	cc *CongestionController

	// Callbacks
	OnRetransmit          func(pkt *SentPacket)
	OnPacketPermanentLoss func(pkt *SentPacket)
}

// NewPacketManager creates a new packet manager.
func NewPacketManager(clock Clock, cc *CongestionController) *PacketManager {
	return &PacketManager{
		clock:              clock,
		nextSeq:            0,
		lastSentSeq:        -1,
		sentPackets:        make(map[uint32]*SentPacket),
		retransmitTimers:   make(map[uint32]*time.Timer),
		retransmitAttempts: make(map[uint32]int),
		cc:                 cc,
	}
}

// NextSequence returns the next sequence number and increments the counter.
func (pm *PacketManager) NextSequence() uint32 {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	seq := pm.nextSeq
	pm.nextSeq++
	return seq
}

// LastSentSeq returns the last sent sequence number (-1 if none sent).
func (pm *PacketManager) LastSentSeq() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.lastSentSeq
}

// retransmitTimeout returns the RTO in milliseconds based on congestion controller RTT.
func (pm *PacketManager) retransmitTimeout() time.Duration {
	sRtt := pm.cc.SmoothedRtt()
	rttVar := pm.cc.RttVar()

	rtoMs := sRtt.Milliseconds() + 4*rttVar.Milliseconds()
	if rtoMs < 200 {
		rtoMs = 200
	}
	if rtoMs > 5000 {
		rtoMs = 5000
	}
	return time.Duration(rtoMs) * time.Millisecond
}

// SendPacket registers a sent packet for tracking and retransmission.
func (pm *PacketManager) SendPacket(pkt *SentPacket) {
	pm.mu.Lock()
	pkt.SentTime = pm.clock.Now()
	pm.lastSentSeq = int(pkt.Sequence)
	pm.sentPackets[pkt.Sequence] = pkt
	pm.mu.Unlock()

	pm.scheduleRetransmission(pkt)
}

// HandleAckFrame processes an ACK frame and returns newly acknowledged sequence numbers.
func (pm *PacketManager) HandleAckFrame(frame *AckFrame) []uint32 {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var acked []uint32

	// Process first range: largestAcked down to largestAcked - firstAckRangeLength + 1
	if frame.FirstAckRangeLength > 0 {
		for i := uint32(0); i < frame.FirstAckRangeLength; i++ {
			seq := frame.LargestAcked - i
			if pkt, ok := pm.sentPackets[seq]; ok {
				pkt.IsAcked = true
				delete(pm.sentPackets, seq)
				if t, ok := pm.retransmitTimers[seq]; ok {
					t.Stop()
					delete(pm.retransmitTimers, seq)
				}
				delete(pm.retransmitAttempts, seq)
				acked = append(acked, seq)
			}
		}
	}

	// Process additional ACK ranges
	currentSeq := int64(frame.LargestAcked) - int64(frame.FirstAckRangeLength)
	for _, r := range frame.AckRanges {
		rangeEnd := currentSeq - int64(r.Gap)
		for i := uint32(0); i < r.AckRangeLength; i++ {
			seq := uint32(rangeEnd - int64(i))
			if pkt, ok := pm.sentPackets[seq]; ok {
				pkt.IsAcked = true
				delete(pm.sentPackets, seq)
				if t, ok := pm.retransmitTimers[seq]; ok {
					t.Stop()
					delete(pm.retransmitTimers, seq)
				}
				delete(pm.retransmitAttempts, seq)
				acked = append(acked, seq)
			}
		}
		currentSeq = rangeEnd - int64(r.AckRangeLength) - 1 + 1 - 1
	}

	return acked
}

// GetPacket returns a sent packet by sequence number.
func (pm *PacketManager) GetPacket(seq uint32) *SentPacket {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.sentPackets[seq]
}

// PendingCount returns the number of unacked sent packets.
func (pm *PacketManager) PendingCount() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return len(pm.sentPackets)
}

func (pm *PacketManager) scheduleRetransmission(pkt *SentPacket) {
	retryCount := 0
	seq := pkt.Sequence

	var retransmit func()
	retransmit = func() {
		pm.mu.Lock()
		if _, ok := pm.sentPackets[seq]; !ok {
			// Already ACKed
			if t, ok := pm.retransmitTimers[seq]; ok {
				t.Stop()
				delete(pm.retransmitTimers, seq)
			}
			pm.mu.Unlock()
			return
		}

		retryCount++
		if retryCount <= MaxRetransmitRetries {
			pm.retransmitAttempts[seq] = retryCount
			p := pm.sentPackets[seq]
			pm.mu.Unlock()

			if pm.OnRetransmit != nil {
				pm.OnRetransmit(p)
			}

			// Exponential backoff
			rto := pm.retransmitTimeout()
			backoff := rto * time.Duration(1<<(retryCount-1))
			if backoff < MinRetransmitTimeout {
				backoff = MinRetransmitTimeout
			}
			if backoff > MaxRetransmitTimeout {
				backoff = MaxRetransmitTimeout
			}

			pm.mu.Lock()
			pm.retransmitTimers[seq] = pm.clock.AfterFunc(backoff, retransmit)
			pm.mu.Unlock()
		} else {
			// Give up
			if t, ok := pm.retransmitTimers[seq]; ok {
				t.Stop()
				delete(pm.retransmitTimers, seq)
			}
			p := pm.sentPackets[seq]
			delete(pm.sentPackets, seq)
			delete(pm.retransmitAttempts, seq)
			pm.mu.Unlock()

			if pm.OnPacketPermanentLoss != nil && p != nil {
				pm.OnPacketPermanentLoss(p)
			}
		}
	}

	rto := pm.retransmitTimeout()
	pm.mu.Lock()
	pm.retransmitTimers[seq] = pm.clock.AfterFunc(rto, retransmit)
	pm.mu.Unlock()
}

// Destroy cancels all retransmission timers.
func (pm *PacketManager) Destroy() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for seq, t := range pm.retransmitTimers {
		t.Stop()
		delete(pm.retransmitTimers, seq)
	}
	pm.sentPackets = make(map[uint32]*SentPacket)
}
