package udx

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// ConnectionID represents a UDX Connection Identifier (0-20 bytes).
type ConnectionID struct {
	data [MaxCIDLength]byte
	len  int
}

// NewConnectionID creates a ConnectionID from a byte slice.
func NewConnectionID(b []byte) (ConnectionID, error) {
	if len(b) < MinCIDLength || len(b) > MaxCIDLength {
		return ConnectionID{}, fmt.Errorf("%w: got %d, want %d-%d", ErrInvalidCIDLength, len(b), MinCIDLength, MaxCIDLength)
	}
	var cid ConnectionID
	cid.len = len(b)
	copy(cid.data[:cid.len], b)
	return cid, nil
}

// RandomConnectionID generates a random ConnectionID with the given length.
func RandomConnectionID(length int) (ConnectionID, error) {
	if length < MinCIDLength || length > MaxCIDLength {
		return ConnectionID{}, fmt.Errorf("%w: got %d, want %d-%d", ErrInvalidCIDLength, length, MinCIDLength, MaxCIDLength)
	}
	var cid ConnectionID
	cid.len = length
	if length > 0 {
		if _, err := rand.Read(cid.data[:length]); err != nil {
			return ConnectionID{}, fmt.Errorf("generating random CID: %w", err)
		}
	}
	return cid, nil
}

// Len returns the length of the CID in bytes.
func (c ConnectionID) Len() int { return c.len }

// Bytes returns a copy of the CID bytes.
func (c ConnectionID) Bytes() []byte {
	b := make([]byte, c.len)
	copy(b, c.data[:c.len])
	return b
}

// IsZero returns true if this is a zero-length CID.
func (c ConnectionID) IsZero() bool { return c.len == 0 }

// Equal returns true if two CIDs are identical.
func (c ConnectionID) Equal(other ConnectionID) bool {
	if c.len != other.len {
		return false
	}
	for i := 0; i < c.len; i++ {
		if c.data[i] != other.data[i] {
			return false
		}
	}
	return true
}

// String returns a hex representation of the CID.
func (c ConnectionID) String() string {
	return "CID(" + hex.EncodeToString(c.data[:c.len]) + ")"
}

// ConnectionCIDs holds the local and remote CIDs for a connection.
type ConnectionCIDs struct {
	Local  ConnectionID
	Remote ConnectionID
}
