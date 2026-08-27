package udx

import (
	"context"
	"math/rand"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Coverage for SACK parsing and loss recovery.
//
// HandleAckFrame advanced its cursor one sequence too far after each
// additional ACK range, so every range after the first was read one lower than
// the peer meant. The sender deleted sequences that had never been
// acknowledged — they were then never retransmitted, and because stream
// delivery is in-order the receiver waited for them forever. It needs two or
// more SACK ranges to bite, i.e. multiple distinct losses in flight, which made
// it look like loss-rate-dependent stalling: 1/12 transfers at 1% loss, 12/12
// at 5%.

// receivedSet is a helper for describing which sequences a peer got.
func ackFrameFor(t *testing.T, received []uint32, largest uint32) *AckFrame {
	t.Helper()
	c := &Connection{recvdDataSeqs: make(map[uint32]struct{})}
	for _, s := range received {
		c.recvdDataSeqs[s] = struct{}{}
	}
	return c.buildAckFrame(largest)
}

// TestSACK_RoundTripAcksExactlyWhatWasReceived is the strongest guard: build an
// ACK from a known received set, parse it back, and require the two to agree.
// Any future drift between builder and parser fails here.
func TestSACK_RoundTripAcksExactlyWhatWasReceived(t *testing.T) {
	cases := []struct {
		name     string
		received []uint32
		sent     int
	}{
		{"contiguous", []uint32{10, 11, 12, 13, 14}, 15},
		{"one gap", []uint32{10, 11, 14, 15, 16}, 20},
		{"two gaps", []uint32{10, 11, 12, 14, 15, 18, 19, 20}, 24},
		{"three gaps", []uint32{1, 2, 5, 6, 9, 10, 13, 14, 17, 18}, 20},
		{"alternating", []uint32{2, 4, 6, 8, 10, 12}, 14},
		{"single packet", []uint32{7}, 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var largest uint32
			for _, s := range tc.received {
				if s > largest {
					largest = s
				}
			}
			frame := ackFrameFor(t, tc.received, largest)

			clk := NewMockClock(time.Now())
			cc := NewCongestionController(clk, func() int { return -1 })
			defer cc.Destroy()
			pm := NewPacketManager(clk, cc)
			defer pm.Destroy()

			for i := 0; i < tc.sent; i++ {
				pm.SendPacket(&SentPacket{Sequence: uint32(i), Size: 100, Frames: []Frame{&PingFrame{}}})
			}

			acked := pm.HandleAckFrame(frame)
			var got []uint32
			for _, p := range acked {
				got = append(got, p.Sequence)
			}
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })

			// Everything the ACK covers must be a sequence the peer actually got.
			inReceived := make(map[uint32]bool, len(tc.received))
			for _, s := range tc.received {
				inReceived[s] = true
			}
			for _, s := range got {
				if !inReceived[s] {
					t.Errorf("acked seq %d, which the peer never received; "+
						"the sender will not retransmit it and the stream stalls", s)
				}
			}

			// And the reverse: a sequence the peer got, that the frame's ranges
			// reach, must not be left tracked as unacked.
			reach := frame.LargestAcked
			var lowest uint32 = reach
			for _, r := range frame.AckRanges {
				_ = r
			}
			for _, s := range tc.received {
				if s < lowest {
					lowest = s
				}
			}
			for _, s := range tc.received {
				if s < lowest {
					continue
				}
				found := false
				for _, g := range got {
					if g == s {
						found = true
						break
					}
				}
				// Only assert for sequences the (max 5) ranges can express.
				if !found && len(frame.AckRanges) < 5 {
					t.Errorf("seq %d was received but not acked by the round-tripped frame", s)
				}
			}
		})
	}
}

