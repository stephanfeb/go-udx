package udx

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// A path that carries full Ethernet frames is found in one probe.
func TestMTU_FullSizePathValidatesInOneProbe(t *testing.T) {
	m := NewMTUDiscovery(NewMockClock(time.Now()))
	if m.Current() != SafeDatagramSize {
		t.Fatalf("starts at %d, want the base %d", m.Current(), SafeDatagramSize)
	}
	if size := m.NextProbe(); size != MaxDatagramSize {
		t.Fatalf("first probe %d, want %d", size, MaxDatagramSize)
	}
	if size := m.NextProbe(); size != 0 {
		t.Fatalf("a second probe %d while one is in flight", size)
	}
	m.OnProbeAcked(MaxDatagramSize)
	if m.Current() != MaxDatagramSize || !m.Done() {
		t.Fatalf("after the probe was acknowledged: current %d, done %v", m.Current(), m.Done())
	}
}

// On a path that carries 1322 bytes (a 1350-byte tunnel), the search settles
// within MTUSearchStep under it, ruling each failing size out after
// MTUProbeTries losses.
func TestMTU_SmallPathConverges(t *testing.T) {
	m := NewMTUDiscovery(NewMockClock(time.Now()))
	const path = 1350 - 28
	probes := 0
	for {
		size := m.NextProbe()
		if size == 0 {
			break
		}
		probes++
		if size <= path {
			m.OnProbeAcked(size)
		} else {
			m.OnProbeLost(size)
		}
		if probes > 40 {
			t.Fatal("the search does not end")
		}
	}
	if m.Current() > path || m.Current() <= path-MTUSearchStep {
		t.Fatalf("settled at %d on a %d-byte path", m.Current(), path)
	}
	if m.Current() <= SafeDatagramSize {
		t.Fatalf("settled at the base %d; the path carries %d", m.Current(), path)
	}
	t.Logf("%d probes settled at %d on a %d-byte path", probes, m.Current(), path)
}

// One loss does not rule a size out: it is tried MTUProbeTries times.
func TestMTU_ASizeIsRuledOutAfterRepeatedLoss(t *testing.T) {
	m := NewMTUDiscovery(NewMockClock(time.Now()))
	for i := 1; i <= MTUProbeTries; i++ {
		if size := m.NextProbe(); size != MaxDatagramSize {
			t.Fatalf("try %d: probe %d, want %d", i, size, MaxDatagramSize)
		}
		m.OnProbeLost(MaxDatagramSize)
	}
	if size := m.NextProbe(); size >= MaxDatagramSize || size <= SafeDatagramSize {
		t.Fatalf("after %d losses the next probe is %d; want between the base and %d", MTUProbeTries, size, MaxDatagramSize)
	}
}

// Full-size data packets lost in a row, with none acknowledged between,
// are a black hole: the size falls back to the base, and the search starts
// over after BlackholeRetry.
func TestMTU_BlackholeFallsBackAndRetries(t *testing.T) {
	clk := NewMockClock(time.Now())
	m := NewMTUDiscovery(clk)
	m.NextProbe()
	m.OnProbeAcked(MaxDatagramSize)

	for i := 1; i < BlackholePackets; i++ {
		m.OnDataLost(MaxDatagramSize)
	}
	m.OnDataAcked(MaxDatagramSize) // one got through: the count starts over
	for i := 1; i < BlackholePackets; i++ {
		m.OnDataLost(MaxDatagramSize)
	}
	if m.Current() != MaxDatagramSize {
		t.Fatalf("fell back after %d losses with an ACK between", BlackholePackets-1)
	}
	m.OnDataLost(SafeDatagramSize) // a base-size loss says nothing about the size
	if m.Current() != MaxDatagramSize {
		t.Fatal("a base-size loss counted towards the black hole")
	}
	m.OnDataLost(MaxDatagramSize)
	if m.Current() != SafeDatagramSize || !m.Done() {
		t.Fatalf("after %d full-size losses in a row: current %d, done %v", BlackholePackets, m.Current(), m.Done())
	}
	if size := m.NextProbe(); size != 0 {
		t.Fatalf("probing %d right after a black hole", size)
	}
	clk.Advance(BlackholeRetry)
	if size := m.NextProbe(); size != MaxDatagramSize {
		t.Fatalf("after %v the search did not restart: probe %d", BlackholeRetry, size)
	}
}

