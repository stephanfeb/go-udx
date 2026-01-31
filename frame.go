package udx

import (
	"encoding/binary"
	"fmt"
)

// FrameType identifies the type of a UDX frame.
type FrameType uint8

const (
	FramePadding           FrameType = 0x00
	FramePing              FrameType = 0x01
	FrameAck               FrameType = 0x02
	FrameStream            FrameType = 0x03
	FrameWindowUpdate      FrameType = 0x04
	FrameMaxData           FrameType = 0x05
	FrameResetStream       FrameType = 0x06
	FrameMaxStreams         FrameType = 0x07
	FrameMTUProbe          FrameType = 0x08
	FramePathChallenge     FrameType = 0x09
	FramePathResponse      FrameType = 0x0a
	FrameConnectionClose   FrameType = 0x0b
	// 0x0c is unused
	FrameStopSending       FrameType = 0x0d
	FrameDataBlocked       FrameType = 0x0e
	FrameStreamDataBlocked FrameType = 0x0f
	FrameNewConnectionID   FrameType = 0x10
	FrameRetireConnectionID FrameType = 0x11
)

// Frame is the interface implemented by all UDX frame types.
type Frame interface {
	Type() FrameType
	Marshal() []byte
	Len() int
}

// UnmarshalFrame parses a single frame from data starting at the given offset.
// Returns the parsed frame and the number of bytes consumed.
func UnmarshalFrame(data []byte, offset int) (Frame, int, error) {
	if offset >= len(data) {
		return nil, 0, fmt.Errorf("%w: no data for frame", ErrPacketTooShort)
	}
	ft := FrameType(data[offset])
	switch ft {
	case FramePadding:
		return &PaddingFrame{}, 1, nil
	case FramePing:
		return &PingFrame{}, 1, nil
	case FrameAck:
		return unmarshalAckFrame(data, offset)
	case FrameStream:
		return unmarshalStreamFrame(data, offset)
	case FrameWindowUpdate:
		return unmarshalWindowUpdateFrame(data, offset)
	case FrameMaxData:
		return unmarshalMaxDataFrame(data, offset)
	case FrameResetStream:
		return unmarshalResetStreamFrame(data, offset)
	case FrameMaxStreams:
		return unmarshalMaxStreamsFrame(data, offset)
	case FrameMTUProbe:
		return unmarshalMTUProbeFrame(data, offset)
	case FramePathChallenge:
		return unmarshalPathChallengeFrame(data, offset)
	case FramePathResponse:
		return unmarshalPathResponseFrame(data, offset)
	case FrameConnectionClose:
		return unmarshalConnectionCloseFrame(data, offset)
	case FrameStopSending:
		return unmarshalStopSendingFrame(data, offset)
	case FrameDataBlocked:
		return unmarshalDataBlockedFrame(data, offset)
	case FrameStreamDataBlocked:
		return unmarshalStreamDataBlockedFrame(data, offset)
	case FrameNewConnectionID:
		return unmarshalNewConnectionIDFrame(data, offset)
	case FrameRetireConnectionID:
		return unmarshalRetireConnectionIDFrame(data, offset)
	default:
		return nil, 0, fmt.Errorf("%w: 0x%02x", ErrUnknownFrameType, ft)
	}
}

// --- PADDING Frame (0x00) ---

type PaddingFrame struct{}

func (f *PaddingFrame) Type() FrameType { return FramePadding }
func (f *PaddingFrame) Len() int        { return 1 }
func (f *PaddingFrame) Marshal() []byte { return []byte{byte(FramePadding)} }

// --- PING Frame (0x01) ---

type PingFrame struct{}

func (f *PingFrame) Type() FrameType { return FramePing }
func (f *PingFrame) Len() int        { return 1 }
func (f *PingFrame) Marshal() []byte { return []byte{byte(FramePing)} }

// --- ACK Frame (0x02) ---

// AckRange represents an additional ACK range (gap + length).
type AckRange struct {
	Gap            uint8
	AckRangeLength uint32
}

