package udx

import (
	"net"
	"testing"
	"time"
)

// Coverage for the send-path wiring uncovered while validating the flow-control
// fix. cwnd, CUBIC and the pacer were all implemented but had no callers, and
// ACKs never reached the congestion controller at all, so none of it ran.

// TestCongestion_AckedPacketsCarrySizeAndTime is the unit-level guard for the
// starvation bug: HandleAckFrame deletes the entry it acknowledges, so callers
// that took a sequence number and asked GetPacket for the details always got
// nil. The congestion controller consequently never saw a single ACK.
func TestCongestion_AckedPacketsCarrySizeAndTime(t *testing.T) {
	clk := NewMockClock(time.Now())
	cc := NewCongestionController(clk, func() int { return -1 })
	defer cc.Destroy()
	pm := NewPacketManager(clk, cc)
	defer pm.Destroy()

	seq := pm.NextSequence()
	pm.SendPacket(&SentPacket{Sequence: seq, Size: 1200, Frames: []Frame{&PingFrame{}}})

	acked := pm.HandleAckFrame(&AckFrame{LargestAcked: seq, FirstAckRangeLength: 1})
	if len(acked) != 1 {
		t.Fatalf("acked %d packets, want 1", len(acked))
	}
	if acked[0].Size != 1200 {
		t.Fatalf("acked packet size: got %d, want 1200", acked[0].Size)
	}
	if acked[0].SentTime.IsZero() {
		t.Fatal("acked packet has no send time; the congestion controller cannot sample RTT without it")
	}
	// The entry is gone from the manager, which is exactly why the size and
	// time have to be returned rather than looked up afterwards.
	if pm.GetPacket(seq) != nil {
		t.Fatal("expected the acked packet to be removed from the manager")
	}
}

