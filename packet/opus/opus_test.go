package opus

import (
	"bytes"
	"errors"
	"testing"

	depacketopus "github.com/tphakala/go-audio-stream/depacket/opus"
)

func TestPacketizeRoundTripsDepacketize(t *testing.T) {
	pkt := []byte{0x78, 0x01, 0x02, 0x03} // arbitrary non-empty Opus packet
	payload, err := Packetize(pkt)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	back, err := depacketopus.Depacketize(payload)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if !bytes.Equal(back, pkt) {
		t.Errorf("round-trip: got %x, want %x", back, pkt)
	}
}

func TestPacketizeRejectsEmpty(t *testing.T) {
	if _, err := Packetize(nil); !errors.Is(err, ErrEmptyPacket) {
		t.Errorf("Packetize(nil) err = %v, want ErrEmptyPacket", err)
	}
}

func TestPacketizeAcceptsMultiFramePacket(t *testing.T) {
	// A code-3 VBR packet with four maximum-size frames is valid under RFC 6716
	// and must not be rejected as oversize.
	pkt := make([]byte, 1275*4+2+2*3)
	got, err := Packetize(pkt)
	if err != nil {
		t.Fatalf("Packetize(%d bytes) err = %v, want nil", len(pkt), err)
	}
	if len(got) != len(pkt) {
		t.Errorf("Packetize len = %d, want %d", len(got), len(pkt))
	}
}

func TestPacketizeRejectsOversize(t *testing.T) {
	if _, err := Packetize(make([]byte, maxPacketBytes+1)); !errors.Is(err, ErrOversizePacket) {
		t.Errorf("Packetize(oversize) err = %v, want ErrOversizePacket", err)
	}
}
