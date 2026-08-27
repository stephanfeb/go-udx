package udx

import "time"

// Transport Parameters
const (
	InitialMaxData       = 1024 * 1024 // 1 MB
	InitialMaxStreamData = 65536       // 64 KB

	// MaxStreamRecvWindow caps the auto-tuned per-stream receive window.
	// The window doubles on each WINDOW_UPDATE, and an update only fires after
	// the application has drained half a window, so a stream must actually move
	// this many bytes before it earns the full window.
	MaxStreamRecvWindow = 4 << 20 // 4 MB

	// StreamBlockedRetryInterval is how often a flow-control-blocked writer
	// re-sends STREAM_DATA_BLOCKED while waiting for credit. WINDOW_UPDATE and
	// STREAM_DATA_BLOCKED both ride seq=0 control packets that are never
	// retransmitted, so this retry is the only thing that recovers a stream
	// whose window update was dropped.
	StreamBlockedRetryInterval = 500 * time.Millisecond

	// sendCreditPollInterval bounds how long a congestion-limited writer parks
	// before re-checking. A lost ACK must not wedge it: the PTO timer needs a
	// chance to retransmit and release inflight bytes.
	sendCreditPollInterval = 5 * time.Millisecond

	// maxConnRecvOOO caps how many data packets the connection holds while
	// waiting for a gap to be filled. In-flight data is already bounded by the
	// congestion window, so this only ever trips on a pathological peer;
	// overflow costs a retransmission, not correctness.
	maxConnRecvOOO = 4096

	InitialMaxStreams     = 100
	MaxAckDelay          = 25 * time.Millisecond
	AckDelayExponent     = 3
)

// Error Codes
const (
	ErrorNoError          = 0x00
	ErrorInternalError    = 0x01
	ErrorStreamLimitError = 0x02
	ErrorFlowControlError = 0x03
	ErrorProtocolViolation = 0x04
	ErrorInvalidMigration = 0x05
	ErrorConnectionTimeout = 0x06
)

// Timeouts
const (
	InitialRTT       = 333 * time.Millisecond
	MaxIdleTimeout   = 30 * time.Second
	HandshakeTimeout = 10 * time.Second
)

// Congestion Control
const (
	MaxDatagramSize        = 1472
	MinCongestionWindow    = 2 * MaxDatagramSize
	InitialCongestionWindow = 10 * MaxDatagramSize
	MaxCongestionWindow    = 1000 * MaxDatagramSize

	// Aliases used by congestion controller
	InitialCwnd = InitialCongestionWindow
	MinCwnd     = MinCongestionWindow
)

// CUBIC parameters
const (
	BetaCubic = 0.7
	CubicC    = 0.4
	PacingGain = 2.88
	PersistentCongestionThreshold = 3
)

// RTT estimation (RFC 9002)
const (
	RTTAlpha = 0.125 // 1/8
	RTTBeta  = 0.25  // 1/4
)

// PTO bounds
const (
	MinPTO = 200 * time.Millisecond
	MaxPTO = 5 * time.Second
)

// Loss detection
const (
	LossDetectionThresholdNum = 9.0
	LossDetectionThresholdDen = 8.0
)

// Path MTU Discovery
const (
	MinMTU          = 1280
	MaxMTU          = 1500
	MTUProbeTimeout = 2 * time.Second
)

// Anti-Amplification
const (
	AmplificationFactor   = 3
	MinBytesForValidation = 1000
)

// Stream Management
const (
	DefaultStreamPriority = 128
	MaxStreamPriority     = 255
)

// Connection IDs
const (
	MinCIDLength     = 0
	MaxCIDLength     = 20
	DefaultCIDLength = 8
)

// Stateless Reset
const (
	StatelessResetTokenLength    = 16
	MinStatelessResetPacketSize  = 39
)

// Protocol Versions
const (
	VersionV1      uint32 = 0x00000001
	VersionV2      uint32 = 0x00000002
	VersionCurrent        = VersionV2
)

// Retransmission
//
// The budget is sized against MaxIdleTimeout: on a healthy path a packet runs
// out of attempts at roughly 28s, just inside 30s. On a slow path the backoff
// tracks the RTO instead of the cap and the budget stretches with it, which is
// the right trade — a long round trip earns more time, not fewer tries.
//
// It used to be 10 attempts with the backoff capped at 30s, which spent them
// over ~111s. Most of that went on waiting rather than trying: the last two
// attempts alone accounted for 60s. Capping the backoff buys 16 attempts in a
// quarter of the time.
//
// Note that MaxIdleTimeout is declared but not currently enforced anywhere, so
// running out of attempts is the ONLY thing that surfaces an unrecoverable
// packet. That is why exhausting them resets the stream (Connection.
// abandonStream) rather than silently dropping the packet: nothing else would
// ever tell the application.
const (
	MaxRetransmitRetries = 16
	MinRetransmitTimeout = 200 * time.Millisecond

	// MaxRetransmitBackoff caps the wait between attempts. It is a ceiling, not
	// a fixed interval, and never applies below the current RTO — retransmitting
	// faster than the round trip just piles duplicates onto a path that has not
	// had time to answer.
	MaxRetransmitBackoff = 2 * time.Second
)