// TestCongestion_InflightDrainsOnAck verifies ACKs actually release inflight
// bytes end to end. Without this the window closes permanently after the first
// cwnd's worth of data.
func TestCongestion_InflightDrainsOnAck(t *testing.T) {
	conn, srvConn, ctx := testPair(t, 30*time.Second)

	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const total = 256 * 1024
	done := make(chan error, 1)
	go func() {
		srvStream, err := srvConn.AcceptStream(ctx)
		if err != nil {
			done <- err
			return
		}
		_, err = drain(srvStream, total, 20*time.Second)
		done <- err
	}()

	stream.SetWriteDeadline(time.Now().Add(20 * time.Second))
	if _, err := stream.Write(pattern(total)); err != nil {
		t.Fatalf("write stalled: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Inflight must have come back down; more than a few windows' worth left
	// standing means ACKs are not being credited.
	if inflight := conn.cc.Inflight(); inflight > 4*conn.cc.Cwnd() {
		t.Fatalf("inflight %d still far above cwnd %d after a completed transfer; ACKs are not draining it",
			inflight, conn.cc.Cwnd())
	}
}

// TestCongestion_WindowGrowsUnderLoad checks cwnd actually opens. It was pinned
// at InitialCwnd forever because OnPacketAcked was never called.
func TestCongestion_WindowGrowsUnderLoad(t *testing.T) {
	conn, srvConn, ctx := testPair(t, 60*time.Second)

	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const total = 2 << 20
	done := make(chan error, 1)
	go func() {
		srvStream, err := srvConn.AcceptStream(ctx)
		if err != nil {
			done <- err
			return
		}
		_, err = drain(srvStream, total, 40*time.Second)
		done <- err
	}()

	stream.SetWriteDeadline(time.Now().Add(40 * time.Second))
	if _, err := stream.Write(pattern(total)); err != nil {
		t.Fatalf("write stalled: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if cwnd := conn.cc.Cwnd(); cwnd <= InitialCwnd {
		t.Fatalf("cwnd still %d after 2MB; it never left InitialCwnd (%d), so ACKs are not driving growth",
			cwnd, InitialCwnd)
	}
	if rtt := conn.cc.SmoothedRtt(); rtt == InitialRTT {
		t.Fatalf("smoothed RTT still the %v default after 2MB; no RTT sample was ever taken", InitialRTT)
	}
}

// TestCongestion_ControlPacketsDoNotLeakInflight guards the accounting rule that
// makes cwnd gating viable. ACKs and window updates are never acknowledged, so
// counting them as inflight would ratchet it up until nothing could be sent.
func TestCongestion_ControlPacketsDoNotLeakInflight(t *testing.T) {
	clk := RealClock{}
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}
	localCID, _ := RandomConnectionID(DefaultCIDLength)
	remoteCID, _ := RandomConnectionID(DefaultCIDLength)

	c := NewConnection(localCID, remoteCID, addr, addr, true, clk,
		func(data []byte, addr net.Addr) error { return nil })
	c.mu.Lock()
	c.state = ConnStateEstablished
	c.addrValidated = true
	c.mu.Unlock()
	defer c.Close()

	before := c.cc.Inflight()

	// A burst of pure control packets: ACKs and window updates.
	for i := 0; i < 200; i++ {
		c.sendWindowUpdate(1, 2, 65536)
		c.sendPacket(2, 1, []Frame{&AckFrame{LargestAcked: uint32(i), FirstAckRangeLength: 1}})
	}

	if got := c.cc.Inflight(); got != before {
		t.Fatalf("400 control packets moved inflight from %d to %d; control packets are never acked, "+
			"so counting them ratchets inflight up until the congestion window is permanently closed",
			before, got)
	}
}

// TestCongestion_RetransmitDoesNotDoubleCountInflight covers the other half of
// the accounting rule: a retransmission reuses its original sequence number, so
// the bytes are already counted.
func TestCongestion_RetransmitDoesNotDoubleCountInflight(t *testing.T) {
	clk := RealClock{}
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9999}
	localCID, _ := RandomConnectionID(DefaultCIDLength)
	remoteCID, _ := RandomConnectionID(DefaultCIDLength)

	c := NewConnection(localCID, remoteCID, addr, addr, true, clk,
		func(data []byte, addr net.Addr) error { return nil })
	c.mu.Lock()
	c.state = ConnStateEstablished
	c.addrValidated = true
	c.mu.Unlock()
	defer c.Close()

	c.sendStreamFrame(1, 2, make([]byte, 1000), false, false)
	afterSend := c.cc.Inflight()
	if afterSend == 0 {
		t.Fatal("a data packet did not register any inflight bytes")
	}

	pkt := c.pm.GetPacket(uint32(c.pm.LastSentSeq()))
	if pkt == nil {
		t.Fatal("sent data packet is not tracked by the packet manager")
	}
	for i := 0; i < 5; i++ {
		c.retransmitPacket(pkt)
	}

	if got := c.cc.Inflight(); got != afterSend {
		t.Fatalf("5 retransmissions moved inflight from %d to %d; a retransmit reuses its original "+
			"sequence number, so its bytes must not be charged again", afterSend, got)
	}
}

// TestCongestion_SendPathIsGated confirms a writer is actually paced by the
// congestion window rather than dumping the whole flow-control window into the
// socket at once. An ungated sender drowns itself in loss the moment the stream
// window grows past a few hundred kilobytes.
func TestCongestion_SendPathIsGated(t *testing.T) {
	conn, srvConn, ctx := testPair(t, 30*time.Second)

	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Grant a large send window so flow control is not the limiting factor.
	stream.OnWindowUpdate(8 << 20)

	accepted := make(chan *Stream, 1)
	go func() {
		s, err := srvConn.AcceptStream(ctx)
		if err == nil {
			accepted <- s
		}
	}()

	// Write without anyone reading, then sample inflight. With no gate this
	// races straight to the flow-control limit.
	go func() {
		stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
		stream.Write(make([]byte, 4<<20))
	}()

	deadline := time.Now().Add(2 * time.Second)
	maxInflight := 0
	for time.Now().Before(deadline) {
		if f := conn.cc.Inflight(); f > maxInflight {
			maxInflight = f
		}
		time.Sleep(2 * time.Millisecond)
	}

	select {
	case s := <-accepted:
		s.Reset(ErrorNoError)
	default:
	}

	if maxInflight == 0 {
		t.Fatal("no bytes ever registered inflight")
	}
	// Inflight should track cwnd, not the multi-megabyte flow-control window.
	if limit := 4 * conn.cc.Cwnd(); maxInflight > limit {
		t.Fatalf("inflight peaked at %d with cwnd %d (limit %d); the send path is not gated by the "+
			"congestion window", maxInflight, conn.cc.Cwnd(), limit)
	}
}
