package udx

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage for loss recovery after retransmission stopped abandoning data.
//
// A packet is now re-sent under a FRESH sequence number for as long as the
// connection lives (PacketManager.Retransmit), so loss can no longer strand a
// stream with an unfillable hole and reset it. What ends a path that has gone
// genuinely silent is the idle timeout closing the whole connection — the
// backstop that used to be missing, which is why exhausting a per-packet retry
// count was previously the only thing that surfaced a dead path (as a stream
// reset, or downstream as 40s yamux keepalive timeouts).

// newPacketManagerWithRTT builds a PacketManager whose RTO reflects the given
// smoothed RTT, by feeding the congestion controller a real sample.
func newPacketManagerWithRTT(rtt time.Duration) *PacketManager {
	clk := RealClock{}
	cc := NewCongestionController(clk, func() int { return -1 })
	if rtt > 0 {
		cc.OnPacketAcked(1200, time.Now().Add(-rtt), 0, true, 1)
	}
	return NewPacketManager(clk, cc)
}

// mockPacketManager builds a PacketManager on a controllable clock, so the RTO
// timers never fire on their own and a test can drive retransmission by hand.
func mockPacketManager() (*PacketManager, *MockClock) {
	clk := NewMockClock(time.Now())
	cc := NewCongestionController(clk, func() int { return -1 })
	return NewPacketManager(clk, cc), clk
}

func dataPacket(seq uint32) *SentPacket {
	return &SentPacket{
		Sequence: seq,
		Size:     1200,
		Frames:   []Frame{&StreamFrame{Data: []byte("payload")}},
	}
}

// TestRetransmit_ReKeysUnderAFreshSequence is the heart of the change: a
// retransmission never reuses the original packet number. RFC 9000 section 12.3
// forbids it, and both v3 stacks reassemble on byte offsets, so a fresh number
// is safe and the tracking must follow the packet from the old number to the
// new.
func TestRetransmit_ReKeysUnderAFreshSequence(t *testing.T) {
	pm, _ := mockPacketManager()

	seq := pm.NextSequence()
	pkt := dataPacket(seq)
	pm.SendPacket(pkt)

	newSeq, ok := pm.Retransmit(pkt)
	if !ok {
		t.Fatal("Retransmit returned false for an unacknowledged packet")
	}
	if newSeq == seq {
		t.Fatalf("retransmit reused sequence %d; a fresh number was required", seq)
	}
	if pkt.Sequence != newSeq {
		t.Fatalf("packet carries sequence %d, want the fresh %d", pkt.Sequence, newSeq)
	}
	if pm.GetPacket(seq) != nil {
		t.Fatalf("the original sequence %d is still tracked after the re-key", seq)
	}
	if pm.GetPacket(newSeq) != pkt {
		t.Fatal("the packet is not tracked under its fresh sequence")
	}
	if pkt.RetransmitCount != 1 {
		t.Fatalf("RetransmitCount = %d after one retransmit, want 1", pkt.RetransmitCount)
	}
}

// TestRetransmit_ResetsTheSentTime pins that a retransmission is treated as a
// fresh transmission for timing: its RTT sample and its loss timer both measure
// from the resend, not the original send (a distinct sequence removes the Karn
// ambiguity that would otherwise forbid the sample).
func TestRetransmit_ResetsTheSentTime(t *testing.T) {
	pm, clk := mockPacketManager()

	seq := pm.NextSequence()
	pkt := dataPacket(seq)
	pm.SendPacket(pkt)
	original := pkt.SentTime

	clk.Advance(pm.retransmitTimeout() + time.Second)
	if _, ok := pm.Retransmit(pkt); !ok {
		t.Fatal("Retransmit returned false for an unacknowledged packet")
	}
	if !pkt.SentTime.After(original) {
		t.Fatalf("SentTime %v was not advanced past the original %v", pkt.SentTime, original)
	}
	if pkt.SentTime != clk.Now() {
		t.Fatalf("SentTime = %v, want the resend time %v", pkt.SentTime, clk.Now())
	}
}

// TestRetransmit_CollapsesTriggersWithinAnRTO guards the two triggers — the RTO
// timer and SACK-driven loss detection — from re-sending the same packet twice
// for one loss. A second attempt inside the RTO is dropped; once the RTO has
// elapsed, a genuine re-send proceeds.
func TestRetransmit_CollapsesTriggersWithinAnRTO(t *testing.T) {
	pm, clk := mockPacketManager()

	seq := pm.NextSequence()
	pkt := dataPacket(seq)
	pm.SendPacket(pkt)

	if _, ok := pm.Retransmit(pkt); !ok {
		t.Fatal("the first retransmit should proceed")
	}
	if _, ok := pm.Retransmit(pkt); ok {
		t.Fatal("a second retransmit within the RTO should be collapsed into the first")
	}

	clk.Advance(pm.retransmitTimeout())
	if _, ok := pm.Retransmit(pkt); !ok {
		t.Fatal("after the RTO elapses a real retransmit should proceed")
	}
}

