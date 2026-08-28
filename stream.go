package udx

import (
	"errors"
	"io"
	"sync"
	"time"
)

var (
	ErrStreamClosed     = errors.New("stream closed")
	ErrStreamReset      = errors.New("stream reset")
	ErrWriteAfterClose  = errors.New("write after close")
	ErrDeadlineExceeded = errors.New("deadline exceeded")
)

// StreamState represents the lifecycle state of a stream.
type StreamState int

const (
	StreamStateIdle StreamState = iota
	StreamStateOpen
	StreamStateHalfClosedLocal  // FIN sent, can still read
	StreamStateHalfClosedRemote // FIN received, can still write
	StreamStateClosed
	StreamStateReset
)

// Stream is a reliable, ordered stream over UDP.
// It implements io.ReadWriteCloser.
type Stream struct {
	mu sync.Mutex

	// Identity
	ID       uint32
	RemoteID uint32

	// State
	state     StreamState
	resetCode uint32

	// Send side
	sendBuf       []byte
	sendSeq       uint32
	sendCond      *sync.Cond
	writeDeadline time.Time

	// Receive side. Reassembly is per stream, on the byte offsets carried in
	// STREAM frames, so a stream is delayed only by its own missing data and
	// never by a sibling's.
	recvBuf    []byte            // contiguous data ready for reading
	recvOOO    map[uint64][]byte // arrived early: offset -> bytes
	recvOffset uint64            // total bytes moved into recvBuf so far
	oooBytes   int               // bytes held in recvOOO, to bound it

	// flowControlViolation is set when the peer overruns the out-of-order
	// backstop. Acted on outside the lock, since resetting takes the
	// connection's.
	flowControlViolation bool
	recvCond             *sync.Cond
	readDeadline         time.Time

	// finReceived means the peer has sent everything; finalSize is where the
	// stream ends. A FIN can overtake data still in flight, so the stream is
	// only really finished once recvOffset reaches finalSize.
	finReceived bool
	finSeen     bool
	finalSize   uint64

	// Flow control
	streamFC *StreamFlowController

	// Connection reference (set by Connection)
	conn streamConn

	// Metrics
	BytesRead    int64
	BytesWritten int64
}

// streamConn is the interface a stream needs from its parent connection.
type streamConn interface {
	sendStreamFrame(streamID uint32, remoteID uint32, offset uint64, data []byte, isFin bool, isSyn bool)
	sendResetStream(streamID uint32, remoteID uint32, errorCode uint32)
	sendWindowUpdate(streamID uint32, remoteID uint32, maxStreamData int64)
	sendStreamDataBlocked(streamID uint32, remoteID uint32, limit int64)
	awaitSendCredit(size int, deadline time.Time) bool
	clock() Clock
}

// NewStream creates a new stream.
func NewStream(id uint32, remoteID uint32, fc *StreamFlowController) *Stream {
	s := &Stream{
		ID:       id,
		RemoteID: remoteID,
		state:    StreamStateIdle,
		recvOOO:  make(map[uint64][]byte),
		streamFC: fc,
	}
	s.sendCond = sync.NewCond(&s.mu)
	s.recvCond = sync.NewCond(&s.mu)
	return s
}

// Read reads ordered data from the stream.
// Blocks until data is available, the stream is closed, or the deadline expires.
func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()

	for len(s.recvBuf) == 0 {
		if s.state == StreamStateReset {
			s.mu.Unlock()
			return 0, ErrStreamReset
		}
		if s.finReceived {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if s.state == StreamStateClosed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if !s.readDeadline.IsZero() && !time.Now().Before(s.readDeadline) {
			s.mu.Unlock()
			return 0, ErrDeadlineExceeded
		}
		// Wake up when the deadline expires. Without this the deadline is only
		// ever checked on entry, so a Read that blocks with no further data
		// arriving hangs indefinitely instead of returning ErrDeadlineExceeded.
		s.waitWithDeadline(s.recvCond, s.readDeadline)
	}

	n := copy(p, s.recvBuf)
	s.recvBuf = s.recvBuf[n:]
	s.BytesRead += int64(n)

	// Flow control is anchored to consumption, so the window only reopens here.
	var sendUpdate bool
	if s.streamFC != nil {
		sendUpdate = s.streamFC.OnDataConsumed(n)
	}
	conn := s.conn
	id, remoteID := s.ID, s.RemoteID
	s.mu.Unlock()

	// Send the update without holding s.mu to avoid deadlock with c.mu.
	if sendUpdate && conn != nil {
		conn.sendWindowUpdate(id, remoteID, s.streamFC.AdvertiseLimit())
	}
	return n, nil
}

