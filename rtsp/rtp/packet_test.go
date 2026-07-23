package rtp_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

func TestParsePacketBasic(t *testing.T) {
	t.Parallel()
	// Reference layout, documented byte by byte:
	//   80 E1 00 11 00 00 27 10 11 22 33 44 00 10 DE AD
	//   80          V=2 P=0 X=0 CC=0
	//   E1          M=1 PT=97
	//   00 11       sequence 17
	//   00 00 27 10 timestamp 10000
	//   11 22 33 44 SSRC 0x11223344
	//   00 10 DE AD payload (4 bytes)
	buf := []byte{0x80, 0xE1, 0x00, 0x11, 0x00, 0x00, 0x27, 0x10,
		0x11, 0x22, 0x33, 0x44, 0x00, 0x10, 0xDE, 0xAD}
	p, err := rtp.ParsePacket(buf)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	h := p.Header
	if h.Version != 2 || !h.Marker || h.PayloadType != 97 {
		t.Errorf("header flags = %+v", h)
	}
	if h.SequenceNumber != 17 || h.Timestamp != 10000 || h.SSRC != 0x11223344 {
		t.Errorf("header fields = %+v", h)
	}
	if h.Padding || h.Extension || h.CSRC != nil {
		t.Errorf("unexpected padding/extension/csrc = %+v", h)
	}
	if !bytes.Equal(p.Payload, []byte{0x00, 0x10, 0xDE, 0xAD}) {
		t.Errorf("payload = % x", p.Payload)
	}
}

func TestParsePacketPaddingCSRCExtension(t *testing.T) {
	t.Parallel()
	payload := []byte{0x00, 0x10, 0xDE, 0xAD}

	pad := buildRTP(false, 97, 1, 0, 0x11223344, nil, nil, payload, 3)
	if p, err := rtp.ParsePacket(pad); err != nil || !bytes.Equal(p.Payload, payload) {
		t.Errorf("padding: payload=% x err=%v", p.Payload, err)
	}

	cs := buildRTP(false, 97, 1, 0, 0x11223344, []uint32{0xAAAAAAAA, 0xBBBBBBBB}, nil, payload, 0)
	p, err := rtp.ParsePacket(cs)
	if err != nil {
		t.Fatalf("csrc: %v", err)
	}
	if len(p.Header.CSRC) != 2 || p.Header.CSRC[0] != 0xAAAAAAAA || p.Header.CSRC[1] != 0xBBBBBBBB {
		t.Errorf("csrc = %v", p.Header.CSRC)
	}
	if !bytes.Equal(p.Payload, payload) {
		t.Errorf("csrc payload = % x", p.Payload)
	}

	ex := buildRTP(false, 97, 1, 0, 0x11223344, nil, []byte{0x12, 0x34, 0x56, 0x78}, payload, 0)
	p, err = rtp.ParsePacket(ex)
	if err != nil || !p.Header.Extension || !bytes.Equal(p.Payload, payload) {
		t.Errorf("extension: hdr=%+v payload=% x err=%v", p.Header, p.Payload, err)
	}
}

func TestParsePacketErrors(t *testing.T) {
	t.Parallel()
	if _, err := rtp.ParsePacket([]byte{0x80, 0xE1, 0x00}); !errors.Is(err, rtp.ErrShortPacket) {
		t.Errorf("short header err = %v, want ErrShortPacket", err)
	}
	// Version 1 (top two bits 01) must be rejected.
	bad := []byte{0x40, 0xE1, 0x00, 0x11, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, err := rtp.ParsePacket(bad); !errors.Is(err, rtp.ErrVersion) {
		t.Errorf("version err = %v, want ErrVersion", err)
	}
	// P set but padding length byte (5) exceeds the 1-byte payload region.
	padbad := []byte{0xA0, 0xE1, 0x00, 0x11, 0, 0, 0, 0, 0, 0, 0, 0, 0x05}
	if _, err := rtp.ParsePacket(padbad); !errors.Is(err, rtp.ErrBadPadding) {
		t.Errorf("padding err = %v, want ErrBadPadding", err)
	}
	// CC=1 declared but no CSRC bytes present.
	csbad := []byte{0x81, 0xE1, 0x00, 0x11, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, err := rtp.ParsePacket(csbad); !errors.Is(err, rtp.ErrShortPacket) {
		t.Errorf("truncated csrc err = %v, want ErrShortPacket", err)
	}
	// X set, extension length says 1 word but no extension bytes present.
	exbad := []byte{0x90, 0xE1, 0x00, 0x11, 0, 0, 0, 0, 0, 0, 0, 0, 0xBE, 0xDE, 0x00, 0x01}
	if _, err := rtp.ParsePacket(exbad); !errors.Is(err, rtp.ErrTruncatedExtension) {
		t.Errorf("truncated ext err = %v, want ErrTruncatedExtension", err)
	}
}
