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

	// Receive side. Reassembly happens at the connection, not here: the
	// sequence number is connection-wide, so a stream only ever sees a sparse
	// subsequence of it. See Connection.HandlePacket.
	recvBuf      []byte // ordered data ready for reading
	recvCond     *sync.Cond
	readDeadline time.Time
	finReceived  bool

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
	sendStreamFrame(streamID uint32, remoteID uint32, data []byte, isFin bool, isSyn bool)
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

		s.BytesWritten += int64(chunkSize)
		s.mu.Unlock()

		// Send without holding s.mu to avoid deadlock with c.mu
		if conn != nil {
			conn.sendStreamFrame(id, remoteID, chunk, false, isSyn)
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
	s.recvCond.Broadcast()
	s.mu.Unlock()

	// Send FIN without holding s.mu to avoid deadlock with c.mu
	if conn != nil {
		conn.sendStreamFrame(id, remoteID, nil, true, false)
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
func (s *Stream) DeliverData(data []byte) {
	s.mu.Lock()

	s.recvBuf = append(s.recvBuf, data...)

	// Receipt only accounts; the window reopens in Read, when the application
	// actually consumes the bytes. Advertising on receipt would let recvBuf grow
	// without bound against a slow reader.
	if s.streamFC != nil {
		s.streamFC.OnDataReceived(len(data))
	}

	s.recvCond.Broadcast()
	s.mu.Unlock()
}

// DeliverFin marks the remote side as closed.
func (s *Stream) DeliverFin() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.finReceived = true
	if s.state == StreamStateHalfClosedLocal {
		s.state = StreamStateClosed
	} else {
		s.state = StreamStateHalfClosedRemote
	}
	s.recvCond.Broadcast()
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
