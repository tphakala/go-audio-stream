package opus_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tphakala/go-audio-stream/depacket/opus"
)

func TestDepacketizeIdentity(t *testing.T) {
	t.Parallel()
	// A plausible Opus packet: TOC byte plus some frame bytes. The value
	// is opaque to this package; identity is the whole contract.
	pkt := []byte{0x78, 0x01, 0x02, 0x03, 0x04}
	got, err := opus.Depacketize(pkt)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if !bytes.Equal(got, pkt) {
		t.Errorf("Depacketize = % x, want % x", got, pkt)
	}
}

func TestDepacketizeEmpty(t *testing.T) {
	t.Parallel()
	if _, err := opus.Depacketize(nil); !errors.Is(err, opus.ErrEmptyPayload) {
		t.Errorf("nil payload: err = %v, want ErrEmptyPayload", err)
	}
	if _, err := opus.Depacketize([]byte{}); !errors.Is(err, opus.ErrEmptyPayload) {
		t.Errorf("empty payload: err = %v, want ErrEmptyPayload", err)
	}
}

func TestDepacketizeSingleByte(t *testing.T) {
	t.Parallel()
	// A one-byte Opus packet (TOC only) is valid at this layer.
	got, err := opus.Depacketize([]byte{0x08})
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if !bytes.Equal(got, []byte{0x08}) {
		t.Errorf("Depacketize = % x, want 08", got)
	}
}
