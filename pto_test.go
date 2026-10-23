package udx

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// The probe timeout (RFC 9002 section 6.2) and what replaces per-packet
// retransmission timers: one PTO per connection that sends a probe or two and
// backs off, loss declared only from acknowledgements, and persistent
// congestion collapsing the window after an outage.

// TestPTO_BacksOffUntilAnAck: each PTO that fires without an ACK doubles the
// wait for the next; an ACK starts the schedule over.
func TestPTO_BacksOffUntilAnAck(t *testing.T) {
	clk := NewMockClock(time.Now())
	cc := NewCongestionController(clk, func() int { return -1 })
	defer cc.Destroy()
	probes := 0
	cc.OnProbe = func() { probes++ }

	cc.OnPacketSent(1200)
	base := cc.PTO()
	if cc.nextPTO() != base {
		t.Fatalf("first PTO waits %v, want the base %v", cc.nextPTO(), base)
	}
	for i := 1; i <= 4; i++ {
		cc.FirePTO()
		if probes != i {
			t.Fatalf("after %d expiries, %d probes were sent", i, probes)
		}
		if got, want := cc.nextPTO(), base<<uint(i); got != want {
			t.Fatalf("after %d expiries the next PTO waits %v, want %v", i, got, want)
		}
	}
	cc.OnPacketAcked(1200, clk.Now(), 0, true, 0)
	// The ACK also gave an RTT sample, so the base moved; the wait is the
	// (new) base again, not a multiple of it.
	if cc.PTOCount() != 0 || cc.nextPTO() != cc.PTO() {
		t.Fatalf("an ACK did not reset the backoff: count %d, next %v, base %v", cc.PTOCount(), cc.nextPTO(), cc.PTO())
	}
}

// TestPTO_IncludesMaxAckDelay: the peer delays its ACKs by up to MaxAckDelay,
// so a PTO without that term would fire on a delayed ACK alone.
func TestPTO_IncludesMaxAckDelay(t *testing.T) {
	cc := NewCongestionController(RealClock{}, func() int { return -1 })
	defer cc.Destroy()
	cc.SetSmoothedRtt(400 * time.Millisecond)
	cc.SetRttVar(50 * time.Millisecond)
	if got, want := cc.PTO(), 400*time.Millisecond+200*time.Millisecond+MaxAckDelay; got != want {
		t.Fatalf("PTO = %v, want srtt + 4 rttvar + max_ack_delay = %v", got, want)
	}
}

// TestPTO_ProbeResendsTheOldestOutstandingDataUnderFreshSequences: a probe
// is the oldest outstanding data again, up to ptoProbes packets, each under
// a fresh sequence; nothing is declared lost and the window is untouched.
func TestPTO_ProbeResendsTheOldestOutstandingDataUnderFreshSequences(t *testing.T) {
	clk := NewMockClock(time.Now())
	c, _, sink := connWithStreamClock(t, clk)

	for i := 0; i < 5; i++ {
		c.sendStreamFrame(4, 2, uint64(i*100), make([]byte, 100), false, false)
	}
	inflight, cwnd := c.cc.Inflight(), c.cc.Cwnd()
	before := sink.datagrams()

	c.cc.FirePTO()

	if got := sink.datagrams() - before; got != ptoProbes {
		t.Fatalf("the PTO sent %d datagrams, want %d probes", got, ptoProbes)
	}
	if c.pm.PendingCount() != 5 {
		t.Fatalf("%d packets tracked after the probe, want all 5 still outstanding", c.pm.PendingCount())
	}
	if c.pm.GetPacket(0) != nil || c.pm.GetPacket(1) != nil {
		t.Fatal("the two oldest packets are still tracked under their old sequences")
	}
	if c.pm.GetPacket(5) == nil || c.pm.GetPacket(6) == nil {
		t.Fatal("the probes are not tracked under fresh sequences 5 and 6")
	}
	if c.cc.Inflight() != inflight || c.cc.Cwnd() != cwnd {
		t.Fatalf("a probe changed inflight %d->%d or cwnd %d->%d", inflight, c.cc.Inflight(), cwnd, c.cc.Cwnd())
	}
	if c.LostQueueLen() != 0 {
		t.Fatal("a probe declared something lost")
	}
}

// TestLoss_ResendsGoThroughTheWindow: packets declared lost leave flight,
// contract the window once, and are re-sent only as the window admits them;
// the rest wait for the next ACK.
func TestLoss_ResendsGoThroughTheWindow(t *testing.T) {
	clk := NewMockClock(time.Now())
	c, _, sink := connWithStreamClock(t, clk)
	// The pacer's timer never fires on this clock. A zero RTT sample (as on
	// loopback) makes the pacing rate unlimited whenever it is recomputed, so
	// the congestion window alone gates the resends.
	c.cc.OnPacketAcked(0, clk.Now(), 0, true, -1)

	// Fill the initial window: 10 packets of 1200 bytes.
	for i := 0; i < 10; i++ {
		c.sendStreamFrame(4, 2, uint64(i*1200), make([]byte, 1200), false, false)
	}
	// Only the last is acknowledged: the first seven are three or more behind
	// and lost; 7 and 8 wait on the loss timer.
	before := sink.datagrams()
	c.HandlePacket(&Packet{Version: VersionCurrent, Frames: []Frame{&AckFrame{LargestAcked: 9, FirstAckRangeLength: 1}}})

	if !c.cc.InRecovery() {
		t.Fatal("loss did not start a recovery epoch")
	}
	resent := sink.datagrams() - before
	queued := c.LostQueueLen()
	if resent+queued != 7 {
		t.Fatalf("%d re-sent + %d queued, want the 7 lost packets", resent, queued)
	}
	if queued == 0 {
		t.Fatalf("all 7 lost packets were re-sent at once into a window of %d bytes", c.cc.Cwnd())
	}
	if c.cc.Inflight() > c.cc.Cwnd() {
		t.Fatalf("inflight %d exceeds cwnd %d after re-sending", c.cc.Inflight(), c.cc.Cwnd())
	}

	// Acknowledging a resend opens the window and more of the queue goes out
	// (the ACK also declares 7 and 8 lost, which join the queue).
	seq := uint32(c.pm.LastSentSeq())
	before = sink.datagrams()
	c.HandlePacket(&Packet{Version: VersionCurrent, Frames: []Frame{&AckFrame{LargestAcked: seq, FirstAckRangeLength: 1}}})
	if sink.datagrams() == before {
		t.Fatalf("an ACK that opened the window re-sent nothing; %d still queued", c.LostQueueLen())
	}
	if c.cc.Inflight() > c.cc.Cwnd() {
		t.Fatalf("inflight %d exceeds cwnd %d after the second round of resends", c.cc.Inflight(), c.cc.Cwnd())
	}
}

