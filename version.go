package udx

import (
	"encoding/binary"
	"fmt"
)

// SupportedVersions lists all supported versions in preference order.
var SupportedVersions = []uint32{VersionV2, VersionV1}

// IsVersionSupported checks if a version is supported.
func IsVersionSupported(version uint32) bool {
	for _, v := range SupportedVersions {
		if v == version {
			return true
		}
	}
	return false
}

// NegotiateVersion finds the highest common version between client and server.
// Returns 0 if no common version exists.
func NegotiateVersion(clientVersions []uint32) uint32 {
	for _, sv := range SupportedVersions {
		for _, cv := range clientVersions {
			if sv == cv {
				return sv
			}
		}
	}
	return 0
}

// VersionNegotiationPacket is sent by the server when the client
// requests an unsupported version. It has version=0 in the header.
type VersionNegotiationPacket struct {
	DestinationCID    ConnectionID
	SourceCID         ConnectionID
	SupportedVersions []uint32
}

// MarshalVersionNegotiation serializes a version negotiation packet.
func (p *VersionNegotiationPacket) Marshal() []byte {
	size := 4 + 1 + p.DestinationCID.Len() + 1 + p.SourceCID.Len() + len(p.SupportedVersions)*4
	buf := make([]byte, size)
	offset := 0

	// Version = 0 for version negotiation
	binary.BigEndian.PutUint32(buf[offset:], 0)
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

	// Supported versions
	for _, v := range p.SupportedVersions {
		binary.BigEndian.PutUint32(buf[offset:], v)
		offset += 4
	}

	return buf
}

// UnmarshalVersionNegotiation parses a version negotiation packet.
func UnmarshalVersionNegotiation(data []byte) (*VersionNegotiationPacket, error) {
	if len(data) < 6 { // version(4) + dcidLen(1) + scidLen(1) minimum
		return nil, fmt.Errorf("%w: version negotiation packet", ErrPacketTooShort)
	}

	offset := 0
	version := binary.BigEndian.Uint32(data[offset:])
	if version != 0 {
		return nil, fmt.Errorf("invalid version negotiation packet: version != 0")
	}
	offset += 4

	dcidLen := int(data[offset])
	offset++
	if dcidLen > MaxCIDLength || offset+dcidLen > len(data) {
		return nil, ErrInvalidCIDLength
	}
	dcid, err := NewConnectionID(data[offset : offset+dcidLen])
	if err != nil {
		return nil, err
	}
	offset += dcidLen

	if offset >= len(data) {
		return nil, fmt.Errorf("%w: version negotiation packet", ErrPacketTooShort)
	}
	scidLen := int(data[offset])
	offset++
	if scidLen > MaxCIDLength || offset+scidLen > len(data) {
		return nil, ErrInvalidCIDLength
	}
	scid, err := NewConnectionID(data[offset : offset+scidLen])
	if err != nil {
		return nil, err
	}
	offset += scidLen

	var versions []uint32
	for offset+4 <= len(data) {
		versions = append(versions, binary.BigEndian.Uint32(data[offset:]))
		offset += 4
	}

	return &VersionNegotiationPacket{
		DestinationCID:    dcid,
		SourceCID:         scid,
		SupportedVersions: versions,
	}, nil
}