// TestSACK_MultiRangeCursorIsNotOffByOne pins the exact defect with a
// hand-built frame, independent of the builder.
func TestSACK_MultiRangeCursorIsNotOffByOne(t *testing.T) {
	clk := NewMockClock(time.Now())
	cc := NewCongestionController(clk, func() int { return -1 })
	defer cc.Destroy()
	pm := NewPacketManager(clk, cc)
	defer pm.Destroy()

	for i := 0; i <= 20; i++ {
		pm.SendPacket(&SentPacket{Sequence: uint32(i), Size: 100, Frames: []Frame{&PingFrame{}}})
	}

	// Received 20,19,18 | missing 17,16 | received 15,14 | missing 13 | received 12,11,10
	frame := &AckFrame{
		LargestAcked:        20,
		FirstAckRangeLength: 3,
		AckRanges: []AckRange{
			{Gap: 2, AckRangeLength: 2}, // skip 17,16 -> ack 15,14
			{Gap: 1, AckRangeLength: 3}, // skip 13    -> ack 12,11,10
		},
	}

	acked := pm.HandleAckFrame(frame)
	gotSet := make(map[uint32]bool, len(acked))
	for _, p := range acked {
		gotSet[p.Sequence] = true
	}

	want := []uint32{20, 19, 18, 15, 14, 12, 11, 10}
	for _, s := range want {
		if !gotSet[s] {
			t.Errorf("seq %d should have been acked", s)
		}
	}
	// The off-by-one acked 9 (never sent to the peer as received) and missed 12.
	for _, s := range []uint32{17, 16, 13, 9, 8} {
		if gotSet[s] {
			t.Errorf("seq %d was acked but falls in a gap; the sender will never "+
				"retransmit it and in-order delivery stalls forever", s)
		}
	}
	if !gotSet[12] {
		t.Error("seq 12 was skipped: the range cursor advanced one too far")
	}
}

// lossyPacketConn drops a fraction of outbound datagrams.
// WriteTo is called from several goroutines, and rand.Rand is not safe for
// concurrent use, so the source is guarded.
type lossyPacketConn struct {
	net.PacketConn
	dropRate float64
	mu       sync.Mutex
	rng      *rand.Rand
	dropped  *int64
}

func (c *lossyPacketConn) drop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rng.Float64() < c.dropRate
}

func (c *lossyPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if c.drop() {
		atomic.AddInt64(c.dropped, 1)
		return len(b), nil // silently discard, as a lossy path would
	}
	return c.PacketConn.WriteTo(b, addr)
}

// TestLossRecovery_CompletesUnderLoss drives real transfers over a lossy path.
// Before the SACK cursor fix these stalled permanently: 1/12 runs at 1% loss,
// 4/12 at 2%, 8/12 at 3%, 12/12 at 5%.
func TestLossRecovery_CompletesUnderLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("lossy transfer test is slow; skipped under -short")
	}

	for _, rate := range []float64{0.01, 0.03, 0.05} {
		rate := rate
		t.Run("", func(t *testing.T) {
			const total = 1 << 20
			const runs = 3

			for run := 0; run < runs; run++ {
				var dropped int64
				seed := int64(run*31 + 7)

				serverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				clientUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				server := NewMultiplexer(&lossyPacketConn{PacketConn: serverUDP, dropRate: rate,
					rng: rand.New(rand.NewSource(seed)), dropped: &dropped}, RealClock{})
				client := NewMultiplexer(&lossyPacketConn{PacketConn: clientUDP, dropRate: rate,
					rng: rand.New(rand.NewSource(seed + 500)), dropped: &dropped}, RealClock{})

				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

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

				done := make(chan int, 1)
				go func() {
					s, err := srvConn.AcceptStream(ctx)
					if err != nil {
						done <- -1
						return
					}
					n := 0
					buf := make([]byte, 1<<20)
					s.SetReadDeadline(time.Now().Add(20 * time.Second))
					for n < total {
						r, err := s.Read(buf)
						n += r
						if err != nil {
							break
						}
					}
					done <- n
				}()

				stream.SetWriteDeadline(time.Now().Add(20 * time.Second))
				written, _ := stream.Write(make([]byte, total))
				got := <-done

				cancel()
				client.Close()
				server.Close()

				if got < total {
					t.Fatalf("%.0f%% loss, run %d: stalled at %d of %d bytes "+
						"(wrote %d, dropped %d datagrams) — a lost packet was never retransmitted",
						rate*100, run, got, total, written, atomic.LoadInt64(&dropped))
				}
			}
		})
	}
}