// AckFrame acknowledges received packets with QUIC-style ranges.
type AckFrame struct {
	LargestAcked       uint32
	AckDelay           uint16 // milliseconds
	FirstAckRangeLength uint32
	AckRanges          []AckRange
}

func (f *AckFrame) Type() FrameType { return FrameAck }

func (f *AckFrame) Len() int {
	// Type(1) + LargestAcked(4) + AckDelay(2) + RangeCount(1) + FirstRange(4) + ranges*(Gap(1)+Len(4))
	return 1 + 4 + 2 + 1 + 4 + len(f.AckRanges)*5
}

func (f *AckFrame) Marshal() []byte {
	buf := make([]byte, f.Len())
	buf[0] = byte(FrameAck)
	binary.BigEndian.PutUint32(buf[1:], f.LargestAcked)
	binary.BigEndian.PutUint16(buf[5:], f.AckDelay)
	buf[7] = byte(len(f.AckRanges))
	binary.BigEndian.PutUint32(buf[8:], f.FirstAckRangeLength)
	offset := 12
	for _, r := range f.AckRanges {
		buf[offset] = r.Gap
		binary.BigEndian.PutUint32(buf[offset+1:], r.AckRangeLength)
		offset += 5
	}
	return buf
}

func unmarshalAckFrame(data []byte, offset int) (*AckFrame, int, error) {
	start := offset
	if offset+12 > len(data) { // type(1)+largest(4)+delay(2)+count(1)+firstRange(4)
		return nil, 0, fmt.Errorf("%w: ACK frame", ErrPacketTooShort)
	}
	offset++ // skip type
	largest := binary.BigEndian.Uint32(data[offset:])
	offset += 4
	delay := binary.BigEndian.Uint16(data[offset:])
	offset += 2
	rangeCount := int(data[offset])
	offset++
	firstRange := binary.BigEndian.Uint32(data[offset:])
	offset += 4

	if offset+rangeCount*5 > len(data) {
		return nil, 0, fmt.Errorf("%w: ACK frame ranges", ErrPacketTooShort)
	}
	ranges := make([]AckRange, rangeCount)
	for i := 0; i < rangeCount; i++ {
		ranges[i] = AckRange{
			Gap:            data[offset],
			AckRangeLength: binary.BigEndian.Uint32(data[offset+1:]),
		}
		offset += 5
	}

	return &AckFrame{
		LargestAcked:       largest,
		AckDelay:           delay,
		FirstAckRangeLength: firstRange,
		AckRanges:          ranges,
	}, offset - start, nil
}

// --- STREAM Frame (0x03) ---

const (
	StreamFlagFIN = 0x01
	StreamFlagSYN = 0x02
)

// StreamFrame carries data for a specific stream.
type StreamFrame struct {
	IsFin bool
	IsSyn bool
	Data  []byte
}

func (f *StreamFrame) Type() FrameType { return FrameStream }
func (f *StreamFrame) Len() int        { return 1 + 1 + 2 + len(f.Data) } // type+flags+datalen+data

func (f *StreamFrame) Marshal() []byte {
	buf := make([]byte, f.Len())
	buf[0] = byte(FrameStream)
	var flags byte
	if f.IsFin {
		flags |= StreamFlagFIN
	}
	if f.IsSyn {
		flags |= StreamFlagSYN
	}
	buf[1] = flags
	binary.BigEndian.PutUint16(buf[2:], uint16(len(f.Data)))
	copy(buf[4:], f.Data)
	return buf
}

