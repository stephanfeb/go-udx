package udx

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrMaxStreams       = errors.New("max streams exceeded")
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

	localCID   ConnectionID
	remoteCID  ConnectionID
	localAddr  net.Addr
	remoteAddr net.Addr

	state ConnectionState
	clk   Clock

	// Stream management
	streams         map[uint32]*Stream
	nextStreamID    uint32
	isInitiator     bool
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

	// Receive-side acknowledgement state, under ackMu (never taken with mu
	// held, and the timer callback takes it alone). recvd knows which
	// data-bearing packets arrived, for the SACK ranges; ackPending counts
	// those not yet acknowledged; ackTimer flushes them when the threshold
	// is not reached; largestArrival is when the largest one arrived, for
	// the AckDelay the frame reports.
	ackMu          sync.Mutex
	recvd          recvTracker
	ackPending     int
	ackTimer       *time.Timer
	largestArrival time.Time

	// Anti-amplification
	bytesSent     int64
	bytesReceived int64
	addrValidated bool

	// Packet sending. sendBatchFunc, when the multiplexer provides it, sends
	// several datagrams in as few syscalls as the platform allows.
	sendFunc      func(data []byte, addr net.Addr) error
	sendBatchFunc func(bufs [][]byte, addr net.Addr) error

	// inbound is the queue the multiplexer's read loop hands packets to;
	// inboundLoop handles them on this connection's own goroutine.
	inbound        chan *Packet
	inboundDropped int64 // atomic

	// Cleanup callback (set by multiplexer to remove from connection map)
	onClose func()

	// Send-credit signalling: writers park here when the congestion window or
	// pacer is closed, and handleAckFrame wakes them.
	sendCreditMu sync.Mutex
	sendCredit   *sync.Cond

	// Idle timeout (RFC 9000 section 10.1). lastActivity is stamped on every
	// received packet; a self-rearming watchdog closes the connection once it
	// has been silent longer than idleTimeout(). This is the backstop that ends
	// a dead path now that retransmission never gives up on its own.
	lastActivity time.Time
	idleTimer    *time.Timer

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
		localCID:          localCID,
		remoteCID:         remoteCID,
		localAddr:         localAddr,
		remoteAddr:        remoteAddr,
		state:             ConnStateNew,
		clk:               clk,
		streams:           make(map[uint32]*Stream),
		isInitiator:       isInitiator,
		maxStreams:        InitialMaxStreams,
		incomingStreams:   make(chan *Stream, 16),
		pmtud:             NewPMTUDController(),
		pathChallengeResp: make(chan [8]byte, 1),
		sendFunc:          sendFunc,
		inbound:           make(chan *Packet, inboundQueue),
		closeCh:           make(chan struct{}),
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
	// Retransmission re-keys the packet under a fresh sequence inside the packet
	// manager and then calls this to put it on the wire; the callback only
	// marshals and sends. There is no permanent-loss callback any more: a packet
	// is re-sent until acknowledged, and a path that has gone silent is ended by
	// the idle timeout closing the whole connection, not by resetting one stream.
	c.pm.OnRetransmit = c.retransmitPacket
	c.fc = NewFlowController(int64(InitialMaxData), int64(InitialMaxData))

	// Odd stream IDs for initiator, even for responder
	if isInitiator {
		c.nextStreamID = 1
	} else {
		c.nextStreamID = 2
	}

	c.startIdleTimer()

	go c.inboundLoop()
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

// CloseWithError closes the connection with an error code and reason, telling
// the peer with a CONNECTION_CLOSE.
func (c *Connection) CloseWithError(code uint32, reason string) error {
	return c.closeInternal(code, reason, true)
}

// closeInternal tears the connection down once. When announce is true it sends
// a CONNECTION_CLOSE first; the idle path sets it false, because RFC 9000
// section 10.1 closes an idle connection silently — there may be nothing left
// on the path to hear it, and sending into a black hole risks amplification.
// Either way, blocked readers and writers are woken through the stream resets.
func (c *Connection) closeInternal(code uint32, reason string, announce bool) error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.state = ConnStateClosing
		if c.idleTimer != nil {
			c.idleTimer.Stop()
		}
		c.mu.Unlock()
		c.ackMu.Lock()
		if c.ackTimer != nil {
			c.ackTimer.Stop()
			c.ackTimer = nil
		}
		c.ackPending = 0
		c.ackMu.Unlock()

		if announce {
			c.sendFrames([]Frame{&ConnectionCloseFrame{
				ErrorCode:    code,
				ReasonPhrase: reason,
			}})
		}

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