// TestRetransmit_OnAnAckedPacketIsANoop makes sure a retransmit trigger that
// races an ACK does nothing: the packet is gone from tracking, so there is
// nothing to re-send and no timer to leak.
func TestRetransmit_OnAnAckedPacketIsANoop(t *testing.T) {
	pm, _ := mockPacketManager()

	seq := pm.NextSequence()
	pkt := dataPacket(seq)
	pm.SendPacket(pkt)

	pm.HandleAckFrame(&AckFrame{LargestAcked: seq, FirstAckRangeLength: 1})

	if _, ok := pm.Retransmit(pkt); ok {
		t.Fatal("retransmit of an acknowledged packet should be a no-op")
	}
	if len(pm.retransmitTimers) != 0 {
		t.Fatalf("a retransmit timer leaked for an acked packet: %d remain", len(pm.retransmitTimers))
	}
}

// TestRetransmitBackoff_IsCappedAndNeverBelowRTO covers both halves of the cap.
//
// It grows exponentially, saturates rather than running away, and — the part
// that is easy to get wrong — the ceiling never drops below the RTO. Capping
// below the round trip would retransmit before the peer could possibly have
// answered, so a slow path would be answered with a flood of duplicates.
func TestRetransmitBackoff_IsCappedAndNeverBelowRTO(t *testing.T) {
	// There is no per-packet retry cap any more, so the schedule is unbounded;
	// a generous span is enough to prove the shape.
	const attempts = 20

	t.Run("fast path saturates at the cap", func(t *testing.T) {
		pm := newPacketManagerWithRTT(0) // no samples: RTO sits at the 200ms floor
		rto := pm.retransmitTimeout()

		var prev time.Duration
		for attempt := 1; attempt <= attempts; attempt++ {
			got := pm.retransmitBackoff(attempt)
			if got > MaxRetransmitBackoff {
				t.Fatalf("attempt %d: backoff %v exceeds the %v cap", attempt, got, MaxRetransmitBackoff)
			}
			if got < rto {
				t.Fatalf("attempt %d: backoff %v is below the RTO %v", attempt, got, rto)
			}
			if got < prev {
				t.Fatalf("attempt %d: backoff went backwards, %v after %v", attempt, got, prev)
			}
			prev = got
		}
		if prev != MaxRetransmitBackoff {
			t.Fatalf("backoff saturated at %v, want the %v cap", prev, MaxRetransmitBackoff)
		}
	})

	t.Run("slow path never retransmits faster than the round trip", func(t *testing.T) {
		// An RTT well above the cap: the RTO must win, not MaxRetransmitBackoff.
		pm := newPacketManagerWithRTT(3 * time.Second)
		rto := pm.retransmitTimeout()
		if rto <= MaxRetransmitBackoff {
			t.Fatalf("RTO %v did not exceed the cap %v; the test proves nothing", rto, MaxRetransmitBackoff)
		}

		for attempt := 1; attempt <= attempts; attempt++ {
			if got := pm.retransmitBackoff(attempt); got < rto {
				t.Fatalf("attempt %d: backoff %v is shorter than the RTO %v — "+
					"this retransmits before the peer could have answered", attempt, got, rto)
			}
		}
	})
}

// TestIdleTimeout_IsAtLeastThreePTO pins the relationship that keeps loss
// recovery from being cut off: the idle timeout is never shorter than three
// PTOs, and never shorter than the MaxIdleTimeout floor. The RTO is capped, so
// the floor wins today; the max() is what stays correct if the cap ever rises.
func TestIdleTimeout_IsAtLeastThreePTO(t *testing.T) {
	clk := NewMockClock(time.Now())
	c, _, _ := connWithStreamClock(t, clk)

	pto := c.pm.retransmitTimeout()
	if got := c.idleTimeout(); got < 3*pto {
		t.Fatalf("idle timeout %v is below three PTOs (%v)", got, 3*pto)
	}
	if got := c.idleTimeout(); got < MaxIdleTimeout {
		t.Fatalf("idle timeout %v dropped below the %v floor", got, MaxIdleTimeout)
	}
}