func unmarshalStreamFrame(data []byte, offset int) (*StreamFrame, int, error) {
	if offset+4 > len(data) { // type+flags+datalen minimum
		return nil, 0, fmt.Errorf("%w: STREAM frame header", ErrPacketTooShort)
	}
	flags := data[offset+1]
	dataLen := int(binary.BigEndian.Uint16(data[offset+2:]))
	if offset+4+dataLen > len(data) {
		return nil, 0, fmt.Errorf("%w: STREAM frame data", ErrPacketTooShort)
	}
	payload := make([]byte, dataLen)
	copy(payload, data[offset+4:offset+4+dataLen])
	return &StreamFrame{
		IsFin: flags&StreamFlagFIN != 0,
		IsSyn: flags&StreamFlagSYN != 0,
		Data:  payload,
	}, 4 + dataLen, nil
}

// --- WINDOW_UPDATE Frame (0x04) ---

type WindowUpdateFrame struct {
	WindowSize uint32
}

func (f *WindowUpdateFrame) Type() FrameType { return FrameWindowUpdate }
func (f *WindowUpdateFrame) Len() int        { return 5 }

func (f *WindowUpdateFrame) Marshal() []byte {
	buf := make([]byte, 5)
	buf[0] = byte(FrameWindowUpdate)
	binary.BigEndian.PutUint32(buf[1:], f.WindowSize)
	return buf
}

func unmarshalWindowUpdateFrame(data []byte, offset int) (*WindowUpdateFrame, int, error) {
	if offset+5 > len(data) {
		return nil, 0, fmt.Errorf("%w: WINDOW_UPDATE frame", ErrPacketTooShort)
	}
	return &WindowUpdateFrame{
		WindowSize: binary.BigEndian.Uint32(data[offset+1:]),
	}, 5, nil
}

// --- MAX_DATA Frame (0x05) ---

type MaxDataFrame struct {
	MaxData uint64
}

func (f *MaxDataFrame) Type() FrameType { return FrameMaxData }
func (f *MaxDataFrame) Len() int        { return 9 }

func (f *MaxDataFrame) Marshal() []byte {
	buf := make([]byte, 9)
	buf[0] = byte(FrameMaxData)
	binary.BigEndian.PutUint64(buf[1:], f.MaxData)
	return buf
}

func unmarshalMaxDataFrame(data []byte, offset int) (*MaxDataFrame, int, error) {
	if offset+9 > len(data) {
		return nil, 0, fmt.Errorf("%w: MAX_DATA frame", ErrPacketTooShort)
	}
	return &MaxDataFrame{
		MaxData: binary.BigEndian.Uint64(data[offset+1:]),
	}, 9, nil
}

// --- RESET_STREAM Frame (0x06) ---

type ResetStreamFrame struct {
	ErrorCode uint32
}

func (f *ResetStreamFrame) Type() FrameType { return FrameResetStream }
func (f *ResetStreamFrame) Len() int        { return 5 }

func (f *ResetStreamFrame) Marshal() []byte {
	buf := make([]byte, 5)
	buf[0] = byte(FrameResetStream)
	binary.BigEndian.PutUint32(buf[1:], f.ErrorCode)
	return buf
}

func unmarshalResetStreamFrame(data []byte, offset int) (*ResetStreamFrame, int, error) {
	if offset+5 > len(data) {
		return nil, 0, fmt.Errorf("%w: RESET_STREAM frame", ErrPacketTooShort)
	}
	return &ResetStreamFrame{
		ErrorCode: binary.BigEndian.Uint32(data[offset+1:]),
	}, 5, nil
}

// --- MAX_STREAMS Frame (0x07) ---

type MaxStreamsFrame struct {
	MaxStreamCount uint32
}

func (f *MaxStreamsFrame) Type() FrameType { return FrameMaxStreams }
func (f *MaxStreamsFrame) Len() int        { return 5 }

func (f *MaxStreamsFrame) Marshal() []byte {
	buf := make([]byte, 5)
	buf[0] = byte(FrameMaxStreams)
	binary.BigEndian.PutUint32(buf[1:], f.MaxStreamCount)
	return buf
}

func unmarshalMaxStreamsFrame(data []byte, offset int) (*MaxStreamsFrame, int, error) {
	if offset+5 > len(data) {
		return nil, 0, fmt.Errorf("%w: MAX_STREAMS frame", ErrPacketTooShort)
	}
	return &MaxStreamsFrame{
		MaxStreamCount: binary.BigEndian.Uint32(data[offset+1:]),
	}, 5, nil
}

