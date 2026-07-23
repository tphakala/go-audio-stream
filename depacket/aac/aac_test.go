package aac_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tphakala/go-audio-stream/depacket/aac"
)

// hbrConfig is the ubiquitous AAC-hbr configuration: 13-bit AU-size,
// 3-bit AU-Index and AU-Index-delta, 1024 samples per frame.
func hbrConfig() aac.Config {
	return aac.Config{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024}
}

// auHeader16 packs one AAC-hbr AU-header into 16 bits, big-endian: the
// 13-bit size in the high bits and the 3-bit index (or index-delta) in
// the low bits.
func auHeader16(size, index int) []byte {
	v := uint16(size<<3) | uint16(index&0x07)
	return []byte{byte(v >> 8), byte(v)}
}

// buildHBR assembles an AAC-hbr payload from 16-bit headers and their
// data blocks: a 16-bit AU-headers-length (in bits) = 16*len(headers),
// then the packed headers, then the concatenated data.
func buildHBR(headers, data [][]byte) []byte {
	bits := len(headers) * 16
	out := []byte{byte(bits >> 8), byte(bits)}
	for _, h := range headers {
		out = append(out, h...)
	}
	for _, d := range data {
		out = append(out, d...)
	}
	return out
}

func newDepacketizer(t *testing.T) *aac.Depacketizer {
	t.Helper()
	d, err := aac.New(hbrConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestSingleAU(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	data := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	// AU-headers-length 16; header size=4 index=0; then 4 data bytes.
	pkt := buildHBR([][]byte{auHeader16(4, 0)}, [][]byte{data})
	// Bytes: 00 10 00 20 AA BB CC DD.
	aus, err := d.Depacketize(pkt, true, 1000)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 1 {
		t.Fatalf("AU count = %d, want 1", len(aus))
	}
	if !bytes.Equal(aus[0].Data, data) {
		t.Errorf("Data = % x, want % x", aus[0].Data, data)
	}
	if aus[0].RTPOffset != 0 {
		t.Errorf("RTPOffset = %d, want 0", aus[0].RTPOffset)
	}
}

func TestMultiAU(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	d0 := []byte{0x11, 0x22, 0x33}
	d1 := []byte{0x44, 0x55, 0x66, 0x77, 0x88}
	// Two AUs, sizes 3 and 5, both index-delta 0 (consecutive).
	pkt := buildHBR(
		[][]byte{auHeader16(3, 0), auHeader16(5, 0)},
		[][]byte{d0, d1},
	)
	aus, err := d.Depacketize(pkt, true, 2000)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 2 {
		t.Fatalf("AU count = %d, want 2", len(aus))
	}
	if !bytes.Equal(aus[0].Data, d0) || aus[0].RTPOffset != 0 {
		t.Errorf("AU0 = % x @ %d, want % x @ 0", aus[0].Data, aus[0].RTPOffset, d0)
	}
	if !bytes.Equal(aus[1].Data, d1) || aus[1].RTPOffset != 1024 {
		t.Errorf("AU1 = % x @ %d, want % x @ 1024", aus[1].Data, aus[1].RTPOffset, d1)
	}
}

func TestThreeAUOffsets(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	blocks := [][]byte{{0xA1, 0xA2}, {0xB1, 0xB2}, {0xC1, 0xC2}}
	pkt := buildHBR(
		[][]byte{auHeader16(2, 0), auHeader16(2, 0), auHeader16(2, 0)},
		blocks,
	)
	aus, err := d.Depacketize(pkt, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	want := []uint32{0, 1024, 2048}
	if len(aus) != 3 {
		t.Fatalf("AU count = %d, want 3", len(aus))
	}
	for i, au := range aus {
		if au.RTPOffset != want[i] {
			t.Errorf("AU%d RTPOffset = %d, want %d", i, au.RTPOffset, want[i])
		}
	}
}

func TestInterleavingRejected(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// Second header has index-delta 1: interleaving.
	pkt := buildHBR(
		[][]byte{auHeader16(3, 0), auHeader16(5, 1)},
		[][]byte{{1, 2, 3}, {4, 5, 6, 7, 8}},
	)
	_, err := d.Depacketize(pkt, true, 0)
	if !errors.Is(err, aac.ErrInterleavingUnsupported) {
		t.Fatalf("err = %v, want ErrInterleavingUnsupported", err)
	}
}

func TestTruncatedHeader(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// Declares one 16-bit header (length 16) but supplies only one
	// header byte.
	pkt := []byte{0x00, 0x10, 0x00}
	_, err := d.Depacketize(pkt, true, 0)
	if !errors.Is(err, aac.ErrTruncatedHeader) {
		t.Fatalf("err = %v, want ErrTruncatedHeader", err)
	}
	// Too short to even hold the 16-bit length.
	_, err = d.Depacketize([]byte{0x00}, true, 0)
	if !errors.Is(err, aac.ErrTruncatedHeader) {
		t.Fatalf("err = %v, want ErrTruncatedHeader", err)
	}
	// Zero-length AU-headers section.
	_, err = d.Depacketize([]byte{0x00, 0x00, 0xAA}, true, 0)
	if !errors.Is(err, aac.ErrTruncatedHeader) {
		t.Fatalf("err = %v, want ErrTruncatedHeader (zero length)", err)
	}
}

func TestAUSizeOverflow(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// Single AU, marker set (claims complete), declares size 10 but only
	// 4 data bytes follow.
	pkt := buildHBR([][]byte{auHeader16(10, 0)}, [][]byte{{0xAA, 0xBB, 0xCC, 0xDD}})
	_, err := d.Depacketize(pkt, true, 0)
	if !errors.Is(err, aac.ErrAUSizeOverflow) {
		t.Fatalf("err = %v, want ErrAUSizeOverflow", err)
	}
}

func TestHugeAUHeadersLength(t *testing.T) {
	t.Parallel()
	d := newDepacketizer(t)
	// AU-headers-length 0xFFFF bits demands 8192 header bytes; the packet
	// has two. Rejected as truncated, no allocation blowup.
	pkt := []byte{0xFF, 0xFF, 0x00, 0x00}
	_, err := d.Depacketize(pkt, true, 0)
	if !errors.Is(err, aac.ErrTruncatedHeader) {
		t.Fatalf("err = %v, want ErrTruncatedHeader", err)
	}
}

func TestConfigRejection(t *testing.T) {
	t.Parallel()
	bad := []aac.Config{
		{SizeLength: 0, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024},
		{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 0},
		{SizeLength: 32, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024},
		{SizeLength: 33, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024},
		{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 100000},
		{SizeLength: 13, IndexLength: -1, IndexDeltaLength: 3, SamplesPerFrame: 1024},
	}
	for i, cfg := range bad {
		if _, err := aac.New(cfg); !errors.Is(err, aac.ErrConfigInvalid) {
			t.Errorf("case %d: err = %v, want ErrConfigInvalid", i, err)
		}
	}
	if _, err := aac.New(hbrConfig()); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}
}
