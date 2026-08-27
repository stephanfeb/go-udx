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

	// Receive-side SACK tracking: records sequence numbers of received
	// data-bearing packets so ACKs can include selective acknowledgment ranges.
	recvdDataSeqs map[uint32]struct{}

	// Connection-level receive ordering. Sequence numbers are allocated per
	// connection, so this is the only layer that sees a dense run of them and
	// therefore the only layer that can reassemble. Holds data-bearing packets
	// that arrived ahead of a gap until the gap is filled.
	nextExpectSeq uint32
	recvSeqInit   bool
	recvOOO       map[uint32]*Packet

	// Anti-amplification
	bytesSent     int64
	bytesReceived int64
	addrValidated bool

	// Packet sending
	sendFunc func(data []byte, addr net.Addr) error

	// Cleanup callback (set by multiplexer to remove from connection map)
	onClose func()

	// Send-credit signalling: writers park here when the congestion window or
	// pacer is closed, and handleAckFrame wakes them.
	sendCreditMu sync.Mutex
	sendCredit   *sync.Cond

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
		recvdDataSeqs: make(map[uint32]struct{}),
		recvOOO:       make(map[uint32]*Packet),
		pathChallengeResp: make(chan [8]byte, 1),
		sendFunc:       sendFunc,
		closeCh:        make(chan struct{}),
	}

	c.sendCredit = sync.NewCond(&c.sendCreditMu)

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

		c.sendCreditMu.Lock()
		c.sendCredit.Broadcast()
		c.sendCreditMu.Unlock()

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

// sendWindowUpdate advertises an ABSOLUTE offset limit for the stream: the
// highest cumulative byte position the peer may send. It is not a window size.
// See StreamFlowController for why absolute offsets are required here.
//
// The uint32 conversion is a deliberate truncation: the offset travels modulo
// 2^32 and the peer reconstructs it against the limit it already holds, which
// is what keeps a stream's lifetime transfer unbounded over a 4-byte field.
func (c *Connection) sendWindowUpdate(streamID, remoteID uint32, maxStreamData int64) {
	frame := &WindowUpdateFrame{WindowSize: uint32(maxStreamData)}
	c.sendPacket(remoteID, streamID, []Frame{frame})
}

// sendStreamDataBlocked tells the peer we have run out of send credit on this
// stream at the given offset, prompting it to re-advertise its limit.
func (c *Connection) sendStreamDataBlocked(streamID, remoteID uint32, limit int64) {
	frame := &StreamDataBlockedFrame{StreamID: streamID, MaxStreamData: uint64(limit)}
	c.sendPacket(remoteID, streamID, []Frame{frame})
}

// awaitSendCredit blocks until the congestion window and pacer allow another
// packet of the given size, the write deadline passes, or the connection
// closes. Returns false if the caller should stop trying.
//
// Nothing used to gate the send path on the congestion window at all: cwnd,
// CUBIC and the pacer were all implemented but unreachable, so a writer emitted
// packets as fast as flow control allowed. That was invisible only because the
// stream window capped every transfer at ~256KB; once that ceiling was lifted a
// sender would dump megabytes into the socket at once and drown itself in loss.
func (c *Connection) awaitSendCredit(size int, deadline time.Time) bool {
	c.sendCreditMu.Lock()
	defer c.sendCreditMu.Unlock()

	for {
		select {
		case <-c.closeCh:
			return false
		default:
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return false
		}

		wait := c.cc.Pacer.TimeUntilSend()
		if wait <= 0 && c.cc.CanSend(size) {
			return true
		}
		if wait <= 0 || wait > sendCreditPollInterval {
			// Either we are cwnd-limited (woken by handleAckFrame) or the pacer
			// wants longer than one poll. Cap the sleep so a lost ACK cannot
			// wedge the writer: the PTO timer will retransmit and free inflight.
			wait = sendCreditPollInterval
		}

		timer := time.AfterFunc(wait, func() {
			c.sendCreditMu.Lock()
			defer c.sendCreditMu.Unlock()
			c.sendCredit.Broadcast()
		})
		c.sendCredit.Wait()
		timer.Stop()
	}
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

// isDataBearing reports whether a packet carries stream data or a stream
// lifecycle flag, i.e. whether it consumed a sequence number and is tracked by
// the packet manager. Mirrors the hasData test in sendPacket.
func isDataBearing(pkt *Packet) bool {
	for _, f := range pkt.Frames {
		if sf, ok := f.(*StreamFrame); ok {
			if len(sf.Data) > 0 || sf.IsSyn || sf.IsFin {
				return true
			}
		}
	}
	return false
}

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
		// Only data-bearing packets enter the congestion controller's inflight
		// accounting. Control packets (ACKs, window updates) are never
		// acknowledged, so counting them would inflate inflight monotonically
		// until nothing could be sent.
		c.cc.OnPacketSent(len(data))
	}

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

	// No OnPacketSent here: the original transmission is still counted in
	// inflight and this packet keeps its original sequence number, so counting
	// it again would double-charge the congestion window for one packet.

	c.mu.Lock()
	c.bytesSent += int64(len(data))
	c.mu.Unlock()

	if c.sendFunc != nil {
		c.sendFunc(data, c.remoteAddr)
	}
}

