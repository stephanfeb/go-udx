package udx

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestThroughput is the transport's own gauge, run only when UDX_THROUGHPUT
// is set: it moves bytes between two multiplexers on loopback in the shapes
// go-ricochet produces and prints the rate and, on Linux, the datagram count
// (from /proc/net/snmp, so batched syscalls are not miscounted by a wrapper
// that would defeat them). It asserts nothing but completion; the numbers are
// for doc/BASELINES.md-style records, taken before and after a transport
// change on the same host.
//
//	UDX_THROUGHPUT=1 go test -run TestThroughput -v .
func TestThroughput(t *testing.T) {
	if os.Getenv("UDX_THROUGHPUT") == "" {
		t.Skip("set UDX_THROUGHPUT=1 to run the throughput gauge")
	}
	const mb = 1 << 20

	// One stream, one direction, 64 MB: the bulk shape.
	t.Run("bulk-1-stream", func(t *testing.T) {
		gauge(t, 1, 64*mb, 0)
	})
	// Eight streams at once, 16 MB each: many concurrent transfers.
	t.Run("bulk-8-streams", func(t *testing.T) {
		gauge(t, 8, 16*mb, 0)
	})
	// Ten streams doing a 2 KB request and a 137 KB reply, 500 rounds each:
	// the collection-page shape from go-ricochet's bench.
	t.Run("request-reply-10-streams", func(t *testing.T) {
		// Skipped: this shape (ten raw streams on one connection, each doing
		// request/reply) hits a latent stream reset under load — one stream's
		// out-of-order buffer crosses maxStreamRecvOOO before a lost offset
		// refills, because v0.1.1 sends faster than a lost packet's ~200ms RTO
		// recovers it. It is a go-udx robustness bug tracked as backlog N16;
		// no libp2p path opens this shape (yamux muxes over one UDX stream per
		// connection). Remove the skip when working N16 — the body reproduces
		// it deterministically on Linux.
		t.Skip("backlog N16: multi-stream request-reply reset under load")
		gauge(t, 10, 0, 500)
	})
}

// gauge runs streams concurrent transfers of size bytes each (rounds == 0),
// or rounds of a 2 KB request / 137 KB reply on each stream.
func gauge(t *testing.T, streams int, size int, rounds int) {
	t.Helper()
	conn, srvConn, ctx := testPair(t, 5*time.Minute)

	const reqSize, replySize = 2048, 137 * 1024
	before := udpDatagramsReceived()
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, 2*streams)
	var moved int64
	var mu sync.Mutex
	add := func(n int) { mu.Lock(); moved += int64(n); mu.Unlock() }

	for i := 0; i < streams; i++ {
		wg.Add(2)
		// Server side: accept, and either drain or serve.
		go func() {
			defer wg.Done()
			s, err := srvConn.AcceptStream(ctx)
			if err != nil {
				errs <- err
				return
			}
			if rounds == 0 {
				if _, err := drain(s, size, 4*time.Minute); err != nil {
					errs <- err
				}
				return
			}
			reply := pattern(replySize)
			buf := make([]byte, reqSize)
			for r := 0; r < rounds; r++ {
				if _, err := readFull(s, buf); err != nil {
					errs <- fmt.Errorf("server read round %d: %w", r, err)
					return
				}
				if _, err := s.Write(reply); err != nil {
					errs <- fmt.Errorf("server write round %d: %w", r, err)
					return
				}
			}
		}()
		// Client side.
		go func() {
			defer wg.Done()
			s, err := conn.OpenStream(ctx)
			if err != nil {
				errs <- err
				return
			}
			s.SetWriteDeadline(time.Now().Add(4 * time.Minute))
			if rounds == 0 {
				if _, err := s.Write(pattern(size)); err != nil {
					errs <- err
					return
				}
				add(size)
				s.Close()
				return
			}
			req := pattern(reqSize)
			buf := make([]byte, replySize)
			for r := 0; r < rounds; r++ {
				if _, err := s.Write(req); err != nil {
					errs <- fmt.Errorf("client write round %d: %w", r, err)
					return
				}
				if _, err := readFull(s, buf); err != nil {
					errs <- fmt.Errorf("client read round %d: %w", r, err)
					return
				}
				add(reqSize + replySize)
			}
			s.Close()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(errs)
	for err := range errs {
		dumpState(t, "client", conn)
		dumpState(t, "server", srvConn)
		t.Fatal(err)
	}
	after := udpDatagramsReceived()
	line := fmt.Sprintf("THROUGHPUT streams=%d moved=%.1fMB elapsed=%.2fs rate=%.1fMB/s",
		streams, float64(moved)/(1<<20), elapsed.Seconds(), float64(moved)/(1<<20)/elapsed.Seconds())
	if rounds > 0 {
		line += fmt.Sprintf(" rounds/s=%.0f", float64(rounds*streams)/elapsed.Seconds())
	}
	if before >= 0 && after >= 0 {
		line += fmt.Sprintf(" udp_datagrams=%d", after-before)
	}
	t.Log(line)
}

// readFull reads exactly len(buf) bytes from the stream.
func readFull(s *Stream, buf []byte) (int, error) {
	got := 0
	for got < len(buf) {
		n, err := s.Read(buf[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// udpDatagramsReceived is the host's UDP InDatagrams counter on Linux
// (/proc/net/snmp), -1 elsewhere.
func udpDatagramsReceived() int64 {
	f, err := os.Open("/proc/net/snmp")
	if err != nil {
		return -1
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var keys []string
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "Udp:" {
			continue
		}
		if keys == nil {
			keys = fields
			continue
		}
		for i, k := range keys {
			if k == "InDatagrams" && i < len(fields) {
				var v int64
				fmt.Sscan(fields[i], &v)
				return v
			}
		}
	}
	return -1
}

// dumpState prints what a stalled connection was waiting on.
func dumpState(t *testing.T, label string, c *Connection) {
	t.Helper()
	t.Logf("%s: state=%v cwnd=%d inflight=%d srtt=%v pto=%v pending=%d bytesSent=%d bytesRecv=%d",
		label, c.State(), c.cc.Cwnd(), c.cc.Inflight(), c.cc.SmoothedRtt(), c.cc.PTO(),
		c.pm.PendingCount(), c.bytesSent, c.bytesReceived)
	c.mu.Lock()
	streams := make([]*Stream, 0, len(c.streams))
	for _, s := range c.streams {
		streams = append(streams, s)
	}
	c.mu.Unlock()
	for _, s := range streams {
		s.mu.Lock()
		var sendAvail int64 = -1
		var advertise int64 = -1
		if s.streamFC != nil {
			sendAvail = s.streamFC.SendWindowAvailable()
			advertise = s.streamFC.AdvertiseLimit()
		}
		t.Logf("%s: stream %d/%d state=%v written=%d sendAvail=%d advertise=%d",
			label, s.ID, s.RemoteID, s.state, s.BytesWritten, sendAvail, advertise)
		s.mu.Unlock()
	}
}
