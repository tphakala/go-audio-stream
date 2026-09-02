package flac_test

import (
	"bytes"
	"errors"
	"testing"

	depacketflac "github.com/tphakala/go-audio-stream/depacket/flac"
	packetflac "github.com/tphakala/go-audio-stream/packet/flac"
)

// A frame that fits in one payload yields a single fragment with the marker set,
// aliasing the frame.
func TestPacketizeUnfragmented(t *testing.T) {
	frame := []byte{0xFF, 0xF8, 0x01, 0x02, 0x03}
	frags, err := packetflac.Packetize(nil, frame, 1500)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(frags) != 1 {
		t.Fatalf("got %d fragments, want 1", len(frags))
	}
	if !frags[0].Marker {
		t.Error("single fragment must have Marker set")
	}
	if !bytes.Equal(frags[0].Data, frame) || &frags[0].Data[0] != &frame[0] {
		t.Error("single fragment should alias the whole frame")
	}
}

// A frame larger than maxPayload is split into ordered fragments, marker on the
// last only, sizes maxPayload except the remainder.
func TestPacketizeFragmented(t *testing.T) {
	tests := []struct {
		name       string
		frameLen   int
		maxPayload int
		wantFrags  int
		wantLast   int
	}{
		{"exact multiple", 12, 4, 3, 4},
		{"remainder", 10, 4, 3, 2},
		{"tiny mtu", 5, 1, 5, 1},
		{"one under", 7, 4, 2, 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			frame := make([]byte, tc.frameLen)
			for i := range frame {
				frame[i] = byte(i)
			}
			frags, err := packetflac.Packetize(nil, frame, tc.maxPayload)
			if err != nil {
				t.Fatalf("Packetize: %v", err)
			}
			if len(frags) != tc.wantFrags {
				t.Fatalf("got %d fragments, want %d", len(frags), tc.wantFrags)
			}
			for i, fr := range frags {
				last := i == len(frags)-1
				if fr.Marker != last {
					t.Errorf("fragment %d Marker = %v, want %v", i, fr.Marker, last)
				}
				wantLen := tc.maxPayload
				if last {
					wantLen = tc.wantLast
				}
				if len(fr.Data) != wantLen {
					t.Errorf("fragment %d len = %d, want %d", i, len(fr.Data), wantLen)
				}
			}
			// The fragments must concatenate back to the original frame.
			var got []byte
			for _, fr := range frags {
				got = append(got, fr.Data...)
			}
			if !bytes.Equal(got, frame) {
				t.Errorf("concatenated = %x, want %x", got, frame)
			}
		})
	}
}

func TestPacketizeErrors(t *testing.T) {
	if _, err := packetflac.Packetize(nil, []byte{1, 2}, 0); !errors.Is(err, packetflac.ErrInvalidMTU) {
		t.Errorf("maxPayload 0 err = %v, want ErrInvalidMTU", err)
	}
	if _, err := packetflac.Packetize(nil, []byte{1, 2}, -1); !errors.Is(err, packetflac.ErrInvalidMTU) {
		t.Errorf("negative maxPayload err = %v, want ErrInvalidMTU", err)
	}
	if _, err := packetflac.Packetize(nil, nil, 1500); !errors.Is(err, packetflac.ErrEmptyFrame) {
		t.Errorf("empty frame err = %v, want ErrEmptyFrame", err)
	}
}

// Packetize appends into a reused slice so a sender allocates no fragment slice
// per frame.
func TestPacketizeReusesDst(t *testing.T) {
	frame := make([]byte, 100)
	dst := make([]packetflac.Fragment, 0, 8)
	// Capture the backing array's first element via a full-capacity reslice
	// (valid because reslicing is bounded by cap, not len). If Packetize appends
	// in place, the returned slice's first element is this same address.
	base := &dst[:cap(dst)][0]
	got, err := packetflac.Packetize(dst, frame, 30)
	if err != nil {
		t.Fatalf("Packetize: %v", err)
	}
	if len(got) == 0 || &got[0] != base {
		t.Error("Packetize should append into the provided backing array, not allocate a new one")
	}
}

// The packetizer and the depacketizer are inverses: fragmenting a frame and
// feeding the fragments (with their markers) to the depacketizer in order
// reconstructs the original frame.
func TestRoundTripThroughDepacketizer(t *testing.T) {
	for _, frameLen := range []int{1, 4, 5, 100, 1500, 4097} {
		for _, mtu := range []int{1, 7, 1400, 65535} {
			frame := make([]byte, frameLen)
			for i := range frame {
				frame[i] = byte(i*7 + 1)
			}
			frags, err := packetflac.Packetize(nil, frame, mtu)
			if err != nil {
				t.Fatalf("Packetize(len=%d, mtu=%d): %v", frameLen, mtu, err)
			}
			d := depacketflac.New()
			var out []byte
			// All fragments of one frame share the frame's RTP timestamp.
			const frameTS = uint32(42)
			for i, fr := range frags {
				got, derr := d.Depacketize(fr.Data, fr.Marker, frameTS)
				if derr != nil {
					t.Fatalf("Depacketize frag %d (len=%d, mtu=%d): %v", i, frameLen, mtu, derr)
				}
				if got != nil {
					out = got
				}
			}
			if !bytes.Equal(out, frame) {
				t.Errorf("round trip (len=%d, mtu=%d) = %x, want %x", frameLen, mtu, out, frame)
			}
		}
	}
}
