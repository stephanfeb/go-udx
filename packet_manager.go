package udx

import (
	"sync"
	"time"
)

// SentPacket tracks a packet that was sent and awaits acknowledgment.
type SentPacket struct {
	Sequence            uint32
	SentTime            time.Time
	Size                int
	IsAcked             bool
	Frames              []Frame
	DestinationStreamID uint32
	SourceStreamID      uint32
	RetransmitCount     int
	LastRetransmit      time.Time
}

// PacketManager tracks sent packets, handles ACK processing, and schedules retransmissions.
type PacketManager struct {
	clock Clock
	mu    sync.Mutex

	// Sequence tracking
	nextSeq          uint32
	lastSentSeq      int // -1 if nothing sent

	// Sent packets awaiting ACK, keyed by their CURRENT sequence number. A
	// retransmission re-keys its packet under a fresh sequence (see Retransmit),
	// so an entry moves from one number to the next over the packet's life.
	sentPackets map[uint32]*SentPacket

	// Retransmission timers, keyed the same way as sentPackets and re-keyed
	// alongside them: one live timer per unacked packet, following it across
	// renumberings.
	retransmitTimers map[uint32]*time.Timer

	// Congestion controller reference (for RTO calculation)
	cc *CongestionController

	// OnRetransmit puts a packet back on the wire. By the time it is called the
	// packet's Sequence has already been advanced to a fresh number, so the
	// callback only marshals and sends.
	OnRetransmit func(pkt *SentPacket)
}

// NewPacketManager creates a new packet manager.
func NewPacketManager(clock Clock, cc *CongestionController) *PacketManager {
	return &PacketManager{
		clock:              clock,
		nextSeq:          0,
		lastSentSeq:      -1,
		sentPackets:      make(map[uint32]*SentPacket),
		retransmitTimers: make(map[uint32]*time.Timer),
		cc:               cc,
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

// retransmitBackoff returns how long to wait before the given attempt number
// (1-based), backing off exponentially from the current RTO up to a ceiling.
//
// The ceiling never falls below the RTO itself. Capping below the round trip
// would retransmit before the peer could possibly have answered, turning a slow
// path into a flood of duplicates — so on a high-RTT link the RTO wins and the
// backoff simply stops growing.
func (pm *PacketManager) retransmitBackoff(attempt int) time.Duration {
	rto := pm.retransmitTimeout()

	ceiling := MaxRetransmitBackoff
	if ceiling < rto {
		ceiling = rto
	}

	backoff := ceiling
	if shift := attempt - 1; shift < 32 {
		if scaled := rto * time.Duration(1<<shift); scaled < ceiling {
			backoff = scaled
		}
	}
	if backoff < MinRetransmitTimeout {
		backoff = MinRetransmitTimeout
	}
	return backoff
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
// HandleAckFrame processes an ACK and returns the packets it newly acknowledged.
//
// It returns the *SentPacket values rather than bare sequence numbers because
// the entries are deleted here: callers previously took the sequences and then
// asked GetPacket for the size and send time, which always came back nil. That
// silently starved the congestion controller of every ACK it should have seen —
// inflight only ever grew, cwnd never left its initial value, and no RTT sample
// was ever taken.
func (pm *PacketManager) HandleAckFrame(frame *AckFrame) []*SentPacket {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var acked []*SentPacket

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
				acked = append(acked, pkt)
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
				acked = append(acked, pkt)
			}
		}
		// Advance past the range just acknowledged. The next unacknowledged
		// sequence is rangeEnd - AckRangeLength: rangeEnd is the highest seq in
		// the range and AckRangeLength of them were acked going down.
		//
		// This was rangeEnd - AckRangeLength - 1, one too low, which shifted
		// every range after the first down by one. The sender then deleted
		// sequences the peer had never acknowledged — so those packets were
		// never retransmitted and the receiver waited for them forever, while
		// sequences that really were acked stayed tracked. It only bites with
		// two or more SACK ranges, i.e. multiple distinct losses in flight,
		// which is why it showed up as loss-rate-dependent stalling rather than
		// an outright failure. DetectLostPackets walks the same structure and
		// has always done this correctly.
		currentSeq = rangeEnd - int64(r.AckRangeLength)
	}

	return acked
}

// Retransmit prepares an unacked packet to be re-sent under a FRESH sequence
// number, returning the new sequence and true when the caller should send it.
//
// Reusing the original number is what made loss fatal. The retry budget rode on
// the packet, so exhausting it left a hole the stream could never fill and the
// stream was reset (ErrStreamReset). QUIC never reuses a packet number
// (RFC 9000 section 12.3): lost data is re-framed into a new packet, and
// delivery is bounded by the connection's idle timeout rather than a per-packet
// count. Both v3 stacks reassemble streams on byte offsets, not sequence order,
// so a fresh number is safe here — a duplicate is discarded by its offset, and
// the retransmission is acknowledged unambiguously, so no Karn ambiguity
// poisons the RTT estimate.
//
// The re-key moves the tracking entry and the retransmit timer from the old
// number to the new, bumps the attempt count, and — because a fresh sequence is
// a fresh transmission — resets SentTime, so both the RTT sample and the loss
// timer measure from now.
//
// It touches neither inflight nor the congestion window: the bytes stay in
// flight across the re-key. Congestion accounting is the caller's, and differs
// by trigger — a timer-driven resend is a probe and changes nothing, while a
// loss-detected resend pairs with OnCongestionEvent.
//
// Returns (0, false) when the packet was already acknowledged, or was
// retransmitted within the last RTO — collapsing a near-simultaneous RTO timer
// and SACK-driven retransmit into a single send.
func (pm *PacketManager) Retransmit(pkt *SentPacket) (uint32, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	oldSeq := pkt.Sequence
	if _, ok := pm.sentPackets[oldSeq]; !ok {
		// Already acknowledged and removed; drop any lingering timer.
		if t, ok := pm.retransmitTimers[oldSeq]; ok {
			t.Stop()
			delete(pm.retransmitTimers, oldSeq)
		}
		return 0, false
	}

	now := pm.clock.Now()
	if pkt.RetransmitCount > 0 && now.Sub(pkt.LastRetransmit) < pm.retransmitTimeout() {
		// Another trigger re-sent this within the last RTO; one send is enough.
		return 0, false
	}

	newSeq := pm.nextSeq
	pm.nextSeq++

	if t, ok := pm.retransmitTimers[oldSeq]; ok {
		t.Stop()
		delete(pm.retransmitTimers, oldSeq)
	}
	delete(pm.sentPackets, oldSeq)

	pkt.Sequence = newSeq
	pkt.RetransmitCount++
	pkt.LastRetransmit = now
	pkt.SentTime = now
	pm.sentPackets[newSeq] = pkt

	backoff := pm.retransmitBackoff(pkt.RetransmitCount)
	pm.retransmitTimers[newSeq] = pm.clock.AfterFunc(backoff, func() { pm.onRetransmitTimer(pkt) })

	return newSeq, true
}