// waitWithDeadline waits on cond, arranging a wakeup at the deadline so callers
// blocked in Wait actually observe it. A zero deadline waits indefinitely.
// Caller holds s.mu; it is held again on return.
func (s *Stream) waitWithDeadline(cond *sync.Cond, deadline time.Time) {
	if deadline.IsZero() {
		cond.Wait()
		return
	}
	d := time.Until(deadline)
	if d <= 0 {
		return
	}
	t := time.AfterFunc(d, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		cond.Broadcast()
	})
	cond.Wait()
	t.Stop()
}

// blockedWakeupLocked returns the time a flow-control-blocked writer should
// wake up: the write deadline if it lands sooner, otherwise the next
// STREAM_DATA_BLOCKED retry. Caller holds s.mu.
func (s *Stream) blockedWakeupLocked() time.Time {
	retry := time.Now().Add(StreamBlockedRetryInterval)
	if !s.writeDeadline.IsZero() && s.writeDeadline.Before(retry) {
		return s.writeDeadline
	}
	return retry
}

// Write writes data to the stream. Fragments by MTU and applies back-pressure.
func (s *Stream) Write(p []byte) (int, error) {
	s.mu.Lock()

	if s.state == StreamStateReset {
		s.mu.Unlock()
		return 0, ErrStreamReset
	}
	if s.state == StreamStateHalfClosedLocal || s.state == StreamStateClosed {
		s.mu.Unlock()
		return 0, ErrWriteAfterClose
	}

	total := 0
	data := p

	for len(data) > 0 {
		// Wait for flow-control credit.
		for s.streamFC != nil && !s.streamFC.CanSend(1) {
			if s.state == StreamStateReset {
				s.mu.Unlock()
				return total, ErrStreamReset
			}
			if s.state == StreamStateHalfClosedLocal || s.state == StreamStateClosed {
				s.mu.Unlock()
				return total, ErrWriteAfterClose
			}
			if !s.writeDeadline.IsZero() && !time.Now().Before(s.writeDeadline) {
				s.mu.Unlock()
				return total, ErrDeadlineExceeded
			}

			// Tell the peer we are stalled so it re-advertises its limit.
			// WINDOW_UPDATE rides a seq=0 control packet that is never
			// retransmitted, so a single dropped update would otherwise wedge
			// this stream permanently. Re-sent every StreamBlockedRetryInterval
			// until credit arrives.
			limit := s.streamFC.SendLimit()
			conn := s.conn
			id, remoteID := s.ID, s.RemoteID
			s.mu.Unlock()
			if conn != nil {
				conn.sendStreamDataBlocked(id, remoteID, limit)
			}
			s.mu.Lock()

			if s.streamFC.CanSend(1) {
				break // credit arrived while we were unlocked
			}
			s.waitWithDeadline(s.sendCond, s.blockedWakeupLocked())
		}

		// Fragment by MTU, clamped to the credit the peer has actually granted.
		// Without the clamp a chunk can overshoot the advertised offset by up to
		// a full chunk, which the peer is entitled to treat as a flow-control
		// violation.
		chunkSize := MaxDatagramSize - 100 // conservative header overhead
		if chunkSize > len(data) {
			chunkSize = len(data)
		}
		if s.streamFC != nil {
			if avail := s.streamFC.SendWindowAvailable(); avail < int64(chunkSize) {
				chunkSize = int(avail)
			}
		}
		if chunkSize == 0 {
			continue // no credit; back to the wait loop
		}

		chunk := make([]byte, chunkSize)
		copy(chunk, data[:chunkSize])

		conn := s.conn
		id, remoteID := s.ID, s.RemoteID
		deadline := s.writeDeadline
		isSyn := s.state == StreamStateIdle

		s.mu.Unlock()

		// Wait for congestion-window and pacing credit before committing the
		// chunk. Done outside s.mu, and before the flow-control accounting, so a
		// congestion-limited writer does not hold the stream lock or book bytes
		// it may never send.
		if conn != nil && !conn.awaitSendCredit(chunkSize, deadline) {
			s.mu.Lock()
			if s.state == StreamStateReset {
				s.mu.Unlock()
				return total, ErrStreamReset
			}
			s.mu.Unlock()
			return total, ErrDeadlineExceeded
		}

		s.mu.Lock()
		if s.state == StreamStateReset {
			s.mu.Unlock()
			return total, ErrStreamReset
		}
		// Re-check the send window: awaitSendCredit released s.mu, so a
		// concurrent writer on this stream may have consumed the credit that
		// chunkSize was clamped against.
		if s.streamFC != nil && !s.streamFC.CanSend(chunkSize) {
			continue // back to the flow-control wait loop with the lock held
		}
		if isSyn && s.state == StreamStateIdle {
			s.state = StreamStateOpen
		} else {
			isSyn = false
		}

		if s.streamFC != nil {
			s.streamFC.OnDataSent(chunkSize)
		}

		// The chunk's position in the stream is where the write had reached
		// before it, which is what the peer reassembles on.
		offset := uint64(s.BytesWritten)
		s.BytesWritten += int64(chunkSize)
		s.mu.Unlock()

		// Send without holding s.mu to avoid deadlock with c.mu
		if conn != nil {
			conn.sendStreamFrame(id, remoteID, offset, chunk, false, isSyn)
		}

		data = data[chunkSize:]
		total += chunkSize

		s.mu.Lock()
	}

	s.mu.Unlock()
	return total, nil
}