// --- MTU_PROBE Frame (0x08) ---

type MTUProbeFrame struct {
	ProbeSize int // total frame size including type byte
}

func (f *MTUProbeFrame) Type() FrameType { return FrameMTUProbe }
func (f *MTUProbeFrame) Len() int {
	if f.ProbeSize < 1 {
		return 1
	}
	return f.ProbeSize
}

func (f *MTUProbeFrame) Marshal() []byte {
	size := f.Len()
	buf := make([]byte, size)
	buf[0] = byte(FrameMTUProbe)
	// rest is zero-padding
	return buf
}

func unmarshalMTUProbeFrame(data []byte, offset int) (*MTUProbeFrame, int, error) {
	// MTU probe consumes the rest of the packet
	remaining := len(data) - offset
	if remaining < 1 {
		return nil, 0, fmt.Errorf("%w: MTU_PROBE frame", ErrPacketTooShort)
	}
	return &MTUProbeFrame{ProbeSize: remaining}, remaining, nil
}

// --- PATH_CHALLENGE Frame (0x09) ---

type PathChallengeFrame struct {
	Data [8]byte
}

func (f *PathChallengeFrame) Type() FrameType { return FramePathChallenge }
func (f *PathChallengeFrame) Len() int        { return 9 }

func (f *PathChallengeFrame) Marshal() []byte {
	buf := make([]byte, 9)
	buf[0] = byte(FramePathChallenge)
	copy(buf[1:], f.Data[:])
	return buf
}

func unmarshalPathChallengeFrame(data []byte, offset int) (*PathChallengeFrame, int, error) {
	if offset+9 > len(data) {
		return nil, 0, fmt.Errorf("%w: PATH_CHALLENGE frame", ErrPacketTooShort)
	}
	var f PathChallengeFrame
	copy(f.Data[:], data[offset+1:offset+9])
	return &f, 9, nil
}

// --- PATH_RESPONSE Frame (0x0a) ---

type PathResponseFrame struct {
	Data [8]byte
}

func (f *PathResponseFrame) Type() FrameType { return FramePathResponse }
func (f *PathResponseFrame) Len() int        { return 9 }

func (f *PathResponseFrame) Marshal() []byte {
	buf := make([]byte, 9)
	buf[0] = byte(FramePathResponse)
	copy(buf[1:], f.Data[:])
	return buf
}

func unmarshalPathResponseFrame(data []byte, offset int) (*PathResponseFrame, int, error) {
	if offset+9 > len(data) {
		return nil, 0, fmt.Errorf("%w: PATH_RESPONSE frame", ErrPacketTooShort)
	}
	var f PathResponseFrame
	copy(f.Data[:], data[offset+1:offset+9])
	return &f, 9, nil
}

// --- CONNECTION_CLOSE Frame (0x0b) ---

type ConnectionCloseFrame struct {
	ErrorCode    uint32
	FrameTypeVal uint32 // which frame type caused the error (0 if N/A)
	ReasonPhrase string
}

func (f *ConnectionCloseFrame) Type() FrameType { return FrameConnectionClose }

func (f *ConnectionCloseFrame) Len() int {
	// type(1) + errorCode(4) + frameType(4) + reasonLen(2) + reason
	return 1 + 4 + 4 + 2 + len(f.ReasonPhrase)
}

func (f *ConnectionCloseFrame) Marshal() []byte {
	buf := make([]byte, f.Len())
	buf[0] = byte(FrameConnectionClose)
	binary.BigEndian.PutUint32(buf[1:], f.ErrorCode)
	binary.BigEndian.PutUint32(buf[5:], f.FrameTypeVal)
	binary.BigEndian.PutUint16(buf[9:], uint16(len(f.ReasonPhrase)))
	copy(buf[11:], f.ReasonPhrase)
	return buf
}

