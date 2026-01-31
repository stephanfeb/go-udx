package udx

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

var (
	ErrMaxStreams        = errors.New("max streams exceeded")
	ErrHandshakeTimeout = errors.New("handshake timeout")
)

// ConnectionState represents the lifecycle of a connection.
type ConnectionState int

const (
	ConnStateNew ConnectionState = iota
	ConnStateHandshaking
	ConnStateEstablished
	ConnStateClosing
	ConnStateClosed
)

// Connection represents a single peer connection.
type Connection struct {
	mu sync.Mutex

	localCID  ConnectionID
	remoteCID ConnectionID
	localAddr net.Addr
	remoteAddr net.Addr

	state ConnectionState
	clk   Clock

	// Stream management
	streams        map[uint32]*Stream
	nextStreamID   uint32
	isInitiator    bool
	maxStreams      int
	incomingStreams chan *Stream

	// Components
	cc    *CongestionController
	pm    *PacketManager
	fc    *FlowController
	pmtud *PMTUDController

	// Path migration
	pathChallenge     [8]byte
	pathChallengeResp chan [8]byte

	// Anti-amplification
	bytesSent     int64
	bytesReceived int64
	addrValidated bool

	// Packet sending
	sendFunc func(data []byte, addr net.Addr) error

	// Close
	closeOnce sync.Once
	closeCh   chan struct{}
	closeErr  error
}

// NewConnection creates a new connection.
func NewConnection(
	localCID, remoteCID ConnectionID,
	localAddr, remoteAddr net.Addr,
	isInitiator bool,
	clk Clock,
	sendFunc func(data []byte, addr net.Addr) error,
) *Connection {
	c := &Connection{
		localCID:       localCID,
		remoteCID:      remoteCID,
		localAddr:      localAddr,
		remoteAddr:     remoteAddr,
		state:          ConnStateNew,
		clk:            clk,
		streams:        make(map[uint32]*Stream),
		isInitiator:    isInitiator,
		maxStreams:      InitialMaxStreams,
		incomingStreams: make(chan *Stream, 16),
		pmtud:          NewPMTUDController(),
		pathChallengeResp: make(chan [8]byte, 1),
		sendFunc:       sendFunc,
		closeCh:        make(chan struct{}),
	}

	c.cc = NewCongestionController(clk, func() int {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.pm != nil {
			return c.pm.LastSentSeq()
		}
		return -1
	})
	c.pm = NewPacketManager(clk, c.cc)
	c.fc = NewFlowController(int64(InitialMaxData), int64(InitialMaxData))

	// Odd stream IDs for initiator, even for responder
	if isInitiator {
		c.nextStreamID = 1
	} else {
		c.nextStreamID = 2
	}

	return c
}

// OpenStream creates a new bidirectional stream.
func (c *Connection) OpenStream(ctx context.Context) (*Stream, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.state == ConnStateClosed || c.state == ConnStateClosing {
		return nil, ErrConnectionClosed
	}

	if len(c.streams) >= c.maxStreams {
		return nil, ErrMaxStreams
	}

	id := c.nextStreamID
	c.nextStreamID += 2

	fc := NewStreamFlowController(int64(InitialMaxStreamData), int64(InitialMaxStreamData))
	s := NewStream(id, 0, fc) // remoteID set during handshake
	s.conn = c
	// Leave state as Idle so the first Write sends a SYN frame
	c.streams[id] = s
	return s, nil
}

// AcceptStream waits for an incoming stream from the remote.
func (c *Connection) AcceptStream(ctx context.Context) (*Stream, error) {
	select {
	case s := <-c.incomingStreams:
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closeCh:
		return nil, ErrConnectionClosed
	}
}

// Close closes the connection with no error.
func (c *Connection) Close() error {
	return c.CloseWithError(ErrorNoError, "")
}

// CloseWithError closes the connection with an error code and reason.
func (c *Connection) CloseWithError(code uint32, reason string) error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.state = ConnStateClosing
		c.mu.Unlock()

		// Send CONNECTION_CLOSE
		frame := &ConnectionCloseFrame{
			ErrorCode:    code,
			ReasonPhrase: reason,
		}
		c.sendFrames([]Frame{frame})

		c.mu.Lock()
		c.state = ConnStateClosed
		close(c.closeCh)

		// Close all streams
		for _, s := range c.streams {
			s.DeliverReset(code)
		}
		c.mu.Unlock()

		c.cc.Destroy()
		c.pm.Destroy()
	})
	return nil
}

