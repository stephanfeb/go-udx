package udx

import (
	"sort"
	"sync"
	"time"
)

// SentPacket tracks a packet that was sent and awaits acknowledgment.
type SentPacket struct {
	// Sequence is the number the packet was LAST sent under. A re-send takes a
	// fresh one (Resend), so an entry moves from number to number over the
	// packet's life.
	Sequence uint32
	// SentTime is when the packet last went on the wire; FirstSentTime when it
	// first did. The RTT sample and loss detection measure from the last send
	// (a fresh sequence makes its ACK unambiguous); persistent congestion
	// measures how long the data has gone undelivered, from the first.
	SentTime            time.Time
	FirstSentTime       time.Time
	Size                int
	IsAcked             bool
	Frames              []Frame
	DestinationStreamID uint32
	SourceStreamID      uint32
	RetransmitCount     int
	LastRetransmit      time.Time
}

// PacketManager tracks sent packets, matches ACKs to them, and declares loss
// the way RFC 9002 section 6.1 does: from acknowledgements, never from a
// per-packet timer.
//
// A packet is lost once a later one has been acknowledged and it is either
// LossReorderThreshold sequences behind the largest acknowledged or older than
// lossDelay (9/8 of the RTT). Packets behind but not yet old enough wait on the
// loss timer, which declares them lost when the delay elapses without another
// ACK. What is lost is handed to OnPacketsLost; the connection contracts its
// window once per recovery epoch and re-sends the data under fresh sequences
// through the congestion window and the pacer.
//
// This replaced one retransmission timer per packet, armed at the RTO and
// backing off to a 2 s cap. On a radio gap every timer fired together and the
// whole window went out again at once, past the congestion window, and again
// every 2 s into a dead path. The timer-driven resends did not contract the
// window either, so after the gap the sender resumed at the pre-gap window.
// QUIC's one PTO per connection (CongestionController) sends a probe or two
// instead, backing off until the idle timeout, and loss is declared only once
// an ACK proves it.
type PacketManager struct {
	clock Clock
	mu    sync.Mutex

	// Sequence tracking
	nextSeq     uint32
	lastSentSeq int // -1 if nothing sent

	// Sent packets awaiting ACK, keyed by their CURRENT sequence number.
	sentPackets map[uint32]*SentPacket

	// largestAcked is the largest sequence the peer has acknowledged, -1 if
	// none; lastAckTime when the last ACK that acknowledged something
	// arrived, and ackBeforeLoss the one before the ACK (or timer) that last
	// declared loss: persistent congestion asks whether anything was
	// acknowledged since the oldest lost packet went out, which the ACK that
	// revealed the loss does not answer.
	largestAcked  int64
	lastAckTime   time.Time
	ackBeforeLoss time.Time

	// lossTimer fires when the oldest packet behind largestAcked becomes old
	// enough to be lost without another ACK (the time threshold).
	lossTimer *time.Timer

	// Congestion controller reference (for the loss delay)
	cc *CongestionController

	// OnRetransmit puts a packet back on the wire. By the time it is called
	// the packet's Sequence has already been advanced to a fresh number, so
	// the callback only marshals and sends.
	OnRetransmit func(pkt *SentPacket, seq uint32)

	// OnPacketsLost is told what loss detection declared lost, in sequence
	// order, outside the manager's lock. The packets are no longer tracked:
	// the receiver of this callback re-sends them (Resend) and books them.
	OnPacketsLost func(lost []*SentPacket)
}

