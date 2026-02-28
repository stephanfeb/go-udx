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

	// Receive-side packet deduplication (keyed by seq<<32|size)
	seenPackets  map[uint64]struct{}
	highWaterSeq uint32

	// Receive-side SACK tracking: records sequence numbers of received
	// data-bearing packets so ACKs can include selective acknowledgment ranges.
	recvdDataSeqs map[uint32]struct{}

	// Anti-amplification
	bytesSent     int64
	bytesReceived int64
	addrValidated bool

	// Packet sending
	sendFunc func(data []byte, addr net.Addr) error

	// Cleanup callback (set by multiplexer to remove from connection map)
	onClose func()

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
		pmtud:        NewPMTUDController(),
		seenPackets:   make(map[uint64]struct{}),
		recvdDataSeqs: make(map[uint32]struct{}),
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
	c.pm.OnRetransmit = func(pkt *SentPacket) {
		c.retransmitPacket(pkt)
	}
	c.pm.OnPacketPermanentLoss = func(pkt *SentPacket) {
		c.cc.OnPacketLost(pkt.Size)
	}
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

		// Collect streams under lock, then reset without lock to avoid deadlock
		streams := make([]*Stream, 0, len(c.streams))
		for _, s := range c.streams {
			streams = append(streams, s)
		}
		c.mu.Unlock()

		for _, s := range streams {
			s.DeliverReset(code)
		}

		c.cc.Destroy()
		c.pm.Destroy()

		if c.onClose != nil {
			c.onClose()
		}
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

func (c *Connection) sendWindowUpdate(streamID, remoteID uint32, windowSize int) {
	frame := &WindowUpdateFrame{WindowSize: uint32(windowSize)}
	c.sendPacket(remoteID, streamID, []Frame{frame})
}

func (c *Connection) clock() Clock { return c.clk }

// findStream looks up a stream by local ID first, then falls back to searching
// by remote ID. This is needed because the Dart UDX transport assigns random
// stream IDs that don't match Go's sequential IDs — the DestinationStreamID in
// packets from Dart may not match our local stream ID.
func (c *Connection) findStream(localID, remoteID uint32) *Stream {
	c.mu.Lock()
	defer c.mu.Unlock()

	if s, ok := c.streams[localID]; ok {
		return s
	}
	if remoteID != 0 {
		for _, s := range c.streams {
			if s.RemoteID == remoteID {
				return s
			}
		}
	}
	return nil
}

// --- Packet handling ---

