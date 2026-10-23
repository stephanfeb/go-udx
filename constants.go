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

	// maxStreamRecvOOO backstops the per-stream out-of-order buffer. Flow
	// control is what actually bounds it; this only catches a peer ignoring its
	// limit, so it sits above the largest window rather than at it. Set equal
	// to MaxStreamRecvWindow it fires during legitimate transfers, and since
	// the packet has already been acknowledged by then, the refused bytes are
	// never re-sent and the stream stalls forever.
	maxStreamRecvOOO = 2 * MaxStreamRecvWindow

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

	InitialMaxStreams = 100

	// Acknowledgement policy (RFC 9000 section 13.2). A receiver acknowledges
	// at once when a packet arrives out of order or opens or closes a stream,
	// and otherwise after every AckElicitingThreshold data-bearing packets or
	// when the ACK timer fires, whichever is first. Acknowledging every packet
	// on arrival, which this did before, made the receiver send one datagram
	// for every datagram it got: on a 137 KB transfer that was 100 ACKs, and
	// on the server the read loop that also sends them spent as long in
	// sendto as in recvfrom and saturated one core at ~150k datagrams/s.
	AckElicitingThreshold = 2

	// The ACK timer is a quarter of the smoothed RTT, clamped to this range.
	// It is reported in the frame's AckDelay so the sender's RTT sample is not
	// inflated by it, and MaxAckDelay is the most a sender will subtract (the
	// RFC 9002 cap). A short floor keeps a lone packet on a fast path from
	// waiting long: the packet the ACK releases from the sender's inflight
	// accounting may be what its next write is waiting on.
	MinAckDelay = 1 * time.Millisecond
	MaxAckDelay = 25 * time.Millisecond

	// ackHistory is how many sequences below the largest received the ACK
	// builder remembers (recvTracker).
	ackHistory = 512

	// sendBatchMax is how many STREAM chunks one Write hands the socket at a
	// time: they go out in one batch write (sendmmsg on Linux), and the
	// congestion window is asked for them together. Sixteen chunks is
	// 22 KB, about the burst QUIC pacers allow, and the initial window is
	// ten chunks, so an unopened window still sends what it may.
	sendBatchMax = 16

	// inboundQueue is how many decoded packets a connection can have waiting
	// for its goroutine before the read loop drops new ones for it. Dropping
	// is what the socket would do if the loop fell behind; loss recovery
	// covers it either way. 512 packets is about 700 KB of a bulk stream.
	inboundQueue = 512

	AckDelayExponent = 3
)

// Error Codes
const (
	ErrorNoError           = 0x00
	ErrorInternalError     = 0x01
	ErrorStreamLimitError  = 0x02
	ErrorFlowControlError  = 0x03
	ErrorProtocolViolation = 0x04
	ErrorInvalidMigration  = 0x05
	ErrorConnectionTimeout = 0x06
)

// Timeouts
const (
	// InitialRTT is the smoothed RTT before the first sample (RFC 9002
	// section 6.2.2), with InitialRTTVar its variation: the first PTO is
	// about a second. The estimate used to start at 100 ms, so on a slow
	// path the first probe went out before the handshake could answer.
	InitialRTT    = 333 * time.Millisecond
	InitialRTTVar = InitialRTT / 2

	MaxIdleTimeout   = 30 * time.Second
	HandshakeTimeout = 10 * time.Second
)

// SafeDatagramSize is the largest datagram a stream's data goes out in:
// what any internet path carries, as QUIC's 1200 (RFC 9000 section 14), so
// it fits the 1280-byte IPv6 minimum MTU with its headers. Mobile carriers'
// tunnels (464XLAT, GTP) carry less than 1500, and drop a larger datagram
// that may not be fragmented: a reply of more than one full datagram then
// never arrives, and the sender gives the connection up.
const SafeDatagramSize = 1200

// Congestion Control
const (
	MaxDatagramSize         = 1472
	MinCongestionWindow     = 2 * MaxDatagramSize
	InitialCongestionWindow = 10 * MaxDatagramSize
	MaxCongestionWindow     = 1000 * MaxDatagramSize

	// Aliases used by congestion controller
	InitialCwnd = InitialCongestionWindow
	MinCwnd     = MinCongestionWindow
)

// CUBIC parameters
const (
	BetaCubic                     = 0.7
	CubicC                        = 0.4
	PersistentCongestionThreshold = 3
)