// NewPacketManager creates a new packet manager.
func NewPacketManager(clock Clock, cc *CongestionController) *PacketManager {
	return &PacketManager{
		clock:        clock,
		nextSeq:      0,
		lastSentSeq:  -1,
		sentPackets:  make(map[uint32]*SentPacket),
		largestAcked: -1,
		cc:           cc,
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

// LargestAcked is the largest sequence the peer has acknowledged, -1 if none.
func (pm *PacketManager) LargestAcked() int64 {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.largestAcked
}

// SendPacket registers a sent packet for tracking.
func (pm *PacketManager) SendPacket(pkt *SentPacket) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	now := pm.clock.Now()
	pkt.SentTime = now
	if pkt.FirstSentTime.IsZero() {
		pkt.FirstSentTime = now
	}
	pm.lastSentSeq = int(pkt.Sequence)
	pm.sentPackets[pkt.Sequence] = pkt
}

// HandleAckFrame processes an ACK and returns the packets it newly
// acknowledged. The caller books them with the congestion controller and
// then runs DetectLostPackets, in that order (RFC 9002 appendix A.7: the
// acknowledged bytes grow the window before the loss contracts it).
//
// It returns the *SentPacket values rather than bare sequence numbers because
// the entries are deleted here: the caller needs their size and send time.
func (pm *PacketManager) HandleAckFrame(frame *AckFrame) []*SentPacket {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	var acked []*SentPacket
	take := func(seq uint32) {
		if pkt, ok := pm.sentPackets[seq]; ok {
			pkt.IsAcked = true
			delete(pm.sentPackets, seq)
			acked = append(acked, pkt)
		}
	}

	// First range: largestAcked down to largestAcked - firstAckRangeLength + 1
	for i := uint32(0); i < frame.FirstAckRangeLength; i++ {
		take(frame.LargestAcked - i)
	}

	// Additional ranges, walking down. The next unacknowledged sequence after
	// a range is rangeEnd - AckRangeLength: rangeEnd is the highest in the
	// range and AckRangeLength of them were acked going down.
	currentSeq := int64(frame.LargestAcked) - int64(frame.FirstAckRangeLength)
	for _, r := range frame.AckRanges {
		rangeEnd := currentSeq - int64(r.Gap)
		for i := uint32(0); i < r.AckRangeLength; i++ {
			take(uint32(rangeEnd - int64(i)))
		}
		currentSeq = rangeEnd - int64(r.AckRangeLength)
	}

	if len(acked) > 0 {
		pm.ackBeforeLoss = pm.lastAckTime
		pm.lastAckTime = pm.clock.Now()
		if int64(frame.LargestAcked) > pm.largestAcked {
			pm.largestAcked = int64(frame.LargestAcked)
		}
	}
	return acked
}

// DetectLostPackets runs loss detection over everything outstanding
// (RFC 9002 section 6.1) and returns what it declared lost, also handed to
// OnPacketsLost. The connection calls it after each ACK that acknowledged
// something; the loss timer calls it when the time threshold elapses.
func (pm *PacketManager) DetectLostPackets() []*SentPacket {
	pm.mu.Lock()
	lost := pm.detectLostLocked(pm.clock.Now())
	pm.mu.Unlock()
	if len(lost) > 0 && pm.OnPacketsLost != nil {
		pm.OnPacketsLost(lost)
	}
	return lost
}

// detectLostLocked declares lost every outstanding packet below the largest
// acknowledged that is LossReorderThreshold or more behind it, or older than
// lossDelay (RFC 9002 section 6.1.1 and 6.1.2), removes them from tracking
// and returns them in sequence order. Packets behind the largest acknowledged
// but not yet old enough arm the loss timer for when the oldest will be.
//
// Every outstanding packet below the largest acknowledged is considered, not
// only those in the frame's gaps: a receiver that acknowledges one packet at
// a time sends no gaps, and the time threshold is then what declares loss.
func (pm *PacketManager) detectLostLocked(now time.Time) []*SentPacket {
	if pm.lossTimer != nil {
		pm.lossTimer.Stop()
		pm.lossTimer = nil
	}
	if pm.largestAcked < 0 {
		return nil
	}
	delay := pm.lossDelay()
	var lost []*SentPacket
	var nextLoss time.Time
	for seq, pkt := range pm.sentPackets {
		if int64(seq) > pm.largestAcked {
			continue
		}
		behind := pm.largestAcked - int64(seq)
		if behind >= LossReorderThreshold || !now.Before(pkt.SentTime.Add(delay)) {
			lost = append(lost, pkt)
			continue
		}
		at := pkt.SentTime.Add(delay)
		if nextLoss.IsZero() || at.Before(nextLoss) {
			nextLoss = at
		}
	}
	for _, pkt := range lost {
		delete(pm.sentPackets, pkt.Sequence)
	}
	sort.Slice(lost, func(i, j int) bool { return lost[i].Sequence < lost[j].Sequence })
	if !nextLoss.IsZero() {
		wait := nextLoss.Sub(now)
		if wait < LossTimerGranularity {
			wait = LossTimerGranularity
		}
		pm.lossTimer = pm.clock.AfterFunc(wait, pm.onLossTimer)
	}
	return lost
}

// onLossTimer declares lost what has become old enough since the last ACK.
func (pm *PacketManager) onLossTimer() {
	pm.mu.Lock()
	pm.ackBeforeLoss = pm.lastAckTime
	lost := pm.detectLostLocked(pm.clock.Now())
	pm.mu.Unlock()
	if len(lost) > 0 && pm.OnPacketsLost != nil {
		pm.OnPacketsLost(lost)
	}
}

// LossTimerArmed reports whether a packet is waiting on the time threshold.
func (pm *PacketManager) LossTimerArmed() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.lossTimer != nil
}

