package udx

import "sync"

// FlowController manages connection-level flow control.
//
// NOTE: connection-level flow control is currently accounted but NOT enforced.
// Nothing calls CanSendConn on the send path, and neither MAX_DATA nor
// DATA_BLOCKED is ever emitted, so connMaxData bounds nothing. Enforcing it
// without also implementing the MAX_DATA sender would simply relocate the
// stall that TRANSPORT_WINDOW_BUG.md describes from 256KB to InitialMaxData.
// All live back-pressure runs through StreamFlowController below.
type FlowController struct {
	mu sync.Mutex

	// Connection-level flow control
	connMaxData    int64 // Max data the peer allows us to send
	connDataSent   int64
	connDataRecvd  int64
	connRecvWindow int64 // Window we advertise to peer

	// Whether we're blocked at connection level
	connBlocked bool
}

// NewFlowController creates a new connection-level flow controller.
func NewFlowController(initialMaxData int64, initialRecvWindow int64) *FlowController {
	return &FlowController{
		connMaxData:    initialMaxData,
		connRecvWindow: initialRecvWindow,
	}
}

// CanSendConn returns true if we can send n bytes at the connection level.
func (fc *FlowController) CanSendConn(n int) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.connDataSent+int64(n) <= fc.connMaxData
}

// OnDataSent records bytes sent at connection level.
func (fc *FlowController) OnDataSent(n int) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.connDataSent += int64(n)
	fc.connBlocked = fc.connDataSent >= fc.connMaxData
}

// OnDataReceived records bytes received at connection level.
// Returns true if a window update should be sent.
func (fc *FlowController) OnDataReceived(n int) bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.connDataRecvd += int64(n)
	// Trigger update when received > 25% of advertised window
	return fc.connDataRecvd > fc.connRecvWindow/4
}

// UpdateMaxData updates the peer's max data (from MAX_DATA frame).
func (fc *FlowController) UpdateMaxData(maxData int64) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if maxData > fc.connMaxData {
		fc.connMaxData = maxData
		fc.connBlocked = false
	}
}

// IsConnBlocked returns whether the connection is flow-control blocked.
func (fc *FlowController) IsConnBlocked() bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.connBlocked
}

// ResetReceived resets the received counter (after sending a window update).
func (fc *FlowController) ResetReceived() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.connDataRecvd = 0
}

// ConnMaxData returns the current max data limit.
func (fc *FlowController) ConnMaxData() int64 {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.connMaxData
}

// StreamFlowController manages per-stream flow control using absolute byte
// offsets, following the QUIC MAX_STREAM_DATA model (RFC 9000 section 4.1).
//
// The value carried in a WINDOW_UPDATE frame is an ABSOLUTE OFFSET: the highest
// cumulative byte position the peer may send on this stream. It is *not* a
// window size. This distinction is the entire subject of
// doc/TRANSPORT_WINDOW_BUG.md — the receiver used to advertise a window size
// while the sender compared it against a lifetime-cumulative dataSent, which
// capped every stream's total transfer at roughly 256KB.
//
// Absolute offsets are also idempotent, which matters here: WINDOW_UPDATE rides
// on a seq=0 control packet that the packet manager never retransmits, so a
// dropped frame has to be repairable by the next one. A credit/delta scheme
// (yamux style) would lose that credit permanently.
//
// The advertised offset is anchored to bytes *consumed* by the application
// rather than bytes received, so a reader that stops draining the stream
// applies real back-pressure and bounds recvBuf.
type StreamFlowController struct {
	mu sync.Mutex

	// --- Send side ---
	maxStreamData int64 // absolute offset limit granted by the peer
	dataSent      int64 // cumulative bytes sent on this stream
	blocked       bool

	// --- Receive side ---
	dataRecvd      int64 // cumulative bytes received
	dataConsumed   int64 // cumulative bytes handed to the application by Read
	recvWindow     int64 // current window size, auto-tuned up to MaxStreamRecvWindow
	lastAdvertised int64 // absolute offset last advertised to the peer
}

// NewStreamFlowController creates a new stream-level flow controller.
// initialMaxStreamData is the offset limit we assume the peer has granted us
// before any WINDOW_UPDATE arrives; recvWindow is the window we advertise.
// Both peers must start from the same value for the initial credit to line up.
func NewStreamFlowController(initialMaxStreamData int64, recvWindow int64) *StreamFlowController {
	return &StreamFlowController{
		maxStreamData:  initialMaxStreamData,
		recvWindow:     recvWindow,
		lastAdvertised: recvWindow, // dataConsumed is 0, so the limit is the window
	}
}

// --- Send side ---

// CanSend returns true if we can send n more bytes on this stream.
func (sfc *StreamFlowController) CanSend(n int) bool {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.dataSent+int64(n) <= sfc.maxStreamData
}

