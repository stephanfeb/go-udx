package udx

import (
	"bytes"
	"testing"
)

func mustCID(t *testing.T, b []byte) ConnectionID {
	t.Helper()
	cid, err := NewConnectionID(b)
	if err != nil {
		t.Fatalf("NewConnectionID: %v", err)
	}
	return cid
}

func TestConnectionID(t *testing.T) {
	t.Run("random", func(t *testing.T) {
		cid, err := RandomConnectionID(DefaultCIDLength)
		if err != nil {
			t.Fatal(err)
		}
		if cid.Len() != DefaultCIDLength {
			t.Fatalf("expected len %d, got %d", DefaultCIDLength, cid.Len())
		}
	})

	t.Run("zero length", func(t *testing.T) {
		cid, err := NewConnectionID([]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if !cid.IsZero() {
			t.Fatal("expected zero CID")
		}
	})

	t.Run("equality", func(t *testing.T) {
		a := mustCID(t, []byte{1, 2, 3, 4})
		b := mustCID(t, []byte{1, 2, 3, 4})
		c := mustCID(t, []byte{1, 2, 3, 5})
		if !a.Equal(b) {
			t.Fatal("expected equal")
		}
		if a.Equal(c) {
			t.Fatal("expected not equal")
		}
	})

	t.Run("invalid length", func(t *testing.T) {
		_, err := NewConnectionID(make([]byte, MaxCIDLength+1))
		if err == nil {
			t.Fatal("expected error for oversized CID")
		}
	})
}

func TestFrameRoundTrip(t *testing.T) {
	dcid := mustCID(t, []byte{0xaa, 0xbb, 0xcc, 0xdd, 0x11, 0x22, 0x33, 0x44})
	scid := mustCID(t, []byte{0x55, 0x66, 0x77, 0x88})

	frames := []Frame{
		&PaddingFrame{},
		&PingFrame{},
		&AckFrame{
			LargestAcked:       42,
			AckDelay:           10,
			FirstAckRangeLength: 5,
			AckRanges: []AckRange{
				{Gap: 2, AckRangeLength: 3},
				{Gap: 1, AckRangeLength: 7},
			},
		},
		&StreamFrame{IsFin: false, IsSyn: true, Data: []byte("hello world")},
		&StreamFrame{IsFin: true, IsSyn: false, Data: []byte{}},
		&WindowUpdateFrame{WindowSize: 65536},
		&MaxDataFrame{MaxData: 1048576},
		&ResetStreamFrame{ErrorCode: ErrorFlowControlError},
		&MaxStreamsFrame{MaxStreamCount: 100},
		&MTUProbeFrame{ProbeSize: 50},
		&PathChallengeFrame{Data: [8]byte{1, 2, 3, 4, 5, 6, 7, 8}},
		&PathResponseFrame{Data: [8]byte{8, 7, 6, 5, 4, 3, 2, 1}},
		&ConnectionCloseFrame{ErrorCode: ErrorNoError, FrameTypeVal: 0, ReasonPhrase: "goodbye"},
		&ConnectionCloseFrame{ErrorCode: ErrorInternalError, FrameTypeVal: 3, ReasonPhrase: ""},
		&StopSendingFrame{StreamID: 7, ErrorCode: ErrorNoError},
		&DataBlockedFrame{MaxData: 999999},
		&StreamDataBlockedFrame{StreamID: 5, MaxStreamData: 123456},
		&NewConnectionIDFrame{
			SequenceNumber: 1,
			RetirePriorTo:  0,
			ConnectionID:   mustCID(t, []byte{0xde, 0xad, 0xbe, 0xef}),
			ResetToken:     [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		},
		&RetireConnectionIDFrame{SequenceNumber: 0},
	}

	for _, origFrame := range frames {
		t.Run(frameTypeName(origFrame.Type()), func(t *testing.T) {
			// Test individual frame marshal/unmarshal
			marshaled := origFrame.Marshal()
			if len(marshaled) != origFrame.Len() {
				t.Fatalf("Marshal() len %d != Len() %d", len(marshaled), origFrame.Len())
			}
			parsed, consumed, err := UnmarshalFrame(marshaled, 0)
			if err != nil {
				t.Fatalf("UnmarshalFrame: %v", err)
			}
			if consumed != len(marshaled) {
				t.Fatalf("consumed %d != marshaled len %d", consumed, len(marshaled))
			}
			reMarshaled := parsed.Marshal()
			if !bytes.Equal(marshaled, reMarshaled) {
				t.Fatalf("round-trip mismatch:\n  orig: %x\n  got:  %x", marshaled, reMarshaled)
			}

			// Test in a full packet
			pkt := &Packet{
				Version:             VersionV2,
				DestinationCID:      dcid,
				SourceCID:           scid,
				Sequence:            100,
				DestinationStreamID: 1,
				SourceStreamID:      2,
				Frames:              []Frame{origFrame},
			}
			pktBytes := MarshalPacket(pkt)
			parsed2, err := UnmarshalPacket(pktBytes)
			if err != nil {
				t.Fatalf("UnmarshalPacket: %v", err)
			}
			if parsed2.Version != VersionV2 {
				t.Fatalf("version: got %d, want %d", parsed2.Version, VersionV2)
			}
			if !parsed2.DestinationCID.Equal(dcid) {
				t.Fatalf("dst CID mismatch")
			}
			if !parsed2.SourceCID.Equal(scid) {
				t.Fatalf("src CID mismatch")
			}
			if parsed2.Sequence != 100 {
				t.Fatalf("sequence: got %d, want 100", parsed2.Sequence)
			}
			if len(parsed2.Frames) != 1 {
				t.Fatalf("expected 1 frame, got %d", len(parsed2.Frames))
			}
			if parsed2.Frames[0].Type() != origFrame.Type() {
				t.Fatalf("frame type: got %d, want %d", parsed2.Frames[0].Type(), origFrame.Type())
			}
			reBytes := MarshalPacket(parsed2)
			if !bytes.Equal(pktBytes, reBytes) {
				t.Fatalf("packet round-trip mismatch:\n  orig: %x\n  got:  %x", pktBytes, reBytes)
			}
		})
	}
}

func TestPacketMultipleFrames(t *testing.T) {
	dcid := mustCID(t, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	scid := mustCID(t, []byte{9, 10, 11, 12, 13, 14, 15, 16})

	pkt := &Packet{
		Version:             VersionV2,
		DestinationCID:      dcid,
		SourceCID:           scid,
		Sequence:            42,
		DestinationStreamID: 10,
		SourceStreamID:      20,
		Frames: []Frame{
			&StreamFrame{IsSyn: true, Data: []byte("syn")},
			&AckFrame{LargestAcked: 5, AckDelay: 1, FirstAckRangeLength: 3},
			&PingFrame{},
		},
	}

	data := MarshalPacket(pkt)
	parsed, err := UnmarshalPacket(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Frames) != 3 {
		t.Fatalf("expected 3 frames, got %d", len(parsed.Frames))
	}
	if parsed.Frames[0].Type() != FrameStream {
		t.Fatalf("frame 0: expected STREAM, got %d", parsed.Frames[0].Type())
	}
	if parsed.Frames[1].Type() != FrameAck {
		t.Fatalf("frame 1: expected ACK, got %d", parsed.Frames[1].Type())
	}
	if parsed.Frames[2].Type() != FramePing {
		t.Fatalf("frame 2: expected PING, got %d", parsed.Frames[2].Type())
	}
}

func TestPacketZeroCIDs(t *testing.T) {
	dcid, _ := NewConnectionID([]byte{})
	scid, _ := NewConnectionID([]byte{})

	pkt := &Packet{
		Version:        VersionV2,
		DestinationCID: dcid,
		SourceCID:      scid,
		Sequence:       0,
		Frames:         []Frame{&PingFrame{}},
	}

	data := MarshalPacket(pkt)
	parsed, err := UnmarshalPacket(data)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.DestinationCID.IsZero() || !parsed.SourceCID.IsZero() {
		t.Fatal("expected zero CIDs")
	}
}

func TestPacketTooShort(t *testing.T) {
	_, err := UnmarshalPacket([]byte{0, 0, 0, 2})
	if err == nil {
		t.Fatal("expected error for short packet")
	}
}

func TestVersionNegotiationRoundTrip(t *testing.T) {
	dcid := mustCID(t, []byte{1, 2, 3, 4})
	scid := mustCID(t, []byte{5, 6, 7, 8})

	pkt := &VersionNegotiationPacket{
		DestinationCID:    dcid,
		SourceCID:         scid,
		SupportedVersions: SupportedVersions,
	}

	data := pkt.Marshal()
	parsed, err := UnmarshalVersionNegotiation(data)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.DestinationCID.Equal(dcid) {
		t.Fatal("dst CID mismatch")
	}
	if !parsed.SourceCID.Equal(scid) {
		t.Fatal("src CID mismatch")
	}
	if len(parsed.SupportedVersions) != len(SupportedVersions) {
		t.Fatalf("version count: got %d, want %d", len(parsed.SupportedVersions), len(SupportedVersions))
	}
	for i, v := range parsed.SupportedVersions {
		if v != SupportedVersions[i] {
			t.Fatalf("version %d: got %d, want %d", i, v, SupportedVersions[i])
		}
	}
}

func TestAckFrameWithRanges(t *testing.T) {
	f := &AckFrame{
		LargestAcked:       100,
		AckDelay:           5,
		FirstAckRangeLength: 10, // acks 91-100
		AckRanges: []AckRange{
			{Gap: 3, AckRangeLength: 5}, // gap of 3 below 91, then 5 acked
			{Gap: 1, AckRangeLength: 2}, // gap of 1, then 2 acked
		},
	}

	data := f.Marshal()
	parsed, consumed, err := UnmarshalFrame(data, 0)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != len(data) {
		t.Fatalf("consumed %d != %d", consumed, len(data))
	}
	ack := parsed.(*AckFrame)
	if ack.LargestAcked != 100 {
		t.Fatalf("largestAcked: got %d", ack.LargestAcked)
	}
	if len(ack.AckRanges) != 2 {
		t.Fatalf("ranges: got %d", len(ack.AckRanges))
	}
	if ack.AckRanges[0].Gap != 3 || ack.AckRanges[0].AckRangeLength != 5 {
		t.Fatalf("range 0: gap=%d len=%d", ack.AckRanges[0].Gap, ack.AckRanges[0].AckRangeLength)
	}
}

func frameTypeName(ft FrameType) string {
	names := map[FrameType]string{
		FramePadding:            "PADDING",
		FramePing:               "PING",
		FrameAck:                "ACK",
		FrameStream:             "STREAM",
		FrameWindowUpdate:       "WINDOW_UPDATE",
		FrameMaxData:            "MAX_DATA",
		FrameResetStream:        "RESET_STREAM",
		FrameMaxStreams:          "MAX_STREAMS",
		FrameMTUProbe:           "MTU_PROBE",
		FramePathChallenge:      "PATH_CHALLENGE",
		FramePathResponse:       "PATH_RESPONSE",
		FrameConnectionClose:    "CONNECTION_CLOSE",
		FrameStopSending:        "STOP_SENDING",
		FrameDataBlocked:        "DATA_BLOCKED",
		FrameStreamDataBlocked:  "STREAM_DATA_BLOCKED",
		FrameNewConnectionID:    "NEW_CONNECTION_ID",
		FrameRetireConnectionID: "RETIRE_CONNECTION_ID",
	}
	if name, ok := names[ft]; ok {
		return name
	}
	return "UNKNOWN"
}
