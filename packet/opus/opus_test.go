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

func TestPacketizeRejectsOversize(t *testing.T) {
	if _, err := Packetize(make([]byte, maxPacketBytes+1)); !errors.Is(err, ErrOversizePacket) {
		t.Errorf("Packetize(oversize) err = %v, want ErrOversizePacket", err)
	}
}