// TestIdleTimeout_ClosesADeadConnectionAndWakesReaders is the backstop in
// action: a connection that has received nothing for the idle timeout is closed
// silently, and a reader blocked on one of its streams is woken with an error
// rather than left hanging.
func TestIdleTimeout_ClosesADeadConnectionAndWakesReaders(t *testing.T) {
	clk := NewMockClock(time.Now())
	c, s, _ := connWithStreamClock(t, clk)

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := s.Read(buf)
		readErr <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the reader park on the condition

	// No packet has arrived; step past the idle timeout and run the watchdog.
	clk.Advance(c.idleTimeout() + time.Second)
	if !c.idleExpired() {
		t.Fatal("the connection did not report itself idle after the timeout elapsed")
	}
	c.onIdleCheck()

	if got := c.State(); got != ConnStateClosed {
		t.Fatalf("connection state = %v, want ConnStateClosed", got)
	}
	select {
	case err := <-readErr:
		if !errors.Is(err, ErrStreamReset) {
			t.Fatalf("blocked reader woke with %v, want ErrStreamReset", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle close did not wake the blocked reader")
	}
}

// TestIdleTimeout_ReceivingAPacketKeepsItAlive proves the timer resets on
// receipt: a connection that keeps hearing from the peer is never declared
// idle, however long it runs.
func TestIdleTimeout_ReceivingAPacketKeepsItAlive(t *testing.T) {
	clk := NewMockClock(time.Now())
	c, _, _ := connWithStreamClock(t, clk)

	// Just short of the timeout, a packet arrives...
	clk.Advance(c.idleTimeout() - time.Second)
	c.HandlePacket(&Packet{Version: VersionCurrent})
	// ...and the clock runs almost as far again. Without the reset this would be
	// well past the timeout; with it, the connection is still live.
	clk.Advance(c.idleTimeout() - time.Second)
	if c.idleExpired() {
		t.Fatal("a connection that received a packet mid-window was wrongly declared idle")
	}
}

// TestWriter_OnADeadPathFailsRatherThanHanging is the end-to-end statement over
// real sockets: when the path stops carrying anything, a blocked writer must
// come back with an error in bounded time. It used to hang indefinitely, and
// now the idle timeout closes the connection and wakes the writer.
func TestWriter_OnADeadPathFailsRatherThanHanging(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the idle timeout; skipped under -short")
	}

	var dropping int32
	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	server := NewMultiplexer(serverUDP, RealClock{})
	client := NewMultiplexer(&blackholePacketConn{PacketConn: clientUDP, dropping: &dropping}, RealClock{})
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*MaxIdleTimeout)
	t.Cleanup(cancel)

	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Accept(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Establish the stream while the path still works, then cut it.
	s.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := s.Write([]byte("hello")); err != nil {
		t.Fatalf("initial write over a working path: %v", err)
	}
	atomic.StoreInt32(&dropping, 1)

	// Enough to outrun the congestion window, so the writer really blocks.
	done := make(chan error, 1)
	go func() {
		s.SetWriteDeadline(time.Now().Add(2 * MaxIdleTimeout))
		_, err := s.Write(make([]byte, 1<<20))
		done <- err
	}()

	started := time.Now()
	select {
	case err := <-done:
		elapsed := time.Since(started)
		if !errors.Is(err, ErrStreamReset) {
			t.Fatalf("writer on a dead path returned %v after %v, want ErrStreamReset",
				err, elapsed.Round(time.Second))
		}
		t.Logf("writer failed cleanly after %v via the idle timeout", elapsed.Round(time.Second))
	case <-time.After(MaxIdleTimeout + 20*time.Second):
		t.Fatal("writer never returned; a dead path still hangs the caller")
	}
}

// blackholePacketConn silently discards everything once dropping is set, which
// is what a path that has gone away looks like from the sender: no delivery, no
// acknowledgment, and no error either.
type blackholePacketConn struct {
	net.PacketConn
	dropping *int32
}

func (c *blackholePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if atomic.LoadInt32(c.dropping) == 1 {
		return len(b), nil
	}
	return c.PacketConn.WriteTo(b, addr)
}

// connWithStream builds a connection whose datagrams are counted and discarded,
// with one open stream on it, on the real clock.
func connWithStream(t *testing.T) (*Connection, *Stream, *resetCountingSink) {
	return connWithStreamClock(t, RealClock{})
}

// connWithStreamClock is connWithStream on a caller-supplied clock, so an idle
// test can drive time by hand.
func connWithStreamClock(t *testing.T, clk Clock) (*Connection, *Stream, *resetCountingSink) {
	t.Helper()

	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9}
	local, err := NewConnectionID([]byte{1, 2, 3, 4, 5, 6, 7, 8})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := NewConnectionID([]byte{8, 7, 6, 5, 4, 3, 2, 1})
	if err != nil {
		t.Fatal(err)
	}

	sink := &resetCountingSink{}
	c := NewConnection(local, remote, addr, addr, false, clk, sink.send)
	c.addrValidated = true
	t.Cleanup(func() { c.Close() })

	s := NewStream(4, 2, NewStreamFlowController(1<<20, 1<<20))
	s.conn = c
	s.state = StreamStateOpen
	c.streams[4] = s
	return c, s, sink
}

// resetCountingSink discards datagrams while counting the RESET_STREAM frames
// among them, so a test can assert how many actually went out.
type resetCountingSink struct {
	mu     sync.Mutex
	resets int
}

func (r *resetCountingSink) send(data []byte, _ net.Addr) error {
	pkt, err := UnmarshalPacket(data)
	if err != nil {
		return nil
	}
	for _, f := range pkt.Frames {
		if _, ok := f.(*ResetStreamFrame); ok {
			r.mu.Lock()
			r.resets++
			r.mu.Unlock()
		}
	}
	return nil
}

func (r *resetCountingSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resets
}
