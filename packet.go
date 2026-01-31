package udx

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Packet represents a UDX v2 packet with header and frames.
type Packet struct {
	Version             uint32
	DestinationCID      ConnectionID
	SourceCID           ConnectionID
	Sequence            uint32
	DestinationStreamID uint32
	SourceStreamID      uint32
	Frames              []Frame

	// Metadata (not serialized)
	SentTime time.Time
	IsAcked  bool
}

// minHeaderLen is the minimum header size: version(4) + dcidLen(1) + scidLen(1) + seq(4) + dstStream(4) + srcStream(4)
const minHeaderLen = 18

// MarshalPacket serializes a packet to bytes.
func MarshalPacket(p *Packet) []byte {
	// Calculate header size
	headerLen := 4 + 1 + p.DestinationCID.Len() + 1 + p.SourceCID.Len() + 4 + 4 + 4

	// Calculate frames size
	framesLen := 0
	for _, f := range p.Frames {
		framesLen += f.Len()
	}

	buf := make([]byte, headerLen+framesLen)
	offset := 0

	// Version
	binary.BigEndian.PutUint32(buf[offset:], p.Version)
	offset += 4

	// Destination CID
	buf[offset] = byte(p.DestinationCID.Len())
	offset++
	copy(buf[offset:], p.DestinationCID.Bytes())
	offset += p.DestinationCID.Len()

	// Source CID
	buf[offset] = byte(p.SourceCID.Len())
	offset++
	copy(buf[offset:], p.SourceCID.Bytes())
	offset += p.SourceCID.Len()

	// Sequence
	binary.BigEndian.PutUint32(buf[offset:], p.Sequence)
	offset += 4

	// Destination Stream ID
	binary.BigEndian.PutUint32(buf[offset:], p.DestinationStreamID)
	offset += 4

	// Source Stream ID
	binary.BigEndian.PutUint32(buf[offset:], p.SourceStreamID)
	offset += 4

	// Frames
	for _, f := range p.Frames {
		fb := f.Marshal()
		copy(buf[offset:], fb)
		offset += len(fb)
	}

	return buf
}

// UnmarshalPacket parses a packet from bytes.
func UnmarshalPacket(data []byte) (*Packet, error) {
	if len(data) < minHeaderLen {
		return nil, fmt.Errorf("%w: need at least %d bytes, got %d", ErrPacketTooShort, minHeaderLen, len(data))
	}

	offset := 0

	// Version
	version := binary.BigEndian.Uint32(data[offset:])
	offset += 4

	// Destination CID
	dcidLen := int(data[offset])
	offset++
	if dcidLen < MinCIDLength || dcidLen > MaxCIDLength {
		return nil, fmt.Errorf("%w: destination CID length %d", ErrInvalidCIDLength, dcidLen)
	}
	if offset+dcidLen > len(data) {
		return nil, fmt.Errorf("%w: destination CID data", ErrPacketTooShort)
	}
	dcid, err := NewConnectionID(data[offset : offset+dcidLen])
	if err != nil {
		return nil, err
	}
	offset += dcidLen

	// Source CID
	if offset >= len(data) {
		return nil, fmt.Errorf("%w: source CID length", ErrPacketTooShort)
	}
	scidLen := int(data[offset])
	offset++
	if scidLen < MinCIDLength || scidLen > MaxCIDLength {
		return nil, fmt.Errorf("%w: source CID length %d", ErrInvalidCIDLength, scidLen)
	}
	if offset+scidLen > len(data) {
		return nil, fmt.Errorf("%w: source CID data", ErrPacketTooShort)
	}
	scid, err := NewConnectionID(data[offset : offset+scidLen])
	if err != nil {
		return nil, err
	}
	offset += scidLen

	// Sequence + stream IDs
	if offset+12 > len(data) {
		return nil, fmt.Errorf("%w: sequence and stream IDs", ErrPacketTooShort)
	}
	seq := binary.BigEndian.Uint32(data[offset:])
	offset += 4
	dstStreamID := binary.BigEndian.Uint32(data[offset:])
	offset += 4
	srcStreamID := binary.BigEndian.Uint32(data[offset:])
	offset += 4

	// Parse frames
	var frames []Frame
	for offset < len(data) {
		frame, consumed, err := UnmarshalFrame(data, offset)
		if err != nil {
			return nil, fmt.Errorf("parsing frame at offset %d: %w", offset, err)
		}
		frames = append(frames, frame)
		offset += consumed
	}

	return &Packet{
		Version:             version,
		DestinationCID:      dcid,
		SourceCID:           scid,
		Sequence:            seq,
		DestinationStreamID: dstStreamID,
		SourceStreamID:      srcStreamID,
		Frames:              frames,
	}, nil
}