// startIdleTimer stamps the connection live and arms the idle watchdog.
func (c *Connection) startIdleTimer() {
	c.mu.Lock()
	c.lastActivity = c.clk.Now()
	c.mu.Unlock()
	c.armIdleCheck()
}

// idleCheckInterval is how often the watchdog wakes to test for silence. It is
// a poll granularity, not the timeout itself — the timeout is idleTimeout().
const idleCheckInterval = 1 * time.Second

func (c *Connection) armIdleCheck() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closeCh:
		return // already closed; stop rearming
	default:
	}
	c.idleTimer = c.clk.AfterFunc(idleCheckInterval, c.onIdleCheck)
}

func (c *Connection) onIdleCheck() {
	if c.idleExpired() {
		c.closeInternal(ErrorConnectionTimeout, "idle timeout", false)
		return
	}
	c.armIdleCheck()
}

// idleTimeout is the silence a connection tolerates before it is closed. RFC
// 9000 section 10.1 requires at least three PTOs, so loss recovery always gets
// a chance before the path is declared dead. That floor is dynamic: on a slow
// link the retransmission schedule stretches with the RTO and can exceed a
// fixed 30s, so the timeout has to stretch with it rather than cut recovery off.
func (c *Connection) idleTimeout() time.Duration {
	d := MaxIdleTimeout
	if pto := 3 * c.pm.retransmitTimeout(); pto > d {
		d = pto
	}
	return d
}