// Ping sends a PING and waits for acknowledgment.
func (c *Connection) Ping(ctx context.Context) error {
	c.sendFrames([]Frame{&PingFrame{}})
	// In a full implementation, we'd wait for the ACK.
	// For now, fire-and-forget.
	return nil
}

// LocalAddr returns the local address.
func (c *Connection) LocalAddr() net.Addr { return c.localAddr }

// RemoteAddr returns the remote address.
func (c *Connection) RemoteAddr() net.Addr { return c.remoteAddr }

// State returns the connection state.
func (c *Connection) State() ConnectionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// --- streamConn interface ---

func (c *Connection) sendStreamFrame(streamID, remoteID uint32, data []byte, isFin, isSyn bool) {
	frame := &StreamFrame{
		IsFin: isFin,
		IsSyn: isSyn,
		Data:  data,
	}
	// DstStreamID = remote's stream ID, SrcStreamID = our local stream ID
	c.sendPacket(remoteID, streamID, []Frame{frame})
}

func (c *Connection) sendResetStream(streamID, remoteID uint32, errorCode uint32) {
	frame := &ResetStreamFrame{ErrorCode: errorCode}
	// DstStreamID = remote's stream ID, SrcStreamID = our local stream ID
	c.sendPacket(remoteID, streamID, []Frame{frame})
}

func (c *Connection) clock() Clock { return c.clk }

// --- Packet handling ---

func (c *Connection) sendPacket(dstStreamID, srcStreamID uint32, frames []Frame) {
	seq := c.pm.NextSequence()

	pkt := &Packet{
		Version:             VersionCurrent,
		DestinationCID:      c.remoteCID,
		SourceCID:           c.localCID,
		Sequence:            seq,
		DestinationStreamID: dstStreamID,
		SourceStreamID:      srcStreamID,
		Frames:              frames,
	}

	data := MarshalPacket(pkt)

	// Track in packet manager
	sentPkt := &SentPacket{
		Sequence: seq,
		Size:     len(data),
		Frames:   frames,
	}
	c.pm.SendPacket(sentPkt)
	c.cc.OnPacketSent(len(data))

	// Anti-amplification check
	c.mu.Lock()
	if !c.addrValidated && c.bytesSent+int64(len(data)) > c.bytesReceived*AmplificationFactor {
		c.mu.Unlock()
		return // Drop — anti-amplification limit
	}
	c.bytesSent += int64(len(data))
	c.mu.Unlock()

	if c.sendFunc != nil {
		c.sendFunc(data, c.remoteAddr)
	}
}

func (c *Connection) sendFrames(frames []Frame) {
	c.sendPacket(0, 0, frames)
}

// HandlePacket processes an incoming packet.
func (c *Connection) HandlePacket(pkt *Packet) {
	c.mu.Lock()
	c.bytesReceived += int64(len(MarshalPacket(pkt))) // approximate
	c.mu.Unlock()

	for _, frame := range pkt.Frames {
		c.handleFrame(pkt, frame)
	}
}

func (c *Connection) handleFrame(pkt *Packet, frame Frame) {
	switch f := frame.(type) {
	case *StreamFrame:
		c.handleStreamFrame(pkt, f)
	case *AckFrame:
		c.handleAckFrame(f)
	case *WindowUpdateFrame:
		c.handleWindowUpdate(pkt.SourceStreamID, f)
	case *MaxDataFrame:
		c.fc.UpdateMaxData(int64(f.MaxData))
	case *ResetStreamFrame:
		c.handleResetStream(pkt.SourceStreamID, f)
	case *ConnectionCloseFrame:
		c.Close()
	case *PingFrame:
		// Respond with ACK (handled by packet-level ACK)
	case *PathChallengeFrame:
		c.sendFrames([]Frame{&PathResponseFrame{Data: f.Data}})
	case *PathResponseFrame:
		select {
		case c.pathChallengeResp <- f.Data:
		default:
		}
	case *MaxStreamsFrame:
		c.mu.Lock()
		if int(f.MaxStreamCount) > c.maxStreams {
			c.maxStreams = int(f.MaxStreamCount)
		}
		c.mu.Unlock()
	case *NewConnectionIDFrame:
		// Store for future use
	case *RetireConnectionIDFrame:
		// Handle CID retirement
	case *PaddingFrame:
		// Ignore
	case *MTUProbeFrame:
		// Respond via ACK
	case *StopSendingFrame:
		c.mu.Lock()
		if s, ok := c.streams[pkt.DestinationStreamID]; ok {
			c.mu.Unlock()
			s.Reset(f.ErrorCode)
		} else {
			c.mu.Unlock()
		}
	case *DataBlockedFrame:
		// Peer is blocked at connection level; send MAX_DATA
		c.sendFrames([]Frame{&MaxDataFrame{MaxData: uint64(c.fc.ConnMaxData())}})
	case *StreamDataBlockedFrame:
		// Peer stream is blocked; send WINDOW_UPDATE
	}
}