// HandlePacket processes an incoming packet.
//
// Duplicate stream data must never reach a stream twice — it corrupts the Noise
// layer above, which uses a sequential nonce counter (chacha20poly1305 MAC
// failures). That guarantee comes from reassemble, which delivers each sequence
// exactly once by construction.
//
// This used to be a separate (sequence, marshaledSize) dedupe table consulted
// before delivery. It could not be relied on for correctness in either
// direction: it was trimmed above 1000 entries, so a long-delayed duplicate
// went unrecognised, and — worse — a packet recorded here but then dropped for
// want of reassembly buffer would have its retransmission suppressed as a
// duplicate and stall the stream permanently. Sequence order answers the
// question exactly, so nothing is gained by asking twice.
func (c *Connection) HandlePacket(pkt *Packet) {
	c.mu.Lock()
	c.bytesReceived += int64(len(MarshalPacket(pkt)))
	c.mu.Unlock()

	seq := pkt.Sequence

	// Non-STREAM frames are processed on arrival, never held for reassembly.
	// Only the byte stream needs ordering; ACKs, window updates and the rest
	// are self-describing. Queueing them behind a gap would be actively harmful
	// — an ACK trapped behind the very packet it reports lost cannot drive the
	// retransmission that fills the gap, which deadlocks recovery.
	for _, frame := range pkt.Frames {
		if _, ok := frame.(*StreamFrame); ok {
			continue
		}
		c.handleFrame(pkt, frame)
	}

	if !isDataBearing(pkt) {
		return
	}

	// Send ACK with SACK ranges so the remote sender can identify lost packets.
	// This reflects *receipt*, not delivery: a packet buffered ahead of a gap is
	// still safely held, and reporting it lets the peer retransmit only what is
	// genuinely missing.
	//
	// Only data-bearing packets are acknowledged. Acknowledging control-only
	// packets makes every ACK provoke an ACK in return, which ping-pongs
	// without end: a 64KB transfer measured ~176,000 ack-only datagrams for 48
	// data packets before this guard existed. It is also the correct rule
	// independently — sendPacket only registers data-bearing packets with the
	// packet manager, so an ACK for a control packet can never match anything
	// in flight.
	if seq > 0 {
		c.mu.Lock()
		c.recvdDataSeqs[seq] = struct{}{}
		c.mu.Unlock()
	}
	c.sendPacket(pkt.SourceStreamID, pkt.DestinationStreamID, []Frame{c.buildAckFrame(seq)})

	for _, ready := range c.reassemble(pkt) {
		c.deliverStreamFrames(ready)
	}
}

// reassemble places a data-bearing packet in connection sequence order and
// returns the packets that are now contiguous, oldest first. Returns nil while
// a gap remains.
//
// Ordering belongs here rather than in Stream because sendPacket allocates one
// sequence per connection. A stream sees only the subsequence addressed to it,
// which is dense only when the connection carries a single stream — the reason
// concurrent streams stalled after exactly one packet each.
func (c *Connection) reassemble(pkt *Packet) []*Packet {
	c.mu.Lock()
	defer c.mu.Unlock()

	seq := pkt.Sequence

	// The peer's first data packet establishes the baseline; it is not
	// necessarily zero, since the handshake may consume sequences.
	if !c.recvSeqInit {
		c.nextExpectSeq = seq
		c.recvSeqInit = true
	}

	if seq != c.nextExpectSeq {
		if seq > c.nextExpectSeq {
			if _, exists := c.recvOOO[seq]; !exists {
				// Bound the buffer. Anything dropped here is still tracked by
				// the peer's packet manager and will be retransmitted, so this
				// costs throughput rather than correctness.
				if len(c.recvOOO) < maxConnRecvOOO {
					c.recvOOO[seq] = pkt
				}
			}
		}
		// seq < nextExpectSeq is a duplicate or late arrival; already delivered.
		return nil
	}

	ready := []*Packet{pkt}
	c.nextExpectSeq++
	for {
		next, ok := c.recvOOO[c.nextExpectSeq]
		if !ok {
			break
		}
		delete(c.recvOOO, c.nextExpectSeq)
		ready = append(ready, next)
		c.nextExpectSeq++
	}
	return ready
}

// deliverStreamFrames hands a packet's STREAM frames to their streams. Called
// only for packets released in order by reassemble.
func (c *Connection) deliverStreamFrames(pkt *Packet) {
	for _, frame := range pkt.Frames {
		if sf, ok := frame.(*StreamFrame); ok {
			c.handleStreamFrame(pkt, sf)
		}
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
		// The peer is out of send credit. Re-advertise our current limit without
		// growing the window: the peer is either missing a dropped WINDOW_UPDATE
		// (which this repairs, since the offset is idempotent) or genuinely
		// waiting on our application to consume. Growing here would let a peer
		// inflate our receive buffer just by claiming to be blocked.
		if s := c.findStream(pkt.DestinationStreamID, pkt.SourceStreamID); s != nil {
			if s.streamFC != nil {
				c.sendWindowUpdate(s.ID, s.RemoteID, s.streamFC.RefreshLimit())
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
		s.DeliverData(f.Data)
	}
	if f.IsFin {
		s.DeliverFin()
	}
}

func (c *Connection) handleAckFrame(f *AckFrame) {
	acked := c.pm.HandleAckFrame(f)
	for _, pkt := range acked {
		// Only the largest newly-acked packet yields an RTT sample and drives
		// the window increase (RFC 9002 section 5.1); the rest just release
		// their inflight bytes.
		isLargest := pkt.Sequence == f.LargestAcked
		c.cc.OnPacketAcked(pkt.Size, pkt.SentTime, time.Duration(f.AckDelay)*time.Millisecond,
			isLargest, int(f.LargestAcked))
	}
	if len(acked) > 0 {
		c.sendCredit.Broadcast()
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
		s.OnWindowUpdate(f.WindowSize)
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