// Close sends a FIN and closes the write side.
func (s *Stream) Close() error {
	s.mu.Lock()

	if s.state == StreamStateClosed || s.state == StreamStateReset {
		s.mu.Unlock()
		return nil
	}

	if s.state == StreamStateHalfClosedRemote {
		s.state = StreamStateClosed
	} else {
		s.state = StreamStateHalfClosedLocal
	}

	conn := s.conn
	id, remoteID := s.ID, s.RemoteID
	// A FIN carries no data, so its offset is the stream's final size. The
	// receiver needs that to know when it has everything: a FIN can overtake
	// data that is still in flight, and closing the stream on its arrival
	// alone would truncate the tail.
	finalSize := uint64(s.BytesWritten)
	s.recvCond.Broadcast()
	s.mu.Unlock()

	// Send FIN without holding s.mu to avoid deadlock with c.mu
	if conn != nil {
		conn.sendStreamFrame(id, remoteID, finalSize, nil, true, false)
	}
	return nil
}

// CloseWrite closes only the write side (half-close).
func (s *Stream) CloseWrite() error {
	return s.Close()
}

// Reset sends a RESET_STREAM frame with the given error code.
func (s *Stream) Reset(errorCode uint32) error {
	s.mu.Lock()

	if s.state == StreamStateClosed || s.state == StreamStateReset {
		s.mu.Unlock()
		return nil
	}

	s.state = StreamStateReset
	s.resetCode = errorCode

	conn := s.conn
	id, remoteID := s.ID, s.RemoteID
	s.sendCond.Broadcast()
	s.recvCond.Broadcast()
	s.mu.Unlock()

	// Send reset without holding s.mu to avoid deadlock with c.mu
	if conn != nil {
		conn.sendResetStream(id, remoteID, errorCode)
	}
	return nil
}

// SetReadDeadline sets the deadline for Read operations.
func (s *Stream) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readDeadline = t
	s.recvCond.Broadcast()
	return nil
}

// SetWriteDeadline sets the deadline for Write operations.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeDeadline = t
	s.sendCond.Broadcast()
	return nil
}

// --- Receive-side methods called by Connection ---

// DeliverData appends already-ordered data to the stream's receive buffer.
//
// Reordering is NOT done here. UDP delivers out of order and the byte stream
// must be contiguous — the Noise layer above uses a sequential nonce counter,
// so a single transposition causes MAC failures — but the sequence number that
// establishes the order is allocated per *connection*, not per stream. This
// used to reorder on that number directly, which works only while a connection
// carries one stream: with two or more, each stream sees a sparse subsequence
// of the shared counter, so every packet after the first waits forever for a
// gap that belongs to a different stream. Connection.HandlePacket now holds
// packets until they are contiguous and calls this in order.
func (s *Stream) DeliverData(offset uint64, data []byte) {
	s.mu.Lock()

	accepted := s.placeLocked(offset, data)
	if s.flowControlViolation {
		s.mu.Unlock()
		// The peer sent more unorderable data than any window permits. Those
		// bytes are gone and the stream can never complete, so fail it loudly
		// instead of waiting on an offset that will never be filled.
		s.Reset(ErrorFlowControlError)
		return
	}
	if accepted > 0 {
		// Receipt only accounts; the window reopens in Read, when the
		// application actually consumes the bytes. Advertising on receipt would
		// let the receive buffer grow without bound against a slow reader.
		// Bytes waiting out of order count too — they are held either way.
		if s.streamFC != nil {
			s.streamFC.OnDataReceived(accepted)
		}
		s.finishLocked()
		s.recvCond.Broadcast()
	}
	s.mu.Unlock()
}