// AckBeforeLoss is when the last ACK before the one that revealed the
// current loss arrived (zero if none): whether anything was acknowledged
// since a lost packet went out is what persistent congestion asks.
func (pm *PacketManager) AckBeforeLoss() time.Time {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.ackBeforeLoss
}

// Resend gives a packet a FRESH sequence number for another transmission and
// tracks it under that number, returning the number and true, or false when
// the packet has been acknowledged and must not go out again.
//
// Reusing the original number is what made loss fatal: the retry budget rode
// on the packet, so exhausting it left a hole the stream could never fill.
// QUIC never reuses a packet number (RFC 9000 section 12.3): lost data is
// re-framed into a new packet. Both v3 stacks reassemble streams on byte
// offsets, so a fresh number is safe: a duplicate is discarded by its offset,
// and the retransmission is acknowledged unambiguously.
//
// The packet may be tracked (a PTO probe re-sends the oldest outstanding
// packet, which stays outstanding) or not (loss detection removed it): either
// way it is tracked under the new number afterwards, with SentTime reset so
// the RTT sample and loss detection measure from this send. Congestion
// accounting is the caller's: a probe's bytes are still in flight, a lost
// packet's left flight when it was declared lost and are booked again by the
// caller when it goes out.
func (pm *PacketManager) Resend(pkt *SentPacket) (uint32, bool) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if pkt.IsAcked {
		return 0, false
	}
	if cur, ok := pm.sentPackets[pkt.Sequence]; ok && cur == pkt {
		delete(pm.sentPackets, pkt.Sequence)
	}
	now := pm.clock.Now()
	newSeq := pm.nextSeq
	pm.nextSeq++
	pkt.Sequence = newSeq
	pkt.RetransmitCount++
	pkt.LastRetransmit = now
	pkt.SentTime = now
	if pkt.FirstSentTime.IsZero() {
		pkt.FirstSentTime = now
	}
	pm.lastSentSeq = int(newSeq)
	pm.sentPackets[newSeq] = pkt
	return newSeq, true
}

// OldestUnacked returns up to n outstanding packets, lowest sequence first:
// what a PTO probe re-sends (RFC 9002 section 6.2.4). A re-send takes a fresh
// sequence, so the lowest numbers are the longest unsent.
func (pm *PacketManager) OldestUnacked(n int) []*SentPacket {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if len(pm.sentPackets) == 0 || n <= 0 {
		return nil
	}
	all := make([]*SentPacket, 0, len(pm.sentPackets))
	for _, pkt := range pm.sentPackets {
		all = append(all, pkt)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Sequence < all[j].Sequence })
	if len(all) > n {
		all = all[:n]
	}
	return all
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

// PendingCount returns the number of unacked sent packets.
func (pm *PacketManager) PendingCount() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return len(pm.sentPackets)
}

// Destroy stops the loss timer and forgets every packet.
func (pm *PacketManager) Destroy() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.lossTimer != nil {
		pm.lossTimer.Stop()
		pm.lossTimer = nil
	}
	pm.sentPackets = make(map[uint32]*SentPacket)
}
