package udx

import "time"

// Clock abstracts time operations for testing.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) *time.Timer
	NewTimer(d time.Duration) *time.Timer
}

// RealClock uses the standard time package.
type RealClock struct{}

func (RealClock) Now() time.Time                                  { return time.Now() }
func (RealClock) AfterFunc(d time.Duration, f func()) *time.Timer { return time.AfterFunc(d, f) }
func (RealClock) NewTimer(d time.Duration) *time.Timer            { return time.NewTimer(d) }

// MockClock is a controllable clock for testing.
type MockClock struct {
	now time.Time
}

// NewMockClock creates a mock clock at the given time.
func NewMockClock(t time.Time) *MockClock {
	return &MockClock{now: t}
}

func (m *MockClock) Now() time.Time { return m.now }

func (m *MockClock) AfterFunc(d time.Duration, f func()) *time.Timer {
	// Return a stopped timer — tests manually control time
	t := time.NewTimer(time.Hour)
	t.Stop()
	return t
}

func (m *MockClock) NewTimer(d time.Duration) *time.Timer {
	t := time.NewTimer(time.Hour)
	t.Stop()
	return t
}

// Advance moves the mock clock forward by d.
func (m *MockClock) Advance(d time.Duration) {
	m.now = m.now.Add(d)
}
