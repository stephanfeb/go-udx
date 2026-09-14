package udx

// recvTracker records which data-bearing packets have arrived so an ACK can
// carry SACK ranges, and says whether each arrival is out of order.
//
// It replaces a map of every received sequence number that buildAckFrame
// walked in full on every ACK (to find the largest, to walk the ranges, and
// to prune). That walk was 5% of a bulk receiver's CPU. The tracker keeps the
// largest sequence explicitly and a bitmap of the ackHistory sequences below
// it, indexed by sequence modulo the history, so recording an arrival is a
// few bit operations and building the frame is one downward scan.
//
// Sequences more than ackHistory below the largest are forgotten. An ACK can
// carry at most five ranges, so a receiver that far behind is already being
// described by the sender's RTO rather than by SACK; and because the history
// is a window below the largest, a packet that arrives absurdly late is simply
// not acknowledged, which the sender resolves by retransmitting under a fresh
// sequence (see PacketManager.Retransmit).
type recvTracker struct {
	largest uint32
	any     bool
	bits    [ackHistory / 64]uint64
}

func (r *recvTracker) has(seq uint32) bool {
	if !r.any || seq > r.largest || r.largest-seq >= ackHistory {
		return false
	}
	i := seq % ackHistory
	return r.bits[i/64]&(1<<(i%64)) != 0
}

func (r *recvTracker) set(seq uint32) {
	i := seq % ackHistory
	r.bits[i/64] |= 1 << (i % 64)
}

func (r *recvTracker) clear(seq uint32) {
	i := seq % ackHistory
	r.bits[i/64] &^= 1 << (i % 64)
}

// add records seq and reports whether it arrived out of order: below the
// largest seen (a late or retransmitted packet filling a gap) or leaving a
// hole behind it (something in between has not arrived). Either is the
// signal to acknowledge at once, so the sender learns about the gap without
// waiting for the ACK timer. A duplicate is not out of order.
func (r *recvTracker) add(seq uint32) (outOfOrder bool) {
	if !r.any {
		r.any = true
		r.largest = seq
		r.set(seq)
		return false
	}
	switch {
	case seq > r.largest:
		gap := seq - r.largest - 1
		// The slots between the old largest and seq are being reused for
		// sequences that have not arrived, so they must read as missing.
		if seq-r.largest >= ackHistory {
			r.bits = [ackHistory / 64]uint64{}
		} else {
			for s := r.largest + 1; s < seq; s++ {
				r.clear(s)
			}
		}
		r.largest = seq
		r.set(seq)
		return gap > 0
	case seq == r.largest:
		return false
	default:
		if r.largest-seq >= ackHistory {
			return true
		}
		if r.has(seq) {
			return false
		}
		r.set(seq)
		return true
	}
}

// frame builds the ACK: the largest sequence with the run of received
// sequences directly below it, then up to five (gap, run) pairs walking
// downward, the shape the Dart side parses.
func (r *recvTracker) frame(ackDelay uint16) *AckFrame {
	if !r.any {
		return &AckFrame{LargestAcked: 0, AckDelay: ackDelay, FirstAckRangeLength: 1}
	}
	f := &AckFrame{LargestAcked: r.largest, AckDelay: ackDelay}

	// cursor walks down from the largest; steps bounds it to the history.
	cursor := r.largest
	steps := uint32(0)
	run := func() uint32 {
		n := uint32(0)
		for steps < ackHistory && r.has(cursor) {
			n++
			steps++
			if cursor == 0 {
				steps = ackHistory
				break
			}
			cursor--
		}
		return n
	}
	gap := func() uint8 {
		n := uint8(0)
		for steps < ackHistory && n < 255 && !r.has(cursor) {
			n++
			steps++
			if cursor == 0 {
				steps = ackHistory
				break
			}
			cursor--
		}
		return n
	}

	f.FirstAckRangeLength = run()
	for len(f.AckRanges) < 5 && steps < ackHistory {
		g := gap()
		if g == 0 {
			break
		}
		n := run()
		if n == 0 {
			break
		}
		f.AckRanges = append(f.AckRanges, AckRange{Gap: g, AckRangeLength: n})
	}
	return f
}