// Blackhole, as the connection calls it after BlackholePTOs probe timeouts,
// does the same at once; a stale probe's outcome is ignored after it.
func TestMTU_BlackholeFromProbeTimeouts(t *testing.T) {
	clk := NewMockClock(time.Now())
	m := NewMTUDiscovery(clk)
	m.NextProbe()
	m.OnProbeAcked(MaxDatagramSize)
	m.Blackhole()
	if m.Current() != SafeDatagramSize {
		t.Fatalf("current %d after a black hole", m.Current())
	}
	m.OnProbeAcked(MaxDatagramSize)
	if m.Current() != SafeDatagramSize {
		t.Fatal("a stale probe ACK raised the size after a black hole")
	}
	m.Reset()
	if m.Current() != SafeDatagramSize || m.NextProbe() != MaxDatagramSize {
		t.Fatal("Reset did not start the search over")
	}
}

// mtuPair is a client and server connected through nat, with the client's
// datagrams counted; the server reads what each stream sends and answers a
// byte so the client knows it arrived.
func mtuPair(t *testing.T, nat *natProxy, server *Multiplexer, sent *int64) *Connection {
	t.Helper()
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	var dropping int32
	var cutAt int64
	client := NewMultiplexer(&countingBlackhole{PacketConn: clientUDP, dropping: &dropping, sent: sent, cutAt: &cutAt}, RealClock{})
	t.Cleanup(func() { client.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	conn, err := client.Dial(ctx, nat.addr())
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// mtuServer answers each stream's first size bytes with one byte.
func mtuServer(t *testing.T, size int) *Multiplexer {
	t.Helper()
	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	server := NewMultiplexer(serverUDP, RealClock{})
	t.Cleanup(func() { server.Close() })
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
	return server
}

// transfer writes size bytes on a new stream and waits for the server's
// byte back.
func transfer(t *testing.T, conn *Connection, size int, within time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	s, err := conn.OpenStream(ctx)
	if err != nil {
		return err
	}
	s.SetWriteDeadline(time.Now().Add(within))
	if _, err := s.Write(make([]byte, size)); err != nil {
		return err
	}
	s.SetReadDeadline(time.Now().Add(within))
	_, err = io.ReadFull(s, make([]byte, 1))
	return err
}

// waitForSize waits for the validated datagram size to reach atLeast.
// Probing follows the ACKs, so a search that has not ended when the
// traffic stops (its last probe lost, which only the next ACK reveals)
// resumes with the next transfer; what matters is the size it reached.
func waitForSize(t *testing.T, conn *Connection, atLeast int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for conn.MaxDatagram() < atLeast {
		if time.Now().After(deadline) {
			t.Fatalf("the datagram size reached %d, want at least %d", conn.MaxDatagram(), atLeast)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Over loopback the search validates full-size datagrams during the first
// transfer, and the next transfer goes out in them.
func TestMTU_LoopbackReachesFullSize(t *testing.T) {
	const size = 64 * 1024
	var sent int64
	server := mtuServer(t, size)
	nat := newNATProxy(t, server.Addr(), 0, 0)
	conn := mtuPair(t, nat, server, &sent)

	if err := transfer(t, conn, size, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitForSize(t, conn, MaxDatagramSize)

	before := atomic.LoadInt64(&sent)
	if err := transfer(t, conn, size, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	datagrams := atomic.LoadInt64(&sent) - before
	atMost := int64(size/(MaxDatagramSize-streamChunkOverhead)) + 4
	if datagrams > atMost {
		t.Fatalf("%d datagrams for %d bytes at a %d-byte datagram size; want at most %d", datagrams, size, conn.MaxDatagram(), atMost)
	}
	t.Logf("%d datagrams for %d bytes", datagrams, size)
}

// Over a path that carries less than a full frame the search settles
// under what it carries, and above the base.
func TestMTU_SmallPathSettlesUnderIt(t *testing.T) {
	const size = 512 * 1024
	const path = 1350 - 28
	var sent int64
	server := mtuServer(t, size)
	nat := newNATProxy(t, server.Addr(), path, 0)
	conn := mtuPair(t, nat, server, &sent)

	if err := transfer(t, conn, size, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	waitForSize(t, conn, path-MTUSearchStep+1)
	if got := conn.MaxDatagram(); got > path {
		t.Fatalf("settled at %d on a %d-byte path", got, path)
	}
	t.Logf("settled at %d on a %d-byte path", conn.MaxDatagram(), path)
}

// A path that shrinks under a validated size (a carrier's tunnel after a
// handover) drops every full-size packet: the connection falls back to
// the base, re-chunks what it re-sends, and the transfer completes. Here
// the write's last chunk is short, crosses, and its ACK reveals the loss
// of the rest (BlackholePackets); the probe-timeout path is
// TestMTU_ProbeTimeoutsFallBackAndRechunk.
func TestMTU_BlackholeFallsBackToTheBase(t *testing.T) {
	const size = 64 * 1024
	var sent int64
	server := mtuServer(t, size)
	nat := newNATProxy(t, server.Addr(), 0, 0)
	conn := mtuPair(t, nat, server, &sent)

	if err := transfer(t, conn, size, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitForSize(t, conn, MaxDatagramSize)

	nat.limit(1350 - 28)
	start := time.Now()
	if err := transfer(t, conn, size, 15*time.Second); err != nil {
		t.Fatalf("the transfer after the path shrank: %v (size now %d)", err, conn.MaxDatagram())
	}
	if conn.MaxDatagram() != SafeDatagramSize {
		t.Fatalf("size %d after the path shrank, want the base %d", conn.MaxDatagram(), SafeDatagramSize)
	}
	t.Logf("recovered in %v", time.Since(start).Round(time.Millisecond))
}

// With nothing crossing and nothing acknowledged, BlackholePTOs probe
// timeouts in a row take the size back to the base, and that PTO's probe
// goes out re-chunked to fit: the first packets that can cross.
func TestMTU_ProbeTimeoutsFallBackAndRechunk(t *testing.T) {
	clk := NewMockClock(time.Now())
	c, _, sink := connWithStreamClock(t, clk)
	c.mtu.NextProbe()
	c.mtu.OnProbeAcked(MaxDatagramSize)

	data := pattern(MaxDatagramSize - streamChunkOverhead)
	c.sendStreamFrame(4, 2, 0, data, false, false)
	raw := sink.sentRaw()
	if full := len(raw[len(raw)-1]); full <= SafeDatagramSize {
		t.Fatalf("the packet went out at %d bytes; the test needs one larger than the base", full)
	}

	for i := 1; i < BlackholePTOs; i++ {
		c.cc.FirePTO()
	}
	if c.MaxDatagram() != MaxDatagramSize {
		t.Fatalf("fell back after %d probe timeouts", BlackholePTOs-1)
	}
	before := len(sink.sentRaw())
	c.cc.FirePTO()
	if c.MaxDatagram() != SafeDatagramSize {
		t.Fatalf("size %d after %d probe timeouts, want the base", c.MaxDatagram(), BlackholePTOs)
	}
	pieces := sink.sentRaw()[before:]
	if len(pieces) < 2 {
		t.Fatalf("the probe went out as %d datagram(s); want the packet re-chunked", len(pieces))
	}
	var got []byte
	for _, d := range pieces {
		if len(d) > SafeDatagramSize {
			t.Fatalf("a %d-byte datagram after the fallback", len(d))
		}
		pkt, err := UnmarshalPacket(d)
		if err != nil {
			t.Fatal(err)
		}
		sf := pkt.Frames[0].(*StreamFrame)
		if int(sf.Offset) != len(got) {
			t.Fatalf("piece at offset %d, want %d", sf.Offset, len(got))
		}
		got = append(got, sf.Data...)
	}
	if string(got) != string(data) {
		t.Fatalf("the pieces carry %d bytes, want the packet's %d", len(got), len(data))
	}
	if n := c.pm.PendingCount(); n != len(pieces) {
		t.Fatalf("%d packets tracked after the re-chunking, want the %d pieces", n, len(pieces))
	}
}
