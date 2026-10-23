package udx

import (
	"sync"
	"time"
)

// MTUDiscovery finds how large a datagram the path carries, in the
// packetization layer (DPLPMTUD, RFC 8899; RFC 9000 section 14), and
// watches for the path shrinking underneath a validated size.
//
// Streams send datagrams of SafeDatagramSize, 1200 bytes, which any path
// carries. The search probes up from there towards MaxDatagramSize with
// packets padded to the size under test: one is acknowledged, that size is
// validated and streams use it; one is lost MTUProbeTries times, that size
// is ruled out. The next size is halfway between the largest validated and
// the smallest ruled out, and the search ends when the two are within
// MTUSearchStep. A probe is numbered and tracked like data, so the ACK
// ranges and loss detection report on it, but it is never re-sent, is not
// booked in flight, and its loss is no congestion signal (section 14.4).
//
// A black hole is a path that stopped carrying the validated size: a
// carrier's tunnel on the far side of a handover, where every full-size
// packet vanishes and only the small ones get through. BlackholePackets
// consecutive full-size data packets lost with none acknowledged in
// between, or BlackholePTOs probe timeouts in a row (nothing at all is
// being acknowledged), take the size back to SafeDatagramSize at once;
// re-sends are re-chunked to fit (Connection.fitToPath), and the search
// starts over after BlackholeRetry.
//
// This was a controller that nothing drove: the connection constructed it
// and never sent a probe, and every stream sent 1200-byte datagrams for
// the life of the connection.
type MTUDiscovery struct {
	mu    sync.Mutex
	clock Clock

	current   int // validated: what streams send
	probe     int // the size under test, 0 when the search is over
	lowFailed int // the smallest size ruled out
	tries     int // losses of probe so far
	inFlight  bool
	done      bool

	// retryAt is when the search restarts after a black hole; zero when
	// there was none.
	retryAt time.Time
	// largeLost counts consecutive lost data packets larger than the base
	// with none acknowledged since.
	largeLost int
}

// NewMTUDiscovery starts a search that will probe MaxDatagramSize first.
func NewMTUDiscovery(clock Clock) *MTUDiscovery {
	m := &MTUDiscovery{clock: clock, current: SafeDatagramSize}
	m.restartLocked()
	return m
}

func (m *MTUDiscovery) restartLocked() {
	m.probe = MaxDatagramSize
	m.lowFailed = MaxDatagramSize + 1
	m.tries = 0
	m.inFlight = false
	m.done = false
	m.retryAt = time.Time{}
	m.largeLost = 0
}

// Current is the validated datagram size: what streams send now.
func (m *MTUDiscovery) Current() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// Done reports whether the search has ended (also after a black hole,
// until the retry).
func (m *MTUDiscovery) Done() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.done
}

// NextProbe returns the size to probe now and marks it in flight, or 0
// when nothing is to be sent: the search is over, a probe is already in
// flight, or the wait after a black hole has not passed.
func (m *MTUDiscovery) NextProbe() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.retryAt.IsZero() {
		if m.clock.Now().Before(m.retryAt) {
			return 0
		}
		m.restartLocked()
	}
	if m.done || m.inFlight {
		return 0
	}
	m.inFlight = true
	return m.probe
}

// OnProbeAcked validates size and moves the search up.
func (m *MTUDiscovery) OnProbeAcked(size int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight = false
	if !m.retryAt.IsZero() {
		return // stale: the path has shrunk since
	}
	if size > m.current {
		m.current = size
	}
	m.tries = 0
	m.nextLocked()
}

// OnProbeLost counts a loss of size; the MTUProbeTries-th rules it out and
// moves the search down.
func (m *MTUDiscovery) OnProbeLost(size int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inFlight = false
	if !m.retryAt.IsZero() {
		return
	}
	m.tries++
	if m.tries < MTUProbeTries {
		return // the same size again
	}
	m.tries = 0
	if size < m.lowFailed {
		m.lowFailed = size
	}
	m.nextLocked()
}

// nextLocked picks the next size to probe, or ends the search.
func (m *MTUDiscovery) nextLocked() {
	if m.lowFailed-m.current <= MTUSearchStep {
		m.done = true
		m.probe = 0
		return
	}
	m.probe = (m.current + m.lowFailed) / 2
}

// OnDataAcked is told of every acknowledged data packet: a full-size one
// getting through clears the black-hole count.
func (m *MTUDiscovery) OnDataAcked(size int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if size > SafeDatagramSize {
		m.largeLost = 0
	}
}

// OnDataLost is told of every lost data packet: BlackholePackets full-size
// ones in a row, with none acknowledged between, mean the path no longer
// carries the validated size.
func (m *MTUDiscovery) OnDataLost(size int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current <= SafeDatagramSize || size <= SafeDatagramSize {
		return
	}
	m.largeLost++
	if m.largeLost >= BlackholePackets {
		m.blackholeLocked()
	}
}

// Blackhole takes the size back to the base at once, as the connection
// does after BlackholePTOs probe timeouts in a row; the search restarts
// after BlackholeRetry.
func (m *MTUDiscovery) Blackhole() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blackholeLocked()
}

func (m *MTUDiscovery) blackholeLocked() {
	if m.current <= SafeDatagramSize {
		return
	}
	m.current = SafeDatagramSize
	m.probe = 0
	m.done = true
	m.inFlight = false
	m.largeLost = 0
	m.retryAt = m.clock.Now().Add(BlackholeRetry)
}

// Reset starts over from the base, for a connection that moved to a new
// path (RFC 9000 section 9.4).
func (m *MTUDiscovery) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = SafeDatagramSize
	m.restartLocked()
}