// Pacing (RFC 9002 section 7.7). The rate is gain * cwnd / smoothed_rtt;
// section 7.7 suggests a gain of about 1.25, and slow start, where the
// window doubles each round trip, takes 2 so that pacing does not hold the
// window's growth back. (The gain used to be BBR's 2.88 startup gain, over
// min_rtt, for the whole connection.)
const (
	PacingGain          = 1.25
	PacingGainSlowStart = 2.0

	// PacingBurstPackets is how many datagrams may go out at once without
	// waiting on the pacer; the budget holds this many, or MinPacingDelay
	// of the rate when that is more.
	PacingBurstPackets = 10

	// MinPacingDelay is the shortest wait the pacer asks for: a timer is
	// not set for less, and the burst covers at least this much of the rate
	// so a wait that overshoots by that little costs no throughput.
	MinPacingDelay = time.Millisecond
)

// RTT estimation (RFC 9002)
const (
	RTTAlpha = 0.125 // 1/8
	RTTBeta  = 0.25  // 1/4
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
	StatelessResetTokenLength   = 16
	MinStatelessResetPacketSize = 39
)

// Protocol Versions
//
// V3 added the byte offset to STREAM frames. It is not compatible with V2: the
// eight extra bytes sit where a V2 parser expects the data length, so a V2 peer
// does not fail on a V3 frame, it reads a plausible wrong length and delivers
// nonsense into the byte stream. Multiplexer.handleDatagram drops packets with
// an unsupported version for exactly that reason — a version mismatch has to be
// a dropped packet, not silent corruption.
const (
	VersionV1      uint32 = 0x00000001
	VersionV2      uint32 = 0x00000002
	VersionV3      uint32 = 0x00000003
	VersionCurrent        = VersionV3
)

// Retransmission
//
// There is no retransmission timer per packet and no per-packet retry cap.
// Loss is declared from acknowledgements (PacketManager.detectLostLocked:
// the packet and time thresholds of RFC 9002 section 6.1), and the lost data
// is re-sent under a fresh sequence number through the congestion window.
// When nothing is acknowledged at all, the connection's one PTO timer
// (CongestionController) sends ptoProbes probes, backing off exponentially
// for as long as the connection lives; what ends a path that has genuinely
// gone away is MaxIdleTimeout closing the whole connection.
//
// This replaced one RTO timer per packet. On a radio gap every timer fired
// together and the whole window went out again, past the congestion window
// and the pacer, and again every two seconds into a dead path; the resends
// never contracted the window, so after the gap the sender resumed at the
// pre-gap window into a path whose capacity it no longer knew.
const (
	// ptoProbes is how many outstanding packets a PTO re-sends, the oldest
	// first (RFC 9002 section 6.2.4 allows up to two).
	ptoProbes = 2
)

// Loss detection thresholds, RFC 9002 section 6.1.
//
// A gap in the acknowledged sequence numbers does not mean loss. UDP reorders,
// and a packet that arrives late still arrives. Loss detection used to treat
// every gap as loss on sight; a packet is now only declared lost once it is far
// enough behind the largest acknowledged packet, or old enough, that reordering
// can no longer explain it.
//
// Do not expect these to pay for themselves in throughput. They were added on
// the theory that mistaking reordering for loss was expensive, and measurement
// did not bear that out: against loss_detection_test.go the retransmission rate
// moved by less than a point at every displacement from 200us to 5ms, and at
// every reordering rate from 2% to 20%. The reason is that kPacketThreshold
// tolerates three packets of displacement, and on a fast link even a
// sub-millisecond delay displaces far more than three, so the threshold is
// exceeded and the packet is declared lost anyway.
//
// They are here because declaring every gap lost on sight is wrong in
// principle and diverges from the RFC this transport otherwise follows, not
// because they were observed to help.
const (
	// LossReorderThreshold is kPacketThreshold: how many sequence numbers a
	// packet must trail the largest acknowledged one before a gap counts as
	// loss. RFC 9002 recommends 3, which tolerates the reordering seen on real
	// paths without meaningfully delaying recovery.
	LossReorderThreshold = 3

	// LossTimeThresholdNumerator and LossTimeThresholdDenominator express
	// kTimeThreshold, 9/8, as a fraction to keep the arithmetic in integers.
	// Applied to the larger of the smoothed and latest RTT, this is the other
	// way a packet qualifies as lost: too old to still be in flight, even if
	// fewer than LossReorderThreshold packets have been acknowledged past it.
	LossTimeThresholdNumerator   = 9
	LossTimeThresholdDenominator = 8

	// LossTimerGranularity is kGranularity, the floor on the time threshold. It
	// keeps a near-zero RTT estimate — loopback, or before the first sample —
	// from making the time threshold fire instantly and undo the packet
	// threshold.
	LossTimerGranularity = time.Millisecond
)
