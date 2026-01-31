package udx

import (
	"io"
	"sync"
	"testing"
	"time"
)

// mockStreamConn implements streamConn for testing.
type mockStreamConn struct {
	mu      sync.Mutex
	sent    []mockSentFrame
	resets  []mockReset
	clk     Clock
}

type mockSentFrame struct {
	streamID, remoteID uint32
	data               []byte
	isFin, isSyn       bool
}

type mockReset struct {
	streamID, remoteID uint32
	errorCode          uint32
}

func (m *mockStreamConn) sendStreamFrame(streamID, remoteID uint32, data []byte, isFin, isSyn bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := make([]byte, len(data))
	copy(d, data)
	m.sent = append(m.sent, mockSentFrame{streamID, remoteID, d, isFin, isSyn})
}

func (m *mockStreamConn) sendResetStream(streamID, remoteID uint32, errorCode uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resets = append(m.resets, mockReset{streamID, remoteID, errorCode})
}

func (m *mockStreamConn) clock() Clock { return m.clk }

func newTestStream(t *testing.T) (*Stream, *mockStreamConn) {
	t.Helper()
	fc := NewStreamFlowController(1<<20, 1<<20)
	s := NewStream(1, 2, fc)
	mc := &mockStreamConn{clk: NewMockClock(time.Now())}
	s.conn = mc
	s.state = StreamStateOpen
	return s, mc
}

func TestStream_WriteAndSend(t *testing.T) {
	s, mc := newTestStream(t)

	n, err := s.Write([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("wrote %d, want 5", n)
	}

	mc.mu.Lock()
	if len(mc.sent) != 1 {
		t.Fatalf("sent %d frames, want 1", len(mc.sent))
	}
	if string(mc.sent[0].data) != "hello" {
		t.Fatalf("sent data: got %q", mc.sent[0].data)
	}
	mc.mu.Unlock()
}

func TestStream_ReadDeliverData(t *testing.T) {
	s, _ := newTestStream(t)

	// Deliver data
	s.DeliverData(0, []byte("world"))

	buf := make([]byte, 10)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "world" {
		t.Fatalf("read: got %q", buf[:n])
	}
}

func TestStream_OutOfOrderDelivery(t *testing.T) {
	s, _ := newTestStream(t)

	// Deliver seq 1 before seq 0
	s.DeliverData(1, []byte("B"))
	s.DeliverData(0, []byte("A"))

	buf := make([]byte, 10)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "AB" {
		t.Fatalf("read: got %q, want AB", buf[:n])
	}
}

func TestStream_FIN(t *testing.T) {
	s, mc := newTestStream(t)

	err := s.Close()
	if err != nil {
		t.Fatal(err)
	}

	mc.mu.Lock()
	if len(mc.sent) != 1 || !mc.sent[0].isFin {
		t.Fatal("expected FIN frame")
	}
	mc.mu.Unlock()

	if s.State() != StreamStateHalfClosedLocal {
		t.Fatalf("state: got %d, want HalfClosedLocal", s.State())
	}

	// Deliver remote FIN
	s.DeliverFin()
	if s.State() != StreamStateClosed {
		t.Fatalf("state: got %d, want Closed", s.State())
	}
}

func TestStream_ReadAfterFIN(t *testing.T) {
	s, _ := newTestStream(t)

	s.DeliverData(0, []byte("data"))
	s.DeliverFin()

	buf := make([]byte, 10)
	n, err := s.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != "data" {
		t.Fatalf("read: got %q", buf[:n])
	}

	// Next read should return EOF
	_, err = s.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestStream_Reset(t *testing.T) {
	s, mc := newTestStream(t)

	err := s.Reset(ErrorInternalError)
	if err != nil {
		t.Fatal(err)
	}

	mc.mu.Lock()
	if len(mc.resets) != 1 || mc.resets[0].errorCode != ErrorInternalError {
		t.Fatal("expected RESET_STREAM")
	}
	mc.mu.Unlock()

	if s.State() != StreamStateReset {
		t.Fatalf("state: got %d, want Reset", s.State())
	}

	// Read should return error
	buf := make([]byte, 10)
	_, err = s.Read(buf)
	if err != ErrStreamReset {
		t.Fatalf("expected ErrStreamReset, got %v", err)
	}
}

func TestStream_WriteAfterClose(t *testing.T) {
	s, _ := newTestStream(t)
	s.Close()

	_, err := s.Write([]byte("nope"))
	if err != ErrWriteAfterClose {
		t.Fatalf("expected ErrWriteAfterClose, got %v", err)
	}
}

func TestStream_Fragmentation(t *testing.T) {
	s, mc := newTestStream(t)

	// Write more than one chunk
	bigData := make([]byte, 3000)
	for i := range bigData {
		bigData[i] = byte(i % 256)
	}

	n, err := s.Write(bigData)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3000 {
		t.Fatalf("wrote %d, want 3000", n)
	}

	mc.mu.Lock()
	if len(mc.sent) < 2 {
		t.Fatalf("expected multiple fragments, got %d", len(mc.sent))
	}
	totalSent := 0
	for _, f := range mc.sent {
		totalSent += len(f.data)
	}
	if totalSent != 3000 {
		t.Fatalf("total sent: %d, want 3000", totalSent)
	}
	mc.mu.Unlock()
}

func TestStream_ConcurrentReadWrite(t *testing.T) {
	s, _ := newTestStream(t)

	var wg sync.WaitGroup
	wg.Add(2)

	// Writer
	go func() {
		defer wg.Done()
		s.Write([]byte("concurrent"))
	}()

	// Reader
	go func() {
		defer wg.Done()
		s.DeliverData(0, []byte("response"))
		buf := make([]byte, 20)
		s.Read(buf)
	}()

	wg.Wait()
}