func unmarshalConnectionCloseFrame(data []byte, offset int) (*ConnectionCloseFrame, int, error) {
	if offset+11 > len(data) { // type+errCode+frameType+reasonLen
		return nil, 0, fmt.Errorf("%w: CONNECTION_CLOSE frame header", ErrPacketTooShort)
	}
	errCode := binary.BigEndian.Uint32(data[offset+1:])
	frameType := binary.BigEndian.Uint32(data[offset+5:])
	reasonLen := int(binary.BigEndian.Uint16(data[offset+9:]))
	total := 11 + reasonLen
	if offset+total > len(data) {
		return nil, 0, fmt.Errorf("%w: CONNECTION_CLOSE frame reason", ErrPacketTooShort)
	}
	reason := string(data[offset+11 : offset+11+reasonLen])
	return &ConnectionCloseFrame{
		ErrorCode:    errCode,
		FrameTypeVal: frameType,
		ReasonPhrase: reason,
	}, total, nil
}

// --- STOP_SENDING Frame (0x0d) ---

type StopSendingFrame struct {
	StreamID  uint32
	ErrorCode uint32
}

func (f *StopSendingFrame) Type() FrameType { return FrameStopSending }
func (f *StopSendingFrame) Len() int        { return 9 }

func (f *StopSendingFrame) Marshal() []byte {
	buf := make([]byte, 9)
	buf[0] = byte(FrameStopSending)
	binary.BigEndian.PutUint32(buf[1:], f.StreamID)
	binary.BigEndian.PutUint32(buf[5:], f.ErrorCode)
	return buf
}

func unmarshalStopSendingFrame(data []byte, offset int) (*StopSendingFrame, int, error) {
	if offset+9 > len(data) {
		return nil, 0, fmt.Errorf("%w: STOP_SENDING frame", ErrPacketTooShort)
	}
	return &StopSendingFrame{
		StreamID:  binary.BigEndian.Uint32(data[offset+1:]),
		ErrorCode: binary.BigEndian.Uint32(data[offset+5:]),
	}, 9, nil
}

// --- DATA_BLOCKED Frame (0x0e) ---

type DataBlockedFrame struct {
	MaxData uint64
}

func (f *DataBlockedFrame) Type() FrameType { return FrameDataBlocked }
func (f *DataBlockedFrame) Len() int        { return 9 }

func (f *DataBlockedFrame) Marshal() []byte {
	buf := make([]byte, 9)
	buf[0] = byte(FrameDataBlocked)
	binary.BigEndian.PutUint64(buf[1:], f.MaxData)
	return buf
}

func unmarshalDataBlockedFrame(data []byte, offset int) (*DataBlockedFrame, int, error) {
	if offset+9 > len(data) {
		return nil, 0, fmt.Errorf("%w: DATA_BLOCKED frame", ErrPacketTooShort)
	}
	return &DataBlockedFrame{
		MaxData: binary.BigEndian.Uint64(data[offset+1:]),
	}, 9, nil
}

// --- STREAM_DATA_BLOCKED Frame (0x0f) ---

type StreamDataBlockedFrame struct {
	StreamID      uint32
	MaxStreamData uint64
}

func (f *StreamDataBlockedFrame) Type() FrameType { return FrameStreamDataBlocked }
func (f *StreamDataBlockedFrame) Len() int        { return 13 }

func (f *StreamDataBlockedFrame) Marshal() []byte {
	buf := make([]byte, 13)
	buf[0] = byte(FrameStreamDataBlocked)
	binary.BigEndian.PutUint32(buf[1:], f.StreamID)
	binary.BigEndian.PutUint64(buf[5:], f.MaxStreamData)
	return buf
}

