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

func TestPMTUD_BinarySearch(t *testing.T) {
	p := NewPMTUDController()

	if p.State() != PMTUDInitial {
		t.Fatal("should start in initial state")
	}
	if p.CurrentMTU() != MinMTU {
		t.Fatalf("initial MTU: got %d, want %d", p.CurrentMTU(), MinMTU)
	}

	// Start probe
	headerSize := 34
	probeSize, ok := p.StartProbe(1, headerSize)
	if !ok {
		t.Fatal("should be able to start probe")
	}
	if probeSize != MinMTU-headerSize {
		t.Fatalf("probe size: got %d, want %d", probeSize, MinMTU-headerSize)
	}
	if p.State() != PMTUDSearching {
		t.Fatal("should be searching")
	}

	// ACK the probe — MTU goes to minMTU, next probe = (1280+1500+1)/2 = 1390
	p.OnProbeAcked(1)
	if p.CurrentMTU() != MinMTU {
		t.Fatalf("MTU after first probe: got %d, want %d", p.CurrentMTU(), MinMTU)
	}
	if p.State() != PMTUDSearching {
		t.Fatal("should still be searching")
	}

	// Continue probing until validated
	for i := uint32(2); p.State() == PMTUDSearching; i++ {
		_, ok := p.StartProbe(i, headerSize)
		if !ok {
			t.Fatal("probe should succeed")
		}
		p.OnProbeAcked(i)
	}

	if p.State() != PMTUDValidated {
		t.Fatal("should be validated")
	}
	if p.CurrentMTU() < MinMTU || p.CurrentMTU() > MaxMTU {
		t.Fatalf("final MTU out of range: %d", p.CurrentMTU())
	}
}

func TestPMTUD_ProbeLost(t *testing.T) {
	p := NewPMTUDController()

	p.StartProbe(1, 34)
	p.OnProbeAcked(1) // Validate minMTU

	// Start next probe and lose it
	p.StartProbe(2, 34)
	p.OnProbeLost(2)

	// Should reduce search range
	if p.CurrentMTU() != MinMTU {
		t.Fatalf("MTU should still be minMTU after probe loss: got %d", p.CurrentMTU())
	}
}
