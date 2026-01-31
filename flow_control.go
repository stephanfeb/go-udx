package udx

import "sync"

// FlowController manages two-level flow control: connection and stream level.
type FlowController struct {
	mu sync.Mutex

	// Connection-level flow control
	connMaxData     int64 // Max data the peer allows us to send
	connDataSent    int64
	connDataRecvd   int64
	connRecvWindow  int64 // Window we advertise to peer

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

// StreamFlowController manages per-stream flow control.
type StreamFlowController struct {
	mu sync.Mutex

	maxStreamData   int64 // Max data peer allows on this stream
	dataSent        int64
	dataRecvd       int64
	recvWindow      int64
	blocked         bool
}

// NewStreamFlowController creates a new stream-level flow controller.
func NewStreamFlowController(initialMaxStreamData int64, recvWindow int64) *StreamFlowController {
	return &StreamFlowController{
		maxStreamData: initialMaxStreamData,
		recvWindow:    recvWindow,
	}
}

// CanSend returns true if we can send n bytes on this stream.
func (sfc *StreamFlowController) CanSend(n int) bool {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.dataSent+int64(n) <= sfc.maxStreamData
}

// OnDataSent records bytes sent on this stream.
func (sfc *StreamFlowController) OnDataSent(n int) {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	sfc.dataSent += int64(n)
	sfc.blocked = sfc.dataSent >= sfc.maxStreamData
}

// OnDataReceived records bytes received on this stream.
// Returns true if a window update should be sent.
func (sfc *StreamFlowController) OnDataReceived(n int) bool {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	sfc.dataRecvd += int64(n)
	return sfc.dataRecvd > sfc.recvWindow/4
}

// UpdateMaxStreamData updates the max stream data (from WINDOW_UPDATE frame).
func (sfc *StreamFlowController) UpdateMaxStreamData(max int64) {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	if max > sfc.maxStreamData {
		sfc.maxStreamData = max
		sfc.blocked = false
	}
}

// IsBlocked returns whether the stream is flow-control blocked.
func (sfc *StreamFlowController) IsBlocked() bool {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	return sfc.blocked
}

// ResetReceived resets the received counter (after sending a window update).
func (sfc *StreamFlowController) ResetReceived() {
	sfc.mu.Lock()
	defer sfc.mu.Unlock()
	sfc.dataRecvd = 0
}
