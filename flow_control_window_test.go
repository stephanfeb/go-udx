package udx

import (
	"testing"
)

// Regression coverage for doc/TRANSPORT_WINDOW_BUG.md: a connection stalled
// permanently once roughly 256KB had crossed it. The receiver advertised a
// window *size* while the sender applied it as an absolute limit against a
// lifetime-cumulative dataSent, so the advertised window doubled as a lifetime
// transfer cap. The auto-tuning then diverged and stopped emitting updates
// entirely: each round granted ~W/4 while raising the trigger to (W*1.25)/4.

// TestStreamFC_AdvertisesAbsoluteOffset pins the wire semantics. The value in a
// WINDOW_UPDATE is an absolute offset (consumed + window), never a window size.
func TestStreamFC_AdvertisesAbsoluteOffset(t *testing.T) {
	sfc := NewStreamFlowController(1000, 1000)

	if got := sfc.AdvertisedLimit(); got != 1000 {
		t.Fatalf("initial advertised limit: got %d, want 1000", got)
	}

	// Receiving alone must not move the advertised limit — only consumption does.
	sfc.OnDataReceived(800)
	if got := sfc.AdvertisedLimit(); got != 1000 {
		t.Fatalf("advertised limit after receipt: got %d, want 1000 (unchanged)", got)
	}
	if got := sfc.BufferedBytes(); got != 800 {
		t.Fatalf("buffered: got %d, want 800", got)
	}

	sfc.OnDataConsumed(800)
	limit := sfc.AdvertiseLimit()

	// consumed(800) + window(2000, doubled from 1000) = 2800.
	if want := int64(2800); limit != want {
		t.Fatalf("advertised limit: got %d, want %d (consumed + window)", limit, want)
	}
	if limit <= 1000 {
		t.Fatal("advertised limit did not advance past the initial offset")
	}
}

// TestStreamFC_SustainedHandshakeDoesNotStall is the reduced form of the
// reported bug: drive the real sender and receiver controllers against each
// other and confirm the exchange never wedges. Before the fix this deadlocked
// at ~318KB with the receiver holding 65,856 bytes against a 79,496 threshold.
func TestStreamFC_SustainedHandshakeDoesNotStall(t *testing.T) {
	sender := NewStreamFlowController(int64(InitialMaxStreamData), int64(InitialMaxStreamData))
	receiver := NewStreamFlowController(int64(InitialMaxStreamData), int64(InitialMaxStreamData))

	const chunk = MaxDatagramSize - 100
	const target = int64(64 << 20) // 64 MB, far past the ~256KB ceiling

	var delivered int64
	spins := 0
	for delivered < target {
		if !sender.CanSend(1) {
			// The receiver has nothing outstanding, so if the sender still has
			// no credit no further update can ever arrive.
			spins++
			if spins > 2 {
				t.Fatalf("sender deadlocked after %d bytes (%.1f KB): sendLimit=%d recvWindow=%d advertised=%d",
					delivered, float64(delivered)/1024,
					sender.SendLimit(), receiver.RecvWindow(), receiver.AdvertisedLimit())
			}
			continue
		}
		spins = 0

		n := chunk
		if avail := sender.SendWindowAvailable(); avail < int64(n) {
			n = int(avail)
		}
		sender.OnDataSent(n)
		delivered += int64(n)

		// Receiver takes delivery and the application drains it immediately.
		receiver.OnDataReceived(n)
		if receiver.OnDataConsumed(n) {
			sender.UpdateMaxStreamData(receiver.AdvertiseLimit())
		}
	}

	if delivered < target {
		t.Fatalf("delivered %d, want >= %d", delivered, target)
	}
}

// TestStreamFC_WindowAutoTunesToCap confirms the window grows for a stream that
// actually moves bulk data and then stops at the cap rather than growing without
// bound.
func TestStreamFC_WindowAutoTunesToCap(t *testing.T) {
	sfc := NewStreamFlowController(int64(InitialMaxStreamData), int64(InitialMaxStreamData))

	if got := sfc.RecvWindow(); got != InitialMaxStreamData {
		t.Fatalf("initial window: got %d, want %d", got, InitialMaxStreamData)
	}

	prev := sfc.RecvWindow()
	for i := 0; i < 64; i++ {
		sfc.OnDataReceived(1 << 20)
		sfc.OnDataConsumed(1 << 20)
		sfc.AdvertiseLimit()

		got := sfc.RecvWindow()
		if got < prev {
			t.Fatalf("window shrank: %d -> %d", prev, got)
		}
		if got > MaxStreamRecvWindow {
			t.Fatalf("window %d exceeded cap %d", got, MaxStreamRecvWindow)
		}
		prev = got
	}

	if prev != MaxStreamRecvWindow {
		t.Fatalf("window did not reach cap: got %d, want %d", prev, MaxStreamRecvWindow)
	}
}

