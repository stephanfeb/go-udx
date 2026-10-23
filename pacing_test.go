package udx

import (
	"context"
	"io"
	"testing"
	"time"
)

// A sender's rate follows its window: slow start runs until the first loss
// and the pacer is told of every change to the window. Over a lossless
// path with a 40 ms round trip (20 ms each way) 4 MB takes under half a
// second. It took two seconds: slow start ended at a 64 KB ssthresh and
// the window crawled up from there at CUBIC's pace, with the sender
// window-bound the whole way.
func TestPacing_RateFollowsTheWindow(t *testing.T) {
	server, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	client, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	const size = 4 << 20
	// The server reads size bytes from each stream and answers one byte.
	go func() {
		sc, err := server.Accept(context.Background())
		if err != nil {
			return
		}
		for {
			s, err := sc.AcceptStream(context.Background())
			if err != nil {
				return
			}
			go func() {
				if _, err := io.ReadFull(s, make([]byte, size)); err == nil {
					s.Write([]byte{1})
				}
			}()
		}
	}()

	nat := newNATProxy(t, server.Addr(), 0, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := client.Dial(ctx, nat.addr())
	if err != nil {
		t.Fatal(err)
	}
	s, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if _, err := s.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	s.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(s, make([]byte, 1)); err != nil {
		t.Fatalf("the transfer did not complete: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("4 MB over a 40 ms path in %v: %.1f MB/s", elapsed.Round(time.Millisecond), float64(size)/elapsed.Seconds()/(1<<20))
	t.Logf("at the end: cwnd %d, srtt %v, pacing rate %.0f B/s, PTO count %d, in recovery %v",
		conn.cc.Cwnd(), conn.cc.SmoothedRtt(), conn.cc.Pacer.PacingRate(), conn.cc.PTOCount(), conn.cc.InRecovery())
	if elapsed > 2*time.Second {
		t.Fatalf("4 MB over a 40 ms path took %v; the pacing rate is not following the window", elapsed)
	}
}

// Until an RTT sample exists nothing is paced: the initial window goes out
// as it is.
func TestPacing_UnpacedBeforeAnRttSample(t *testing.T) {
	clk := NewMockClock(time.Now())
	cc := NewCongestionController(clk, nil)
	for i := 0; i < 50; i++ {
		cc.OnPacketSent(MaxDatagramSize)
		cc.Pacer.OnPacketSent(MaxDatagramSize)
		if d := cc.Pacer.TimeUntilSend(); d != 0 {
			t.Fatalf("paced before any RTT sample: wait %v after %d packets", d, i+1)
		}
	}
}

// The rate follows the window through slow start, at twice cwnd / srtt,
// and drops to 1.25 cwnd / srtt once a loss ends slow start.
func TestPacing_RateTracksTheWindow(t *testing.T) {
	clk := NewMockClock(time.Now())
	cc := NewCongestionController(clk, func() int { return 1000 })
	const rtt = 50 * time.Millisecond

	sent := clk.Now()
	cc.OnPacketSent(MaxDatagramSize)
	clk.Advance(rtt)
	cc.OnPacketAcked(MaxDatagramSize, sent, 0, true, 0)
	if cc.SmoothedRtt() != rtt {
		t.Fatalf("srtt %v, want %v", cc.SmoothedRtt(), rtt)
	}
	for ack := 1; ack <= 20; ack++ {
		want := PacingGainSlowStart * float64(cc.Cwnd()) / rtt.Seconds()
		if got := cc.Pacer.PacingRate(); got != want {
			t.Fatalf("after ack %d (cwnd %d): rate %.0f, want %.0f", ack, cc.Cwnd(), got, want)
		}
		sent := clk.Now()
		cc.OnPacketSent(MaxDatagramSize)
		clk.Advance(rtt)
		cc.OnPacketAcked(MaxDatagramSize, sent, 0, true, ack)
	}
	grown := cc.Cwnd()
	if grown <= InitialCwnd {
		t.Fatalf("the window did not grow: %d", grown)
	}

	cc.OnCongestionEvent()
	if cc.Cwnd() >= grown {
		t.Fatalf("the window did not contract: %d", cc.Cwnd())
	}
	want := PacingGain * float64(cc.Cwnd()) / rtt.Seconds()
	if got := cc.Pacer.PacingRate(); got != want {
		t.Fatalf("after loss (cwnd %d): rate %.0f, want %.0f at the congestion-avoidance gain", cc.Cwnd(), got, want)
	}

	cc.OnPersistentCongestion()
	want = PacingGainSlowStart * float64(MinCwnd) / rtt.Seconds()
	if got := cc.Pacer.PacingRate(); got != want {
		t.Fatalf("after persistent congestion: rate %.0f, want %.0f", got, want)
	}
}

// A burst of PacingBurstPackets goes out without waiting; the next packet
// waits for the budget, which refills at the rate; and over a second of
// sending whenever allowed, the bytes sent are the rate's worth plus the
// burst.
func TestPacing_BurstThenRate(t *testing.T) {
	clk := NewMockClock(time.Now())
	p := NewPacingController(clk)
	const rate = 1 << 20                   // 1 MB/s: a 1472-byte datagram every 1.4 ms
	p.UpdateRate(rate, time.Second, false) // 1.25 * 1 MB / 1 s
	want := 1.25 * rate

	for i := 0; i < PacingBurstPackets; i++ {
		if d := p.TimeUntilSend(); d != 0 {
			t.Fatalf("packet %d of the burst had to wait %v", i+1, d)
		}
		p.OnPacketSent(MaxDatagramSize)
	}
	d := p.TimeUntilSend()
	if d < MinPacingDelay {
		t.Fatalf("after the burst the wait is %v, want at least %v", d, MinPacingDelay)
	}
	clk.Advance(d)
	if d := p.TimeUntilSend(); d != 0 {
		t.Fatalf("the budget did not refill after the wait it asked for: %v more", d)
	}

	// Send whenever allowed for one second.
	sent := 0
	start := clk.Now()
	for clk.Now().Sub(start) < time.Second {
		if d := p.TimeUntilSend(); d > 0 {
			clk.Advance(d)
			continue
		}
		p.OnPacketSent(MaxDatagramSize)
		sent += MaxDatagramSize
	}
	burst := PacingBurstPackets * MaxDatagramSize
	if lo, hi := want*0.95, want*1.05+float64(burst); float64(sent) < lo || float64(sent) > hi {
		t.Fatalf("sent %d bytes in a second at a rate of %.0f: want between %.0f and %.0f", sent, want, lo, hi)
	}
}

// At a high rate the burst is what MinPacingDelay of it is worth, so a
// timer that cannot wait less than that still reaches the rate.
func TestPacing_BurstCoversTheMinimumDelay(t *testing.T) {
	clk := NewMockClock(time.Now())
	p := NewPacingController(clk)
	p.UpdateRate(40<<20, time.Second, false) // 50 MB/s
	burst := p.Budget()
	if byDelay := p.PacingRate() * MinPacingDelay.Seconds(); burst < byDelay {
		t.Fatalf("burst %.0f bytes, want at least %v of the rate, %.0f", burst, MinPacingDelay, byDelay)
	}
	if burst <= float64(PacingBurstPackets*MaxDatagramSize) {
		t.Fatalf("burst %.0f bytes did not grow with the rate", burst)
	}
}

// An overshoot (a batch larger than the budget) is paid for by the wait
// after it: the budget goes negative and the next wait covers the whole
// debt.
func TestPacing_OvershootIsRepaid(t *testing.T) {
	clk := NewMockClock(time.Now())
	p := NewPacingController(clk)
	p.UpdateRate(1<<20, time.Second, false)
	for i := 0; i < 2*PacingBurstPackets; i++ {
		p.OnPacketSent(MaxDatagramSize)
	}
	if b := p.Budget(); b >= 0 {
		t.Fatalf("budget %.0f after an overshoot, want negative", b)
	}
	debt := float64(PacingBurstPackets*MaxDatagramSize) + MaxDatagramSize
	want := time.Duration(debt / p.PacingRate() * float64(time.Second))
	if d := p.TimeUntilSend(); d < want-time.Millisecond || d > want+time.Millisecond {
		t.Fatalf("wait %v after the overshoot, want about %v", d, want)
	}
}

// A zero RTT (loopback, or a test clock that does not move) means unpaced.
func TestPacing_ZeroRttIsUnpaced(t *testing.T) {
	clk := NewMockClock(time.Now())
	p := NewPacingController(clk)
	p.UpdateRate(14720, 0, true)
	for i := 0; i < 100; i++ {
		p.OnPacketSent(MaxDatagramSize)
	}
	if d := p.TimeUntilSend(); d != 0 {
		t.Fatalf("paced with a zero RTT: %v", d)
	}
}
