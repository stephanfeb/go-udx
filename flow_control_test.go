package udx

import "testing"

func TestFlowController_ConnLevel(t *testing.T) {
	fc := NewFlowController(1000, 1000)

	if !fc.CanSendConn(500) {
		t.Fatal("should be able to send 500")
	}

	fc.OnDataSent(500)
	if !fc.CanSendConn(500) {
		t.Fatal("should be able to send another 500")
	}

	fc.OnDataSent(500)
	if fc.CanSendConn(1) {
		t.Fatal("should be blocked at conn level")
	}
	if !fc.IsConnBlocked() {
		t.Fatal("should report blocked")
	}

	// Receive MAX_DATA update
	fc.UpdateMaxData(2000)
	if fc.IsConnBlocked() {
		t.Fatal("should be unblocked after MAX_DATA")
	}
	if !fc.CanSendConn(500) {
		t.Fatal("should be able to send after update")
	}
}

func TestFlowController_WindowUpdate(t *testing.T) {
	fc := NewFlowController(10000, 1000)

	// Receiving data should trigger update when >25% of window
	needsUpdate := fc.OnDataReceived(200)
	if needsUpdate {
		t.Fatal("200 bytes should not trigger update (25% of 1000 = 250)")
	}

	needsUpdate = fc.OnDataReceived(100)
	if !needsUpdate {
		t.Fatal("300 total should trigger update (>250)")
	}

	fc.ResetReceived()
	needsUpdate = fc.OnDataReceived(100)
	if needsUpdate {
		t.Fatal("should not trigger after reset")
	}
}

func TestStreamFlowController(t *testing.T) {
	sfc := NewStreamFlowController(500, 500)

	if !sfc.CanSend(300) {
		t.Fatal("should allow 300")
	}

	sfc.OnDataSent(300)
	if !sfc.CanSend(200) {
		t.Fatal("should allow 200 more")
	}

	sfc.OnDataSent(200)
	if sfc.CanSend(1) {
		t.Fatal("should be blocked")
	}
	if !sfc.IsBlocked() {
		t.Fatal("should report blocked")
	}

	sfc.UpdateMaxStreamData(1000)
	if sfc.IsBlocked() {
		t.Fatal("should be unblocked")
	}
}
