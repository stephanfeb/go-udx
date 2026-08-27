package udx

import (
	"testing"
	"time"
)

// newFCStream builds a stream with a tight, explicit flow-control window so the
// blocking paths are reachable without moving megabytes.
func newFCStream(t *testing.T, initialLimit int64) (*Stream, *mockStreamConn) {
	t.Helper()
	fc := NewStreamFlowController(initialLimit, initialLimit)
	s := NewStream(1, 2, fc)
	mc := &mockStreamConn{clk: NewMockClock(time.Now())}
	s.conn = mc
	s.state = StreamStateOpen
	return s, mc
}

// TestStream_ReadDrivesWindowUpdates pins the direction of the fix: the window
// reopens when the application consumes, not when bytes arrive. Advertising on
// receipt is what let recvBuf grow without bound.
func TestStream_ReadDrivesWindowUpdates(t *testing.T) {
	s, mc := newFCStream(t, 4096)

	// Deliver a full window without reading any of it.
	chunk := make([]byte, 1024)
	for i := 0; i < 4; i++ {
		s.DeliverData(chunk)
	}

	mc.mu.Lock()
	n := len(mc.windowUpdates)
	mc.mu.Unlock()
	if n != 0 {
		t.Fatalf("delivery alone emitted %d window updates; receipt must not reopen the window", n)
	}
	if got := s.streamFC.BufferedBytes(); got != 4096 {
		t.Fatalf("buffered: got %d, want 4096", got)
	}

	// Draining past half the window must emit an update.
	buf := make([]byte, 4096)
	if _, err := s.Read(buf); err != nil {
		t.Fatal(err)
	}

	mc.mu.Lock()
	updates := append([]mockWindowUpdate(nil), mc.windowUpdates...)
	mc.mu.Unlock()

	if len(updates) == 0 {
		t.Fatal("consuming a full window emitted no WINDOW_UPDATE")
	}
	// consumed(4096) + window(8192, doubled) = 12288, an absolute offset.
	if got := int64(updates[0].maxStreamData); got != 12288 {
		t.Fatalf("advertised offset: got %d, want 12288 (consumed + grown window)", got)
	}
	if got := s.streamFC.BufferedBytes(); got != 0 {
		t.Fatalf("buffered after full drain: got %d, want 0", got)
	}
}

// TestStream_WriteRespectsAdvertisedOffset checks the send path never overshoots
// the peer's limit. Write used to test CanSend(1) and then emit a whole MTU
// chunk, exceeding the granted offset by up to a chunk.
func TestStream_WriteRespectsAdvertisedOffset(t *testing.T) {
	// A limit that is not a multiple of the chunk size, so an unclamped write
	// would necessarily overshoot.
	const limit = 3000
	s, mc := newFCStream(t, limit)

	go func() {
		payload := make([]byte, 64*1024)
		s.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		s.Write(payload)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.streamFC.SendWindowAvailable() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mc.mu.Lock()
	sent := 0
	for _, f := range mc.sent {
		sent += len(f.data)
	}
	mc.mu.Unlock()

	if sent > limit {
		t.Fatalf("sent %d bytes past an advertised limit of %d; flow-control violation", sent, limit)
	}
	if sent != limit {
		t.Fatalf("sent %d bytes, want exactly the advertised %d", sent, limit)
	}
}

// TestStream_WriteSendsStreamDataBlocked covers the repair path for a dropped
// WINDOW_UPDATE. Window updates ride seq=0 control packets that the packet
// manager never retransmits, so a stalled writer has to announce itself.
func TestStream_WriteSendsStreamDataBlocked(t *testing.T) {
	s, mc := newFCStream(t, 2048)

	done := make(chan struct{})
	go func() {
		defer close(done)
		payload := make([]byte, 16*1024)
		s.SetWriteDeadline(time.Now().Add(2 * time.Second))
		s.Write(payload)
	}()

	deadline := time.Now().Add(3 * time.Second)
	var blocked []mockBlocked
	for time.Now().Before(deadline) {
		mc.mu.Lock()
		blocked = append([]mockBlocked(nil), mc.blocked...)
		mc.mu.Unlock()
		if len(blocked) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	<-done

	if len(blocked) == 0 {
		t.Fatal("a flow-control-blocked writer sent no STREAM_DATA_BLOCKED; a dropped WINDOW_UPDATE would wedge the stream forever")
	}
	if got := blocked[0].limit; got != 2048 {
		t.Fatalf("STREAM_DATA_BLOCKED offset: got %d, want 2048", got)
	}
}

// TestStream_WriteDeadlineFiresWhenBlocked is a regression for the symptom in
// the bug report. Write checked its deadline only on entry and then waited on a
// condition variable with no timer, so a flow-control stall hung indefinitely
// instead of returning ErrDeadlineExceeded — which is why callers saw a 40s
// yamux keepalive timeout rather than a clean error.
func TestStream_WriteDeadlineFiresWhenBlocked(t *testing.T) {
	s, _ := newFCStream(t, 1024)

	s.SetWriteDeadline(time.Now().Add(150 * time.Millisecond))

	type result struct {
		n   int
		err error
	}
	res := make(chan result, 1)
	go func() {
		n, err := s.Write(make([]byte, 64*1024))
		res <- result{n, err}
	}()

	select {
	case r := <-res:
		if r.err != ErrDeadlineExceeded {
			t.Fatalf("blocked Write returned %v, want ErrDeadlineExceeded", r.err)
		}
		if r.n != 1024 {
			t.Fatalf("wrote %d before blocking, want the full 1024-byte window", r.n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Write ignored its deadline and hung")
	}
}

// TestStream_ReadDeadlineFiresWhenIdle is the same regression on the read side.
func TestStream_ReadDeadlineFiresWhenIdle(t *testing.T) {
	s, _ := newFCStream(t, 1024)

	s.SetReadDeadline(time.Now().Add(150 * time.Millisecond))

	res := make(chan error, 1)
	go func() {
		_, err := s.Read(make([]byte, 1024))
		res <- err
	}()

	select {
	case err := <-res:
		if err != ErrDeadlineExceeded {
			t.Fatalf("idle Read returned %v, want ErrDeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle Read ignored its deadline and hung")
	}
}