func unmarshalStreamDataBlockedFrame(data []byte, offset int) (*StreamDataBlockedFrame, int, error) {
	if offset+13 > len(data) {
		return nil, 0, fmt.Errorf("%w: STREAM_DATA_BLOCKED frame", ErrPacketTooShort)
	}
	return &StreamDataBlockedFrame{
		StreamID:      binary.BigEndian.Uint32(data[offset+1:]),
		MaxStreamData: binary.BigEndian.Uint64(data[offset+5:]),
	}, 13, nil
}

// --- NEW_CONNECTION_ID Frame (0x10) ---

type NewConnectionIDFrame struct {
	SequenceNumber uint64
	RetirePriorTo  uint64
	ConnectionID   ConnectionID
	ResetToken     [StatelessResetTokenLength]byte
}

func (f *NewConnectionIDFrame) Type() FrameType { return FrameNewConnectionID }

func (f *NewConnectionIDFrame) Len() int {
	// type(1) + seq(8) + retirePriorTo(8) + cidLen(1) + cid + token(16)
	return 1 + 8 + 8 + 1 + f.ConnectionID.Len() + StatelessResetTokenLength
}

func (f *NewConnectionIDFrame) Marshal() []byte {
	buf := make([]byte, f.Len())
	buf[0] = byte(FrameNewConnectionID)
	offset := 1
	binary.BigEndian.PutUint64(buf[offset:], f.SequenceNumber)
	offset += 8
	binary.BigEndian.PutUint64(buf[offset:], f.RetirePriorTo)
	offset += 8
	buf[offset] = byte(f.ConnectionID.Len())
	offset++
	copy(buf[offset:], f.ConnectionID.Bytes())
	offset += f.ConnectionID.Len()
	copy(buf[offset:], f.ResetToken[:])
	return buf
}

func unmarshalNewConnectionIDFrame(data []byte, offset int) (*NewConnectionIDFrame, int, error) {
	start := offset
	if offset+18 > len(data) { // type(1)+seq(8)+retire(8)+cidLen(1) minimum
		return nil, 0, fmt.Errorf("%w: NEW_CONNECTION_ID frame header", ErrPacketTooShort)
	}
	offset++ // skip type
	seq := binary.BigEndian.Uint64(data[offset:])
	offset += 8
	retire := binary.BigEndian.Uint64(data[offset:])
	offset += 8
	cidLen := int(data[offset])
	offset++
	if cidLen > MaxCIDLength || offset+cidLen+StatelessResetTokenLength > len(data) {
		return nil, 0, fmt.Errorf("%w: NEW_CONNECTION_ID frame CID/token", ErrPacketTooShort)
	}
	cid, err := NewConnectionID(data[offset : offset+cidLen])
	if err != nil {
		return nil, 0, err
	}
	offset += cidLen
	var token [StatelessResetTokenLength]byte
	copy(token[:], data[offset:offset+StatelessResetTokenLength])
	offset += StatelessResetTokenLength

	return &NewConnectionIDFrame{
		SequenceNumber: seq,
		RetirePriorTo:  retire,
		ConnectionID:   cid,
		ResetToken:     token,
	}, offset - start, nil
}

// --- RETIRE_CONNECTION_ID Frame (0x11) ---

type RetireConnectionIDFrame struct {
	SequenceNumber uint64
}

func (f *RetireConnectionIDFrame) Type() FrameType { return FrameRetireConnectionID }
func (f *RetireConnectionIDFrame) Len() int        { return 9 }

func (f *RetireConnectionIDFrame) Marshal() []byte {
	buf := make([]byte, 9)
	buf[0] = byte(FrameRetireConnectionID)
	binary.BigEndian.PutUint64(buf[1:], f.SequenceNumber)
	return buf
}

func unmarshalRetireConnectionIDFrame(data []byte, offset int) (*RetireConnectionIDFrame, int, error) {
	if offset+9 > len(data) {
		return nil, 0, fmt.Errorf("%w: RETIRE_CONNECTION_ID frame", ErrPacketTooShort)
	}
	return &RetireConnectionIDFrame{
		SequenceNumber: binary.BigEndian.Uint64(data[offset+1:]),
	}, 9, nil
}