// idleExpired reports whether the connection has received nothing for longer
// than idleTimeout(). A connection already closing or closed never "expires".
func (c *Connection) idleExpired() bool {
	timeout := c.idleTimeout()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == ConnStateClosing || c.state == ConnStateClosed {
		return false
	}
	return c.clk.Now().Sub(c.lastActivity) >= timeout
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

func (c *Connection) sendStreamFrame(streamID, remoteID uint32, offset uint64, data []byte, isFin, isSyn bool) {
	frame := &StreamFrame{
		IsFin:  isFin,
		IsSyn:  isSyn,
		Offset: offset,
		Data:   data,
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

// retransmitPacket puts a packet back on the wire. PacketManager.Retransmit has
// already advanced pkt.Sequence to a fresh number and re-keyed its tracking, so
// this only marshals and sends — under the new sequence, QUIC-style, never the
// original (RFC 9000 section 12.3).
//
// No OnPacketSent: the bytes were charged to the congestion window at their
// first transmission and remain in flight until acknowledged. A retransmission
// carries the same bytes, so counting it again would double-charge one packet.
func (c *Connection) retransmitPacket(pkt *SentPacket, seq uint32) {
	// seq is passed rather than read from pkt.Sequence: Retransmit assigns it
	// under the packet manager's lock and this send runs unlocked, so reading
	// the field here would race a concurrent re-key of the same packet (an RTO
	// timer firing while the ACK path also retransmits it). The frames and
	// stream IDs are fixed once the packet is first sent, so those are safe.
	rePkt := &Packet{
		Version:             VersionCurrent,
		DestinationCID:      c.remoteCID,
		SourceCID:           c.localCID,
		Sequence:            seq,
		DestinationStreamID: pkt.DestinationStreamID,
		SourceStreamID:      pkt.SourceStreamID,
		Frames:              pkt.Frames,
	}

	data := MarshalPacket(rePkt)

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
// failures). That guarantee comes from the byte offsets: Stream.placeLocked
// discards anything already delivered, so a retransmission is harmless however
// often it arrives.
//
// This used to be a separate (sequence, marshaledSize) dedupe table consulted
// before delivery. It could not be relied on for correctness in either
// direction: it was trimmed above 1000 entries, so a long-delayed duplicate
// went unrecognised, and — worse — a packet recorded here but then dropped for
// want of buffer would have its retransmission suppressed as a duplicate and
// stall the stream permanently. The offsets answer the question exactly, so
// nothing is gained by asking twice.
func (c *Connection) HandlePacket(pkt *Packet) {
	// Packets decoded from the wire carry their datagram length; ones built
	// in-process (tests) are measured. This used to re-encode every packet
	// received just to count its bytes, on the read loop, under the lock.
	size := pkt.wireLen
	if size == 0 {
		size = len(MarshalPacket(pkt))
	}
	c.mu.Lock()
	c.bytesReceived += int64(size)
	// Any packet received is proof the path is alive; restart the idle clock
	// (RFC 9000 section 10.1: the timer resets on receiving and processing a
	// packet).
	c.lastActivity = c.clk.Now()
	c.mu.Unlock()

	// Every frame is handled on arrival. Nothing is queued waiting for an
	// earlier packet: STREAM frames carry their own offset, so a stream places
	// its bytes itself and is held up only by its own gaps, and the other frame
	// types were never ordered to begin with.
	for _, frame := range pkt.Frames {
		c.handleFrame(pkt, frame)
	}

	if !isDataBearing(pkt) {
		return
	}

	// Only data-bearing packets are acknowledged. Acknowledging control-only
	// packets makes every ACK provoke an ACK in return, which ping-pongs
	// without end: a 64KB transfer measured ~176,000 ack-only datagrams for 48
	// data packets before this guard existed. It is also the correct rule
	// independently — sendPacket only registers data-bearing packets with the
	// packet manager, so an ACK for a control packet can never match anything
	// in flight.
	c.noteReceived(pkt)
}

// noteReceived records a data-bearing packet and decides whether to
// acknowledge now or later (RFC 9000 section 13.2). ACKs reflect *receipt*,
// not delivery: a packet buffered ahead of a gap is still safely held, and
// reporting it lets the peer retransmit only what is genuinely missing.
//
// Now: the packet is out of order (the sender should learn of the gap
// without waiting), or it opens or closes a stream (a SYN is what a dial or
// stream open is waiting on; a FIN's ACK lets the sender retire the stream),
// or it is the AckElicitingThreshold-th unacknowledged packet. Later: the
// ACK timer, a quarter of the smoothed RTT within [MinAckDelay, MaxAckDelay],
// and the frame reports how long it waited so the sender's RTT sample stays
// honest. Every packet used to be acknowledged on arrival, which doubled the
// datagrams a receiver handles and, on the server, saturated the single read
// loop that both receives and sends those ACKs.
func (c *Connection) noteReceived(pkt *Packet) {
	seq := pkt.Sequence
	now := c.clk.Now()
	edge := false
	for _, f := range pkt.Frames {
		if sf, ok := f.(*StreamFrame); ok && (sf.IsSyn || sf.IsFin) {
			edge = true
			break
		}
	}

	// Every data-bearing packet is recorded, sequence 0 included: the packet
	// manager numbers from 0, so the first data packet of a connection (the
	// SYN) carries 0, and only control packets (never data-bearing, never
	// acknowledged) reuse it. A guard here against 0 left that first packet
	// unacknowledged once anything else had arrived, and its retransmissions
	// with it.
	c.ackMu.Lock()
	hadAny, before := c.recvd.any, c.recvd.largest
	outOfOrder := c.recvd.add(seq)
	if !hadAny || c.recvd.largest != before {
		c.largestArrival = now
	}
	c.ackPending++
	if outOfOrder || edge || c.ackPending >= AckElicitingThreshold {
		frame := c.takeAckLocked(now)
		c.ackMu.Unlock()
		c.sendPacket(pkt.SourceStreamID, pkt.DestinationStreamID, []Frame{frame})
		return
	}
	if c.ackTimer == nil {
		delay := c.cc.SmoothedRtt() / 4
		if delay < MinAckDelay {
			delay = MinAckDelay
		}
		if delay > MaxAckDelay {
			delay = MaxAckDelay
		}
		src, dst := pkt.SourceStreamID, pkt.DestinationStreamID
		c.ackTimer = c.clk.AfterFunc(delay, func() { c.ackTimerFired(src, dst) })
	}
	c.ackMu.Unlock()
}

// ackTimerFired flushes whatever is pending when the ACK timer elapses.
func (c *Connection) ackTimerFired(srcStreamID, dstStreamID uint32) {
	c.ackMu.Lock()
	c.ackTimer = nil
	if c.ackPending == 0 {
		c.ackMu.Unlock()
		return
	}
	frame := c.takeAckLocked(c.clk.Now())
	c.ackMu.Unlock()
	c.sendPacket(srcStreamID, dstStreamID, []Frame{frame})
}

// takeAckLocked builds the ACK for everything received, clears the pending
// count and disarms the timer. Called with ackMu held.
func (c *Connection) takeAckLocked(now time.Time) *AckFrame {
	c.ackPending = 0
	if c.ackTimer != nil {
		c.ackTimer.Stop()
		c.ackTimer = nil
	}
	var delay uint16
	if !c.largestArrival.IsZero() {
		if d := now.Sub(c.largestArrival) / time.Millisecond; d > 0 {
			if d > 65535 {
				d = 65535
			}
			delay = uint16(d)
		}
	}
	return c.recvd.frame(delay)
}

// buildAckFrame is the frame that would acknowledge everything received so
// far, with latestSeq recorded first. It is the shape tests and the Dart
// parser agree on; the send path goes through noteReceived.
func (c *Connection) buildAckFrame(latestSeq uint32) *AckFrame {
	c.ackMu.Lock()
	defer c.ackMu.Unlock()
	c.recvd.add(latestSeq)
	return c.recvd.frame(0)
}

// enqueue hands a decoded packet to this connection's goroutine. It is what
// the multiplexer's read loop calls, so the loop never runs a connection's
// frame handling or sends. A full queue drops the packet: it is UDP, and
// loss recovery covers a packet the socket could just as well have dropped.
func (c *Connection) enqueue(pkt *Packet) {
	select {
	case c.inbound <- pkt:
	default:
		atomic.AddInt64(&c.inboundDropped, 1)
	}
}

// inboundLoop handles queued packets in arrival order until the connection
// closes.
func (c *Connection) inboundLoop() {
	for {
		select {
		case pkt := <-c.inbound:
			c.HandlePacket(pkt)
		case <-c.closeCh:
			return
		}
	}
}

// InboundDropped is how many packets the read loop dropped for this
// connection because its queue was full.
func (c *Connection) InboundDropped() int64 { return atomic.LoadInt64(&c.inboundDropped) }

// awaitSendCreditUpTo waits like awaitSendCredit for minSize bytes of
// congestion-window and pacing credit, then grants as much as the window
// admits right now, up to maxSize. The bytes are not booked here: the
// caller sends them, and OnPacketSent books each packet.
func (c *Connection) awaitSendCreditUpTo(minSize, maxSize int, deadline time.Time) (int, bool) {
	if !c.awaitSendCredit(minSize, deadline) {
		return 0, false
	}
	granted := c.cc.Available()
	if granted > maxSize {
		granted = maxSize
	}
	if granted < minSize {
		granted = minSize
	}
	return granted, true
}

// sendStreamFrames sends data as consecutive STREAM frames of at most
// chunkSize bytes from offset, isSyn on the first, in one batch write where
// the socket allows (sendBatchFunc). Each packet is sequenced, tracked and
// booked exactly as sendPacket does; only the syscalls are shared. data must
// be the connection's own copy: the frames keep it for retransmission.
func (c *Connection) sendStreamFrames(streamID, remoteID uint32, offset uint64, data []byte, chunkSize int, isSyn bool) {
	bufs := make([][]byte, 0, (len(data)+chunkSize-1)/chunkSize)
	for len(data) > 0 {
		k := chunkSize
		if k > len(data) {
			k = len(data)
		}
		frame := &StreamFrame{IsSyn: isSyn, Offset: offset, Data: data[:k]}
		isSyn = false
		if buf, ok := c.prepareDataPacket(remoteID, streamID, []Frame{frame}); ok {
			bufs = append(bufs, buf)
		}
		offset += uint64(k)
		data = data[k:]
	}
	c.writeDatagrams(bufs)
}

// prepareDataPacket sequences, encodes, tracks and books one data-bearing
// packet and returns its bytes, or false if the anti-amplification limit
// says it may not be sent.
func (c *Connection) prepareDataPacket(dstStreamID, srcStreamID uint32, frames []Frame) ([]byte, bool) {
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
	c.pm.SendPacket(&SentPacket{
		Sequence:            seq,
		Size:                len(data),
		Frames:              frames,
		DestinationStreamID: dstStreamID,
		SourceStreamID:      srcStreamID,
	})
	c.cc.OnPacketSent(len(data))
	c.cc.Pacer.OnPacketSent(len(data))

	c.mu.Lock()
	if !c.addrValidated && c.bytesSent+int64(len(data)) > c.bytesReceived*AmplificationFactor {
		c.mu.Unlock()
		return nil, false // anti-amplification limit
	}
	c.bytesSent += int64(len(data))
	c.mu.Unlock()
	return data, true
}

// writeDatagrams puts encoded packets on the wire, batched where the socket
// allows.
func (c *Connection) writeDatagrams(bufs [][]byte) {
	switch {
	case len(bufs) == 0:
	case c.sendBatchFunc != nil && len(bufs) > 1:
		c.sendBatchFunc(bufs, c.remoteAddr)
	case c.sendFunc != nil:
		for _, b := range bufs {
			c.sendFunc(b, c.remoteAddr)
		}
	}
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
	if !ok && remoteStreamID != 0 && (f.IsSyn || len(f.Data) > 0) {
		// Incoming stream — assign a new local ID, use remote's ID as RemoteID.
		//
		// Data opens the stream too, not just SYN. Requiring SYN was safe only
		// while the connection reordered packets before delivery, which
		// guaranteed the SYN — the lowest sequence number — was seen first.
		// Once frames are handled on arrival a reordered data packet can beat
		// it, and there was nothing to attach the bytes to: they were dropped,
		// having already been acknowledged, so the sender never re-sent them and
		// the stream stalled at that offset forever. Under 25% reordering that
		// killed roughly one transfer in four, always after exactly one packet.
		//
		// A bare FIN or RESET is deliberately not enough. It carries nothing to
		// deliver, so opening a stream for one only invents a stream the
		// application never had.
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
		s.DeliverData(f.Offset, f.Data)
	}
	if f.IsFin {
		// The stream ends one past this frame's last byte, which is the frame's
		// own offset when the FIN carries no data of its own.
		s.DeliverFin(f.Offset + uint64(len(f.Data)))
	}
}

func (c *Connection) handleAckFrame(f *AckFrame) {
	acked := c.pm.HandleAckFrame(f)
	if len(acked) > 0 {
		// One controller update per frame, with every byte the frame newly
		// acknowledges (RFC 9002 section 7.3.1: the window grows by the bytes
		// acked, not per packet). The largest newly-acked packet, if the
		// frame's largest is new, supplies the RTT sample (section 5.1). This
		// used to grow the window by the largest packet's size only, which
		// was invisible while every packet drew its own ACK and would have
		// halved slow start once a receiver acknowledged every second one.
		bytes := 0
		var largest *SentPacket
		for _, pkt := range acked {
			bytes += pkt.Size
			if pkt.Sequence == f.LargestAcked {
				largest = pkt
			}
		}
		var sentTime time.Time
		if largest != nil {
			sentTime = largest.SentTime
		}
		c.cc.OnPacketAcked(bytes, sentTime, time.Duration(f.AckDelay)*time.Millisecond,
			largest != nil, int(f.LargestAcked))
		c.sendCredit.Broadcast()
	}

	// SACK-based loss detection: retransmit packets that fall within gaps.
	//
	// A detected loss both contracts the window (OnCongestionEvent, idempotent
	// within the recovery epoch) and re-sends the packet under a fresh sequence.
	// Retransmit collapses a resend that a firing RTO timer has already covered,
	// so a packet is not sent twice for one loss. The bytes never leave inflight
	// across this — they are being recovered, not abandoned — so there is no
	// OnPacketLost/OnPacketSent pair to balance here.
	lost := c.pm.DetectLostPackets(f)
	for _, seq := range lost {
		if pkt := c.pm.GetPacket(seq); pkt != nil {
			c.cc.OnCongestionEvent()
			if newSeq, ok := c.pm.Retransmit(pkt); ok {
				c.retransmitPacket(pkt, newSeq)
			}
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
