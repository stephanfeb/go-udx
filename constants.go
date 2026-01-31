package udx

import "time"

// Transport Parameters
const (
	InitialMaxData       = 1024 * 1024 // 1 MB
	InitialMaxStreamData = 65536       // 64 KB
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
const (
	MaxRetransmitRetries = 10
	MinRetransmitTimeout = 200 * time.Millisecond
	MaxRetransmitTimeout = 30 * time.Second
)