// placeLocked puts a chunk at its offset, moving whatever is now contiguous
// into recvBuf, and returns how many bytes were newly accepted. Caller holds
// s.mu.
func (s *Stream) placeLocked(offset uint64, data []byte) int {
	end := offset + uint64(len(data))

	// Already delivered. A retransmission of data the application has seen
	// arrives here, and must not be appended a second time — the byte stream
	// would be corrupted, and the Noise layer above would fail its MAC rather
	// than merely reading duplicates.
	if end <= s.recvOffset {
		return 0
	}

	// Partly delivered: keep only the tail that is new.
	if offset < s.recvOffset {
		data = data[s.recvOffset-offset:]
		offset = s.recvOffset
	}

	if offset > s.recvOffset {
		// Ahead of the gap. Hold it until the missing bytes arrive.
		if _, exists := s.recvOOO[offset]; exists {
			return 0
		}
		// Flow control is the real bound on this buffer; the cap is only a
		// backstop against a peer that ignores its limit, which is why it sits
		// well above the largest window rather than at it.
		//
		// Discarding here is never safe. The packet was acknowledged on
		// arrival, so the sender has already stopped tracking it and will never
		// send those bytes again: dropping them silently strands the stream at
		// that offset for good. A cap equal to MaxStreamRecvWindow did exactly
		// that, because a stream whose window has auto-tuned to the maximum can
		// legitimately have the whole window sitting out of order — the netem
		// reorder-20pct condition hit it and hung for the full two minutes.
		// Hitting the backstop now means the peer is misbehaving, so say so
		// rather than quietly losing data.
		if s.oooBytes+len(data) > maxStreamRecvOOO {
			s.flowControlViolation = true
			return 0
		}
		buf := make([]byte, len(data))
		copy(buf, data)
		s.recvOOO[offset] = buf
		s.oooBytes += len(buf)
		return len(buf)
	}

	s.recvBuf = append(s.recvBuf, data...)
	s.recvOffset = end
	accepted := len(data)

	// Release anything that was waiting on the bytes just delivered.
	for {
		chunk, ok := s.recvOOO[s.recvOffset]
		if !ok {
			break
		}
		delete(s.recvOOO, s.recvOffset)
		s.oooBytes -= len(chunk)
		s.recvBuf = append(s.recvBuf, chunk...)
		s.recvOffset += uint64(len(chunk))
	}
	return accepted
}

// DeliverFin records where the stream ends. finalSize is the offset one past
// the peer's last byte.
//
// A FIN carries no data of its own and can overtake data still in flight, so
// arrival is not the same as completion: treating it as EOF on sight would
// truncate the tail. The stream finishes when the delivered bytes reach
// finalSize, which may be now or may be several packets away.
func (s *Stream) DeliverFin(finalSize uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.finSeen = true
	s.finalSize = finalSize
	s.finishLocked()
	s.recvCond.Broadcast()
}

// finishLocked completes the stream once every byte up to finalSize has been
// delivered. Caller holds s.mu.
func (s *Stream) finishLocked() {
	if !s.finSeen || s.finReceived || s.recvOffset < s.finalSize {
		return
	}
	s.finReceived = true
	if s.state == StreamStateHalfClosedLocal {
		s.state = StreamStateClosed
	} else {
		s.state = StreamStateHalfClosedRemote
	}
}

// DeliverReset handles a RESET_STREAM from the remote.
func (s *Stream) DeliverReset(errorCode uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = StreamStateReset
	s.resetCode = errorCode
	s.sendCond.Broadcast()
	s.recvCond.Broadcast()
}

// OnWindowUpdate is called when a WINDOW_UPDATE is received for this stream.
// wire is the frame's payload: the peer's advertised offset modulo 2^32.
func (s *Stream) OnWindowUpdate(wire uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.streamFC != nil {
		s.streamFC.ApplyWindowUpdate(wire)
	}
	s.sendCond.Broadcast()
}

// BufferedBytes returns bytes received on this stream but not yet consumed by
// the application. Exported so flow-control back-pressure can be asserted from
// outside the package, notably by the Go<->Dart interop tests.
func (s *Stream) BufferedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamFC == nil {
		return 0
	}
	return s.streamFC.BufferedBytes()
}

// RecvWindow returns the current auto-tuned receive window for this stream.
//
// Together with BufferedBytes this pins the flow-control invariant a compliant
// peer must satisfy: we advertise dataConsumed + recvWindow as an absolute
// offset, and a sender that respects it can never have more than recvWindow
// bytes outstanding and unconsumed. BufferedBytes() <= RecvWindow() is
// therefore the exact test of whether a peer honours the advertised offset.
func (s *Stream) RecvWindow() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streamFC == nil {
		return 0
	}
	return s.streamFC.RecvWindow()
}

// State returns the current stream state.
func (s *Stream) State() StreamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Ensure Stream implements io.ReadWriteCloser
var _ io.ReadWriteCloser = (*Stream)(nil)