// GetPacket returns a sent packet by sequence number.
func (pm *PacketManager) GetPacket(seq uint32) *SentPacket {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.sentPackets[seq]
}

// lossDelay returns how long a packet must have been outstanding before age
// alone marks it lost: 9/8 of the larger of the smoothed and latest RTT, floored
// at the timer granularity (RFC 9002 section 6.1.2).
//
// The larger of the two RTTs matters. After a sudden increase in delay the
// smoothed estimate lags behind reality, and using it alone would declare a
// whole window of packets lost for the crime of being slower than they used to
// be — precisely when the path can least afford the duplicates.
func (pm *PacketManager) lossDelay() time.Duration {
	rtt := pm.cc.SmoothedRtt()
	if latest := pm.cc.LatestRtt(); latest > rtt {
		rtt = latest
	}

	delay := rtt * LossTimeThresholdNumerator / LossTimeThresholdDenominator
	if delay < LossTimerGranularity {
		delay = LossTimerGranularity
	}
	return delay
}

// DetectLostPackets examines SACK gaps in an AckFrame and returns sequence
// numbers of packets that are inferred lost.
//
// A gap is suspicion, not proof: UDP reorders, and a packet that arrives late
// still arrives. Following RFC 9002 section 6.1, a gap only counts as loss once
// the packet is either LossReorderThreshold sequence numbers behind the largest
// acknowledged one, or older than lossDelay. Until then it is presumed still in
// flight. This used to declare every gap lost on sight, which is why reordering
// cost far more than loss did.
//
// Packets retransmitted within the last RTO are skipped regardless, so a run of
// ACKs describing the same gap cannot re-send the same packet repeatedly.
func (pm *PacketManager) DetectLostPackets(frame *AckFrame) []uint32 {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if len(frame.AckRanges) == 0 {
		return nil
	}

	rto := pm.retransmitTimeout()
	lossDelay := pm.lossDelay()
	now := pm.clock.Now()
	var lost []uint32

	// Walk through the gaps between ACK ranges.
	// After the first range, cursor points to the first sequence below it.
	cursor := int64(frame.LargestAcked) - int64(frame.FirstAckRangeLength)

	for _, r := range frame.AckRanges {
		// The gap contains sequences that were NOT acknowledged.
		for g := uint8(0); g < r.Gap; g++ {
			if cursor < 0 {
				break
			}
			seq := uint32(cursor)
			if pkt, ok := pm.sentPackets[seq]; ok {
				// Skip if already retransmitted recently.
				if pkt.RetransmitCount > 0 && now.Sub(pkt.LastRetransmit) < rto {
					cursor--
					continue
				}
				// Neither far enough behind nor old enough: reordering still
				// explains the gap, so leave it in flight. The RTO timer is the
				// backstop if no further ACK ever resolves it.
				behind := int64(frame.LargestAcked) - cursor
				if behind < LossReorderThreshold && now.Sub(pkt.SentTime) < lossDelay {
					cursor--
					continue
				}
				lost = append(lost, seq)
			}
			cursor--
		}
		// Skip over the acked range.
		cursor -= int64(r.AckRangeLength)
	}

	return lost
}

// PendingCount returns the number of unacked sent packets.
func (pm *PacketManager) PendingCount() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return len(pm.sentPackets)
}

// scheduleRetransmission arms the first RTO timer for a freshly-sent packet.
// Every subsequent timer is armed by Retransmit as it re-keys the packet, so
// there is exactly one live timer per unacked packet, following it across
// renumberings. There is no per-packet retry cap: a packet is re-sent under a
// fresh sequence until it is acknowledged or the connection's idle timeout
// closes a path that has gone silent.
func (pm *PacketManager) scheduleRetransmission(pkt *SentPacket) {
	rto := pm.retransmitTimeout()
	pm.mu.Lock()
	pm.retransmitTimers[pkt.Sequence] = pm.clock.AfterFunc(rto, func() { pm.onRetransmitTimer(pkt) })
	pm.mu.Unlock()
}

// onRetransmitTimer fires when a packet's RTO elapses without an ACK. It
// re-keys the packet under a fresh sequence (which also arms the next timer)
// and puts it back on the wire. A timer-driven resend is a probe, so it leaves
// the congestion window alone — only ACK-based loss detection contracts it.
func (pm *PacketManager) onRetransmitTimer(pkt *SentPacket) {
	if _, ok := pm.Retransmit(pkt); ok {
		if pm.OnRetransmit != nil {
			pm.OnRetransmit(pkt)
		}
	}
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