// TestLoss_PersistentCongestionCollapsesTheWindow: when the lost packets
// span more than three PTOs with no ACK since the oldest went out, the window
// collapses to the minimum (RFC 9002 section 7.6).
func TestLoss_PersistentCongestionCollapsesTheWindow(t *testing.T) {
	clk := NewMockClock(time.Now())
	c, _, _ := connWithStreamClock(t, clk)
	c.cc.OnPacketAcked(1200, clk.Now().Add(-50*time.Millisecond), 0, true, 0) // an RTT sample

	// Packets sent across four PTOs, none acknowledged.
	for i := 0; i < 4; i++ {
		c.sendStreamFrame(4, 2, uint64(i*1200), make([]byte, 1200), false, false)
		clk.Advance(c.cc.PTO())
	}
	c.sendStreamFrame(4, 2, 4*1200, make([]byte, 1200), false, false)
	if c.cc.Cwnd() <= MinCwnd {
		t.Fatalf("cwnd %d is already at the minimum; the test proves nothing", c.cc.Cwnd())
	}

	// The last one is acknowledged: everything before it is lost, over a span
	// of three PTOs, with no ACK in between.
	last := uint32(c.pm.LastSentSeq())
	c.HandlePacket(&Packet{Version: VersionCurrent, Frames: []Frame{&AckFrame{LargestAcked: last, FirstAckRangeLength: 1}}})

	if c.cc.Cwnd() != MinCwnd {
		t.Fatalf("cwnd = %d after persistent congestion, want the minimum %d", c.cc.Cwnd(), MinCwnd)
	}
	if c.cc.InRecovery() {
		t.Fatal("persistent congestion must end recovery so slow start can restart")
	}
}

// TestPTO_AGapSendsProbesNotTheWindow is the end-to-end statement over real
// sockets: while the path is cut with a window in flight, the sender sends a
// few probes with backoff, not the whole window again and again; and when
// the path returns, the transfer completes.
func TestPTO_AGapSendsProbesNotTheWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out a three-second gap; skipped under -short")
	}
	var dropping int32
	var sent int64
	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := NewMultiplexer(serverUDP, RealClock{})
	client := NewMultiplexer(&countingBlackhole{PacketConn: clientUDP, dropping: &dropping, sent: &sent}, RealClock{})
	t.Cleanup(func() { client.Close(); server.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	srvConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const total = 200 * 1024
	read := make(chan error, 1)
	go func() {
		srvStream, err := srvConn.AcceptStream(ctx)
		if err != nil {
			read <- err
			return
		}
		got, err := drain(srvStream, total, 50*time.Second)
		if err == nil && len(got) != total {
			err = context.DeadlineExceeded
		}
		read <- err
	}()

	// Cut the path as soon as the first bytes are out, so a window is in
	// flight when it goes dark.
	go func() {
		time.Sleep(20 * time.Millisecond)
		atomic.StoreInt32(&dropping, 1)
	}()
	s.SetWriteDeadline(time.Now().Add(50 * time.Second))
	writeDone := make(chan error, 1)
	go func() {
		_, err := s.Write(pattern(total))
		writeDone <- err
	}()

	time.Sleep(200 * time.Millisecond) // the cut is in place and the window is full
	atStart := atomic.LoadInt64(&sent)
	const gap = 3 * time.Second
	time.Sleep(gap)
	during := atomic.LoadInt64(&sent) - atStart
	atomic.StoreInt32(&dropping, 0)

	// On loopback the PTO sits at its floor; expirations in the gap are at
	// 1, 3, 7, 15 PTOs: four at most, each sending ptoProbes datagrams.
	expirations := 0
	for at := MinPTO; at < gap; at = at*2 + MinPTO {
		expirations++
	}
	if limit := int64(ptoProbes*expirations + 2); during > limit {
		t.Fatalf("%d datagrams were sent into a %v gap; want at most %d (%d PTOs x %d probes)",
			during, gap, limit, expirations, ptoProbes)
	}
	t.Logf("%d datagrams sent into the %v gap", during, gap)

	if err := <-writeDone; err != nil {
		t.Fatalf("write after the gap: %v", err)
	}
	if err := <-read; err != nil {
		t.Fatalf("read after the gap: %v", err)
	}
}

// countingBlackhole counts datagrams and discards them while dropping is set.
type countingBlackhole struct {
	net.PacketConn
	dropping *int32
	sent     *int64
}

func (c *countingBlackhole) WriteTo(b []byte, addr net.Addr) (int, error) {
	atomic.AddInt64(c.sent, 1)
	if atomic.LoadInt32(c.dropping) == 1 {
		return len(b), nil
	}
	return c.PacketConn.WriteTo(b, addr)
}
