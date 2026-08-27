package udx

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// Netem baseline for the congestion controller.
//
// CUBIC, pacing and RTT sampling only became reachable in d9b1dc7 — before that
// ACKs never reached the controller, so cwnd never left its initial value and no
// RTT sample was ever taken. Every performance number this project produced
// before then described a different system, and all of them were measured on
// loopback with no loss. This measures the live controller under delay, loss and
// reordering, which is where its behaviour actually matters.
//
// Skipped unless UDX_NETEM_BASELINE=1. Intended to run inside the container
// built by interop/Dockerfile.netem, with tc applied to lo. See
// interop/netem-baseline.sh.

type countingPacketConn struct {
	net.PacketConn
	datagrams *int64
	bytes     *int64
	ackOnly   *int64
}

func (c *countingPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	atomic.AddInt64(c.datagrams, 1)
	atomic.AddInt64(c.bytes, int64(len(b)))
	if pkt, err := UnmarshalPacket(b); err == nil && len(pkt.Frames) == 1 {
		if _, ok := pkt.Frames[0].(*AckFrame); ok {
			atomic.AddInt64(c.ackOnly, 1)
		}
	}
	return c.PacketConn.WriteTo(b, addr)
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func TestNetemBaseline(t *testing.T) {
	if os.Getenv("UDX_NETEM_BASELINE") != "1" {
		t.Skip("set UDX_NETEM_BASELINE=1 to run; intended for the netem container")
	}

	payload := envInt("UDX_NETEM_BYTES", 4<<20)
	budget := time.Duration(envInt("UDX_NETEM_TIMEOUT_S", 120)) * time.Second
	label := os.Getenv("UDX_NETEM_LABEL")
	if label == "" {
		label = "unlabelled"
	}

	var datagrams, sentBytes, ackOnly int64

	serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}

	server := NewMultiplexer(&countingPacketConn{serverUDP, &datagrams, &sentBytes, &ackOnly}, RealClock{})
	client := NewMultiplexer(&countingPacketConn{clientUDP, &datagrams, &sentBytes, &ackOnly}, RealClock{})
	defer func() { client.Close(); server.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	conn, err := client.Dial(ctx, server.Addr())
	if err != nil {
		t.Fatal(err)
	}
	srvConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := conn.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	want := make([]byte, payload)
	for i := range want {
		want[i] = byte(i*31 + i/251)
	}

	type readOut struct {
		n   int
		bad bool
		err error
	}
	rc := make(chan readOut, 1)
	go func() {
		s, err := srvConn.AcceptStream(ctx)
		if err != nil {
			rc <- readOut{0, false, err}
			return
		}
		got := make([]byte, 0, payload)
		buf := make([]byte, 256*1024)
		s.SetReadDeadline(time.Now().Add(budget))
		for len(got) < payload {
			n, err := s.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				break
			}
		}
		rc <- readOut{len(got), !bytes.Equal(got, want[:min(len(got), payload)]), nil}
	}()

	start := time.Now()
	stream.SetWriteDeadline(time.Now().Add(budget))
	written, writeErr := stream.Write(want)

	var r readOut
	select {
	case r = <-rc:
	case <-time.After(budget):
		r = readOut{0, false, fmt.Errorf("read timed out")}
	}
	elapsed := time.Since(start)

	dg := atomic.LoadInt64(&datagrams)
	acks := atomic.LoadInt64(&ackOnly)
	wire := atomic.LoadInt64(&sentBytes)

	// Minimum data packets the payload needs, ignoring loss.
	minData := int64(payload)/int64(MaxDatagramSize-100) + 1
	dataDatagrams := dg - acks
	overhead := 0.0
	if minData > 0 {
		overhead = (float64(dataDatagrams)/float64(minData) - 1) * 100
	}

	mbps := 0.0
	if elapsed > 0 {
		mbps = float64(r.n) / elapsed.Seconds() / (1 << 20)
	}

	status := "OK"
	if writeErr != nil || r.err != nil || r.n < payload || r.bad {
		status = "FAIL"
	}

	// One machine-readable line per run for the harness to collect.
	fmt.Printf("NETEM_RESULT label=%q status=%s payload=%d read=%d elapsed_ms=%d mbps=%.2f "+
		"datagrams=%d data_datagrams=%d ack_datagrams=%d wire_bytes=%d retransmit_overhead_pct=%.1f "+
		"cwnd=%d srtt_ms=%.1f minrtt_ms=%.1f inflight=%d recovery=%v\n",
		label, status, payload, r.n, elapsed.Milliseconds(), mbps,
		dg, dataDatagrams, acks, wire, overhead,
		conn.cc.Cwnd(), float64(conn.cc.SmoothedRtt().Microseconds())/1000,
		float64(conn.cc.MinRtt().Microseconds())/1000,
		conn.cc.Inflight(), conn.cc.InRecovery())

	if writeErr != nil {
		t.Fatalf("[%s] write stalled after %d of %d bytes: %v", label, written, payload, writeErr)
	}
	if r.err != nil {
		t.Fatalf("[%s] read failed: %v", label, r.err)
	}
	if r.n < payload {
		t.Fatalf("[%s] transfer incomplete: %d of %d bytes", label, r.n, payload)
	}
	if r.bad {
		t.Fatalf("[%s] payload corrupted", label)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