func (c *Connection) sendPacket(dstStreamID, srcStreamID uint32, frames []Frame) {
	// Determine if this packet carries data (stream frames with data/SYN/FIN).
	// Control-only packets (ACKs, window updates, etc.) reuse the last data
	// sequence number to avoid creating sequence gaps that break the Dart UDX
	// receive ordering. The Dart only buffers OOO packets with stream data;
	// pure-control packets with new sequences cause _nextExpectedSeq to stick.
	hasData := false
	for _, f := range frames {
		if sf, ok := f.(*StreamFrame); ok {
			if len(sf.Data) > 0 || sf.IsSyn || sf.IsFin {
				hasData = true
				break
			}
		}
	}

	var seq uint32
	if hasData {
		seq = c.pm.NextSequence()
	} else {
		// Control-only packets (ACKs, window updates, etc.) always use seq=0.
		// This prevents them from consuming sequence numbers in the receiver's
		// ordering logic. The Dart UDX receive ordering only advances
		// _nextExpectedSeq for data-bearing packets; control packets with new
		// sequences create gaps that stall delivery forever.
		seq = 0
	}

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

	// Only track data packets in the packet manager (control packets use reused seq)
	if hasData {
		sentPkt := &SentPacket{
			Sequence:            seq,
			Size:                len(data),
			Frames:              frames,
			DestinationStreamID: dstStreamID,
			SourceStreamID:      srcStreamID,
		}
		c.pm.SendPacket(sentPkt)
	}
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

// retransmitPacket re-sends a lost packet using its ORIGINAL sequence number.
// This bypasses sendPacket() to avoid allocating a new sequence (which would
// break the receiver's ordering and corrupt the Noise nonce chain).
func (c *Connection) retransmitPacket(pkt *SentPacket) {
	pkt.RetransmitCount++
	pkt.LastRetransmit = c.clk.Now()

	rePkt := &Packet{
		Version:             VersionCurrent,
		DestinationCID:      c.remoteCID,
		SourceCID:           c.localCID,
		Sequence:            pkt.Sequence, // original sequence number
		DestinationStreamID: pkt.DestinationStreamID,
		SourceStreamID:      pkt.SourceStreamID,
		Frames:              pkt.Frames,
	}

	data := MarshalPacket(rePkt)
	c.cc.OnPacketSent(len(data))

	c.mu.Lock()
	c.bytesSent += int64(len(data))
	c.mu.Unlock()

	if c.sendFunc != nil {
		c.sendFunc(data, c.remoteAddr)
	}
}

// HandlePacket processes an incoming packet with deduplication.
// Tracks seen packets to prevent duplicate stream data delivery,
// which corrupts the Noise encryption layer (chacha20poly1305 MAC failures).
//
// The dedup key is (sequence, marshaledSize) because the Dart UDX
// implementation sends genuinely different packets with the same sequence
// number (e.g. connection SYN, stream SYN, and data all at seq=0).
// Using sequence alone would wrongly suppress these distinct packets.
// True retransmissions have the same sequence AND the same size.
func (c *Connection) HandlePacket(pkt *Packet) {
	marshaledData := MarshalPacket(pkt)
	pktSize := uint32(len(marshaledData))

	c.mu.Lock()
	c.bytesReceived += int64(pktSize)

	seq := pkt.Sequence
	// Composite key: upper 32 bits = sequence, lower 32 bits = packet size
	dedupeKey := uint64(seq)<<32 | uint64(pktSize)
	seen := false
	if _, exists := c.seenPackets[dedupeKey]; exists {
		seen = true
	}

	if !seen {
		c.seenPackets[dedupeKey] = struct{}{}
		if seq > c.highWaterSeq {
			c.highWaterSeq = seq
		}
		// Prevent unbounded memory growth: trim entries far below high water mark
		if len(c.seenPackets) > 1000 {
			threshold := uint64(c.highWaterSeq-500) << 32
			for k := range c.seenPackets {
				if k < threshold {
					delete(c.seenPackets, k)
				}
			}
		}
	}
	c.mu.Unlock()

	// Process all frames. For true duplicates (same seq+size), skip StreamFrame
	// data delivery to prevent corrupting the ordered byte stream, but always
	// process control frames (ACK, WindowUpdate, Ping, etc.).
	for _, frame := range pkt.Frames {
		if seen {
			if sf, ok := frame.(*StreamFrame); ok {
				if len(sf.Data) > 0 {
					continue // Skip duplicate stream data
				}
				// Still process SYN/FIN flags even on duplicate seq
			}
		}
		c.handleFrame(pkt, frame)
	}

	// Send ACK with SACK ranges so the remote sender can identify lost packets
	if pkt.SourceStreamID != 0 || pkt.DestinationStreamID != 0 {
		if seq > 0 {
			c.mu.Lock()
			c.recvdDataSeqs[seq] = struct{}{}
			c.mu.Unlock()
		}
		c.sendPacket(pkt.SourceStreamID, pkt.DestinationStreamID, []Frame{c.buildAckFrame(seq)})
	}
}

// buildAckFrame constructs an AckFrame with SACK ranges from received data
// packet sequences. This allows the remote sender to identify exactly which
// packets were lost and selectively retransmit them.
func (c *Connection) buildAckFrame(latestSeq uint32) *AckFrame {
	c.mu.Lock()
	defer c.mu.Unlock()

	// No data packets received yet — send simple ACK for the current packet
	if len(c.recvdDataSeqs) == 0 || latestSeq == 0 {
		return &AckFrame{
			LargestAcked:        latestSeq,
			AckDelay:            0,
			FirstAckRangeLength: 1,
		}
	}

	// Find the largest received sequence
	largest := latestSeq
	for seq := range c.recvdDataSeqs {
		if seq > largest {
			largest = seq
		}
	}

	// Build first range: count consecutive seqs downward from largest
	firstRangeLen := uint32(0)
	cursor := largest
	for {
		if _, ok := c.recvdDataSeqs[cursor]; ok {
			firstRangeLen++
			if cursor == 0 {
				break
			}
			cursor--
		} else {
			break
		}
	}

	frame := &AckFrame{
		LargestAcked:        largest,
		AckDelay:            0,
		FirstAckRangeLength: firstRangeLen,
	}

	// Build additional SACK ranges (max 5).
	// cursor is now the first missing seq below the first range.
	for len(frame.AckRanges) < 5 && cursor > 0 {
		// Count gap (consecutive missing seqs)
		gap := uint8(0)
		for cursor > 0 {
			if _, ok := c.recvdDataSeqs[cursor]; !ok {
				gap++
				cursor--
				if gap == 255 {
					break
				}
			} else {
				break
			}
		}
		if gap == 0 {
			break
		}

		// Count acked range (consecutive received seqs)
		rangeLen := uint32(0)
		for {
			if _, ok := c.recvdDataSeqs[cursor]; ok {
				rangeLen++
				if cursor == 0 {
					break
				}
				cursor--
			} else {
				break
			}
		}
		if rangeLen == 0 {
			break
		}

		frame.AckRanges = append(frame.AckRanges, AckRange{
			Gap:            gap,
			AckRangeLength: rangeLen,
		})
	}

	// Prune old entries to bound memory
	if len(c.recvdDataSeqs) > 500 {
		threshold := largest - 500
		for seq := range c.recvdDataSeqs {
			if seq < threshold {
				delete(c.recvdDataSeqs, seq)
			}
		}
	}

	return frame
}

func (c *Connection) handleFrame(pkt *Packet, frame Frame) {
	switch f := frame.(type) {
	case *StreamFrame:
		c.handleStreamFrame(pkt, f)
	case *AckFrame:
		c.handleAckFrame(f)
	case *WindowUpdateFrame:
		c.handleWindowUpdate(pkt, f)
	case *MaxDataFrame:
		c.fc.UpdateMaxData(int64(f.MaxData))
	case *ResetStreamFrame:
		c.handleResetStream(pkt, f)
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
		if s := c.findStream(pkt.DestinationStreamID, pkt.SourceStreamID); s != nil {
			s.Reset(f.ErrorCode)
		}
	case *DataBlockedFrame:
		// Peer is blocked at connection level; send MAX_DATA
		c.sendFrames([]Frame{&MaxDataFrame{MaxData: uint64(c.fc.ConnMaxData())}})
	case *StreamDataBlockedFrame:
		// Peer stream is blocked; send WINDOW_UPDATE with current receive window
		if s := c.findStream(pkt.DestinationStreamID, pkt.SourceStreamID); s != nil {
			if s.streamFC != nil {
				newWindow := s.streamFC.GrowRecvWindow()
				c.sendWindowUpdate(s.ID, s.RemoteID, int(newWindow))
			}
		}
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

	// SACK-based loss detection: retransmit packets that fall within gaps.
	lost := c.pm.DetectLostPackets(f)
	for _, seq := range lost {
		if pkt := c.pm.GetPacket(seq); pkt != nil {
			c.retransmitPacket(pkt)
		}
	}
}

func (c *Connection) handleWindowUpdate(pkt *Packet, f *WindowUpdateFrame) {
	s := c.findStream(pkt.DestinationStreamID, pkt.SourceStreamID)
	if s != nil {
		s.OnWindowUpdate(int64(f.WindowSize))
	}
}

func (c *Connection) handleResetStream(pkt *Packet, f *ResetStreamFrame) {
	s := c.findStream(pkt.DestinationStreamID, pkt.SourceStreamID)
	if s != nil {
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
