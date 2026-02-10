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

	// Receive side
	recvBuf       []byte         // ordered data ready for reading
	recvOOO       map[uint32][]byte // out-of-order buffer: seq -> data
	nextExpectSeq uint32
	recvCond      *sync.Cond
	readDeadline  time.Time
	finReceived   bool

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
	clock() Clock
}

// NewStream creates a new stream.
func NewStream(id uint32, remoteID uint32, fc *StreamFlowController) *Stream {
	s := &Stream{
		ID:        id,
		RemoteID:  remoteID,
		state:     StreamStateIdle,
		recvOOO:   make(map[uint32][]byte),
		streamFC:  fc,
	}
	s.sendCond = sync.NewCond(&s.mu)
	s.recvCond = sync.NewCond(&s.mu)
	return s
}

// Read reads ordered data from the stream.
// Blocks until data is available, the stream is closed, or the deadline expires.
func (s *Stream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for len(s.recvBuf) == 0 {
		if s.state == StreamStateReset {
			return 0, ErrStreamReset
		}
		if s.finReceived && len(s.recvBuf) == 0 {
			return 0, io.EOF
		}
		if s.state == StreamStateClosed {
			return 0, io.EOF
		}
		if !s.readDeadline.IsZero() && time.Now().After(s.readDeadline) {
			return 0, ErrDeadlineExceeded
		}
		s.recvCond.Wait()
	}

	n := copy(p, s.recvBuf)
	s.recvBuf = s.recvBuf[n:]
	s.BytesRead += int64(n)
	return n, nil
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
		// Wait for flow control
		for s.streamFC != nil && !s.streamFC.CanSend(1) {
			if s.state == StreamStateReset {
				s.mu.Unlock()
				return total, ErrStreamReset
			}
			if !s.writeDeadline.IsZero() && time.Now().After(s.writeDeadline) {
				s.mu.Unlock()
				return total, ErrDeadlineExceeded
			}
			s.sendCond.Wait()
		}

		// Fragment by MTU
		chunkSize := MaxDatagramSize - 100 // conservative header overhead
		if chunkSize > len(data) {
			chunkSize = len(data)
		}

		chunk := make([]byte, chunkSize)
		copy(chunk, data[:chunkSize])

		conn := s.conn
		id, remoteID := s.ID, s.RemoteID
		isSyn := s.state == StreamStateIdle
		if isSyn {
			s.state = StreamStateOpen
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

// DeliverData delivers data to the stream's receive buffer.
// Deduplication is handled at the connection level (Connection.HandlePacket),
// so data arriving here is already guaranteed to be non-duplicate and in order.
func (s *Stream) DeliverData(seq uint32, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recvBuf = append(s.recvBuf, data...)

	if s.streamFC != nil {
		s.streamFC.OnDataReceived(len(data))
	}

	s.recvCond.Broadcast()
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
func (s *Stream) OnWindowUpdate(maxStreamData int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.streamFC != nil {
		s.streamFC.UpdateMaxStreamData(maxStreamData)
	}
	s.sendCond.Broadcast()
}

// State returns the current stream state.
func (s *Stream) State() StreamState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Ensure Stream implements io.ReadWriteCloser
var _ io.ReadWriteCloser = (*Stream)(nil)
