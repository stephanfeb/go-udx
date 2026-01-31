package udx

import "fmt"

// UDXError represents a UDX protocol error with an error code.
type UDXError struct {
	Code   uint32
	Reason string
}

func (e *UDXError) Error() string {
	return fmt.Sprintf("udx error %d: %s", e.Code, e.Reason)
}

// Common errors
var (
	ErrConnectionClosed  = &UDXError{Code: ErrorNoError, Reason: "connection closed"}
	ErrInternalError     = &UDXError{Code: ErrorInternalError, Reason: "internal error"}
	ErrStreamLimit       = &UDXError{Code: ErrorStreamLimitError, Reason: "stream limit exceeded"}
	ErrFlowControl       = &UDXError{Code: ErrorFlowControlError, Reason: "flow control error"}
	ErrProtocolViolation = &UDXError{Code: ErrorProtocolViolation, Reason: "protocol violation"}
	ErrInvalidMigration  = &UDXError{Code: ErrorInvalidMigration, Reason: "invalid migration"}
	ErrTimeout           = &UDXError{Code: ErrorConnectionTimeout, Reason: "connection timeout"}
)

// ErrPacketTooShort is returned when a packet is too short to parse.
var ErrPacketTooShort = fmt.Errorf("packet too short")

// ErrInvalidCIDLength is returned when a CID length is out of range.
var ErrInvalidCIDLength = fmt.Errorf("invalid connection ID length")

// ErrUnknownFrameType is returned when parsing an unknown frame type.
var ErrUnknownFrameType = fmt.Errorf("unknown frame type")

// ErrUnsupportedVersion is returned when a packet has an unsupported version.
var ErrUnsupportedVersion = fmt.Errorf("unsupported protocol version")