func (c *Connection) handleStreamFrame(pkt *Packet, f *StreamFrame) {
	// DstStreamID = our local stream ID (0 if SYN for new stream)
	// SrcStreamID = remote peer's local stream ID
	localStreamID := pkt.DestinationStreamID
	remoteStreamID := pkt.SourceStreamID

	c.mu.Lock()
	// Try to find stream by our local ID first
	s, ok := c.streams[localStreamID]
	if !ok && remoteStreamID != 0 {
		// Try to find by remote stream ID
		for _, candidate := range c.streams {
			if candidate.RemoteID == remoteStreamID {
				s = candidate
				ok = true
				break
			}
		}
	}
	if !ok && f.IsSyn && remoteStreamID != 0 {
		// Incoming stream — assign a new local ID, use remote's ID as RemoteID
		id := c.nextStreamID
		c.nextStreamID += 2

		fc := NewStreamFlowController(int64(InitialMaxStreamData), int64(InitialMaxStreamData))
		s = NewStream(id, remoteStreamID, fc)
		s.conn = c
		s.state = StreamStateOpen
		c.streams[id] = s
		c.mu.Unlock()

		select {
		case c.incomingStreams <- s:
		default:
			// Channel full, drop
		}
	} else {
		c.mu.Unlock()
	}

	if s == nil {
		return
	}

	if len(f.Data) > 0 {
		s.DeliverData(pkt.Sequence, f.Data)
	}
	if f.IsFin {
		s.DeliverFin()
	}
}

func (c *Connection) handleAckFrame(f *AckFrame) {
	acked := c.pm.HandleAckFrame(f)
	for _, seq := range acked {
		// Notify congestion controller
		if pkt := c.pm.GetPacket(seq); pkt != nil {
			c.cc.OnPacketAcked(pkt.Size, pkt.SentTime, time.Duration(f.AckDelay)*time.Millisecond,
				true, int(f.LargestAcked))
		}
	}
}

func (c *Connection) handleWindowUpdate(streamID uint32, f *WindowUpdateFrame) {
	c.mu.Lock()
	s, ok := c.streams[streamID]
	c.mu.Unlock()
	if ok {
		s.OnWindowUpdate(int64(f.WindowSize))
	}
}

func (c *Connection) handleResetStream(streamID uint32, f *ResetStreamFrame) {
	c.mu.Lock()
	s, ok := c.streams[streamID]
	c.mu.Unlock()
	if ok {
		s.DeliverReset(f.ErrorCode)
	}
}

// InitiatePathMigration sends a PATH_CHALLENGE to the new address.
func (c *Connection) InitiatePathMigration(ctx context.Context, newAddr net.Addr) error {
	var challenge [8]byte
	if _, err := rand.Read(challenge[:]); err != nil {
		return fmt.Errorf("generating path challenge: %w", err)
	}

	c.mu.Lock()
	c.pathChallenge = challenge
	c.mu.Unlock()

	c.sendFrames([]Frame{&PathChallengeFrame{Data: challenge}})

	select {
	case resp := <-c.pathChallengeResp:
		if resp != challenge {
			return errors.New("path response mismatch")
		}
		c.mu.Lock()
		c.remoteAddr = newAddr
		c.addrValidated = true
		c.mu.Unlock()
		return nil
	case <-time.After(5 * time.Second):
		return errors.New("path migration timeout")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StreamCount returns the number of active streams.
func (c *Connection) StreamCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.streams)
}