// SendWindowAvailable returns how many more bytes may be sent before the
// peer's advertised limit is reached. Callers should clamp writes to this so
// a chunk never overshoots the granted offset.
func (sfc *StreamFlowController) SendWindowAvailable() int64 {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	avail := sfc.maxStreamData - sfc.dataSent
	if avail < 0 {
		return 0
	}
	return avail
}

// SendLimit returns the absolute offset limit currently granted by the peer.
func (sfc *StreamFlowController) SendLimit() int64 {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.maxStreamData
}

// OnDataSent records bytes sent on this stream.
func (sfc *StreamFlowController) OnDataSent(n int) {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	sfc.dataSent += int64(n)
	sfc.blocked = sfc.dataSent >= sfc.maxStreamData
}

// UpdateMaxStreamData applies a WINDOW_UPDATE from the peer. The argument is an
// absolute offset. Updates are monotonic, so a stale or reordered frame can
// never shrink the limit. Returns true if the limit advanced.
func (sfc *StreamFlowController) UpdateMaxStreamData(max int64) bool {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	if max <= sfc.maxStreamData {
		return false
	}
	sfc.maxStreamData = max
	sfc.blocked = sfc.dataSent >= sfc.maxStreamData
	return true
}

// IsBlocked returns whether the stream is flow-control blocked.
func (sfc *StreamFlowController) IsBlocked() bool {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.blocked
}

// --- Receive side ---

// OnDataReceived records bytes received on this stream. Receipt alone never
// triggers a WINDOW_UPDATE — only consumption does, via OnDataConsumed.
func (sfc *StreamFlowController) OnDataReceived(n int) {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	sfc.dataRecvd += int64(n)
}

// OnDataConsumed records bytes handed to the application and reports whether a
// WINDOW_UPDATE should now be sent.
//
// The trigger fires once the peer's remaining credit under our last
// advertisement falls below half the window. This is stable: each update grants
// at least recvWindow/2 fresh credit and the next threshold is measured against
// the same window, so the grant never falls behind the threshold. The previous
// scheme granted ~W/4 while raising the threshold to (W*1.25)/4, which
// diverged and wedged the stream after about seven updates.
func (sfc *StreamFlowController) OnDataConsumed(n int) bool {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	sfc.dataConsumed += int64(n)
	remaining := sfc.lastAdvertised - sfc.dataConsumed
	return remaining < sfc.recvWindow/2
}

// AdvertiseLimit auto-tunes the receive window upward and returns the absolute
// offset to advertise in a WINDOW_UPDATE.
//
// The window doubles on every update up to MaxStreamRecvWindow. Because an
// update only fires after the application has drained half a window, a stream
// must genuinely move ~MaxStreamRecvWindow bytes before earning the largest
// window — a trickling stream stays small on its own.
func (sfc *StreamFlowController) AdvertiseLimit() int64 {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()

	if sfc.recvWindow < MaxStreamRecvWindow {
		sfc.recvWindow *= 2
		if sfc.recvWindow > MaxStreamRecvWindow {
			sfc.recvWindow = MaxStreamRecvWindow
		}
	}
	return sfc.advertiseLocked()
}

// RefreshLimit recomputes the advertised offset from current consumption
// WITHOUT growing the window, and returns it. Used to answer a
// STREAM_DATA_BLOCKED, where the peer is stalled either because a WINDOW_UPDATE
// was dropped or because it has not yet seen our latest one. Growing here would
// let a peer inflate our buffer just by claiming to be blocked.
func (sfc *StreamFlowController) RefreshLimit() int64 {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.advertiseLocked()
}

// advertiseLocked computes and records the advertised offset. Caller holds mu.
func (sfc *StreamFlowController) advertiseLocked() int64 {
	limit := sfc.dataConsumed + sfc.recvWindow

	// WINDOW_UPDATE carries the offset in a uint32, so it cannot express more
	// than 4GB on a single stream. Clamp rather than wrap: a clamped stream
	// stalls at the ceiling, whereas a wrapped one would hand the peer a limit
	// below what it has already sent and corrupt the accounting.
	if limit > MaxStreamDataOffset {
		limit = MaxStreamDataOffset
	}
	if limit > sfc.lastAdvertised {
		sfc.lastAdvertised = limit
	}
	return sfc.lastAdvertised
}

// AdvertisedLimit returns the offset last advertised to the peer.
func (sfc *StreamFlowController) AdvertisedLimit() int64 {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.lastAdvertised
}

// RecvWindow returns the current auto-tuned receive window size.
func (sfc *StreamFlowController) RecvWindow() int64 {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.recvWindow
}

// BufferedBytes returns bytes received but not yet consumed by the application.
func (sfc *StreamFlowController) BufferedBytes() int64 {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.dataRecvd - sfc.dataConsumed
}