// TestStreamFC_UpdateIsMonotonic guards the idempotence property that lets a
// dropped WINDOW_UPDATE be repaired by the next one. Reordered or stale frames
// must never revoke credit already granted.
func TestStreamFC_UpdateIsMonotonic(t *testing.T) {
	sfc := NewStreamFlowController(1000, 1000)

	if !sfc.UpdateMaxStreamData(5000) {
		t.Fatal("update to 5000 should advance the limit")
	}
	if got := sfc.SendLimit(); got != 5000 {
		t.Fatalf("send limit: got %d, want 5000", got)
	}

	// A stale frame arriving late must be ignored, not applied.
	if sfc.UpdateMaxStreamData(2000) {
		t.Fatal("stale update to 2000 should not advance the limit")
	}
	if got := sfc.SendLimit(); got != 5000 {
		t.Fatalf("stale update shrank the limit to %d, want 5000", got)
	}

	// A duplicate of the current limit is also a no-op.
	if sfc.UpdateMaxStreamData(5000) {
		t.Fatal("duplicate update should not report an advance")
	}
}

// TestStreamFC_BackpressureWithoutConsumption verifies the receive window is
// anchored to consumption: a receiver that never drains stops granting credit,
// which is what bounds recvBuf.
func TestStreamFC_BackpressureWithoutConsumption(t *testing.T) {
	sender := NewStreamFlowController(int64(InitialMaxStreamData), int64(InitialMaxStreamData))
	receiver := NewStreamFlowController(int64(InitialMaxStreamData), int64(InitialMaxStreamData))

	// Sender fills the initial window; the application never reads.
	sent := 0
	for sender.CanSend(1) {
		n := 1024
		if avail := sender.SendWindowAvailable(); avail < int64(n) {
			n = int(avail)
		}
		sender.OnDataSent(n)
		receiver.OnDataReceived(n)
		sent += n
	}

	if sent != InitialMaxStreamData {
		t.Fatalf("sent %d before blocking, want exactly %d", sent, InitialMaxStreamData)
	}
	if !sender.IsBlocked() {
		t.Fatal("sender should be flow-control blocked")
	}

	// STREAM_DATA_BLOCKED must not manufacture credit out of nothing.
	before := receiver.RecvWindow()
	if got := receiver.RefreshLimit(); got != int64(InitialMaxStreamData) {
		t.Fatalf("RefreshLimit with nothing consumed: got %d, want %d", got, InitialMaxStreamData)
	}
	if after := receiver.RecvWindow(); after != before {
		t.Fatalf("RefreshLimit grew the window %d -> %d; a peer could inflate our buffer by claiming to be blocked", before, after)
	}
	sender.UpdateMaxStreamData(receiver.RefreshLimit())
	if sender.CanSend(1) {
		t.Fatal("sender got credit without the application consuming anything")
	}

	// Once the application drains, credit reopens.
	receiver.OnDataConsumed(InitialMaxStreamData)
	sender.UpdateMaxStreamData(receiver.AdvertiseLimit())
	if !sender.CanSend(1) {
		t.Fatal("sender still blocked after the receiver consumed the whole window")
	}
}

// TestStreamFC_ClampsToUint32 covers the wire ceiling: WINDOW_UPDATE carries the
// offset in a uint32, so the advertised value must saturate rather than wrap.
// A wrapped offset would hand the peer a limit below what it had already sent.
func TestStreamFC_ClampsToUint32(t *testing.T) {
	sfc := NewStreamFlowController(int64(InitialMaxStreamData), MaxStreamRecvWindow)

	sfc.OnDataReceived(1)
	sfc.OnDataConsumed(1)
	// Jump consumption to just under the ceiling.
	sfc.OnDataConsumed(int(MaxStreamDataOffset - 1))

	limit := sfc.AdvertiseLimit()
	if limit > MaxStreamDataOffset {
		t.Fatalf("advertised %d, exceeds the uint32 wire ceiling %d", limit, MaxStreamDataOffset)
	}
	if limit != MaxStreamDataOffset {
		t.Fatalf("advertised %d, want the clamped ceiling %d", limit, MaxStreamDataOffset)
	}
	if uint32(limit) != uint32(MaxStreamDataOffset) {
		t.Fatalf("advertised offset does not survive the uint32 wire encoding")
	}
}
