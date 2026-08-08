package latm

import (
	"bytes"
	"errors"
	"testing"
)

// v1 is the out-of-band StreamMuxConfig test vector (hand-verified,
// docs/plans/2026-07-23-phase2-latm-plan.md, vector V1): AAC-LC, 44.1 kHz,
// stereo, one subframe, frameLengthType 0. Decodes to ASC 12 10,
// frameLength 1024.
var v1 = []byte{0x40, 0x00, 0x24, 0x20, 0x3F, 0xC0}

// v1FrameLength960 is v1 with frameLengthFlag set to 1 instead of 0 (the
// ASC's 14th bit, which lands in byte3), asserting the derived frameLength
// switches from 1024 to 960. Built and cross-checked against v1 with a
// standalone bit packer that reproduces v1 byte for byte when
// frameLengthFlag is 0.
var v1FrameLength960 = []byte{0x40, 0x00, 0x24, 0x28, 0x3F, 0xC0}

// v1ExplicitRate is a StreamMuxConfig whose ASC uses
// samplingFrequencyIndex 15 (the escape code for an explicit 24-bit
// sampling rate, here 48000 Hz = 0x00BB80), aot 2, channelConfiguration 2,
// frameLengthFlag 0. The ASC bit range this parses out is 5 bytes:
// 17 80 5D C0 10.
var v1ExplicitRate = []byte{0x40, 0x00, 0x2F, 0x00, 0xBB, 0x80, 0x20, 0x3F, 0xC0}

// v1UnsupportedAOT is a StreamMuxConfig whose audioObjectType is 5 (SBR
// explicit signaling / SBC in this package's shorthand), which parseASC
// rejects before reading any further ASC fields.
var v1UnsupportedAOT = []byte{0x40, 0x00, 0x50}

// v4 is the in-band AudioMuxElement test vector (hand-verified,
// docs/plans/2026-07-23-phase2-latm-plan.md, vector V4): useSameStreamMux
// 0, an inline StreamMuxConfig equal to v1, PayloadLengthInfo 03, payload
// AA BB CC, ByteAlign padding.
var v4 = []byte{0x20, 0x00, 0x12, 0x10, 0x1F, 0xE0, 0x1D, 0x55, 0xDE, 0x60}

// v5 is the in-band AudioMuxElement test vector paired with v4
// (docs/plans/2026-07-23-phase2-latm-plan.md, vector V5): useSameStreamMux
// 1, PayloadLengthInfo 03, payload AA BB CC, ByteAlign padding. Decodes by
// reusing v4's retained StreamMuxConfig.
var v5 = []byte{0x81, 0xD5, 0x5D, 0xE6, 0x00}

func TestInBandSingleAU(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	aus, err := d.Depacketize(v4, true, 0)
	if err != nil {
		t.Fatalf("Depacketize: %v", err)
	}
	if len(aus) != 1 {
		t.Fatalf("AU count = %d, want 1", len(aus))
	}
	want := []byte{0xAA, 0xBB, 0xCC}
	if !bytes.Equal(aus[0].Data, want) {
		t.Errorf("Data = % x, want % x", aus[0].Data, want)
	}
	if aus[0].RTPOffset != 0 {
		t.Errorf("RTPOffset = %d, want 0", aus[0].RTPOffset)
	}

	asc := d.AudioSpecificConfig()
	wantASC := []byte{0x12, 0x10}
	if !bytes.Equal(asc, wantASC) {
		t.Errorf("AudioSpecificConfig = % x, want % x", asc, wantASC)
	}
}

func TestInBandReuseConfig(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.Depacketize(v4, true, 0); err != nil {
		t.Fatalf("Depacketize(v4): %v", err)
	}

	aus, err := d.Depacketize(v5, true, 0)
	if err != nil {
		t.Fatalf("Depacketize(v5): %v", err)
	}
	if len(aus) != 1 {
		t.Fatalf("AU count = %d, want 1", len(aus))
	}
	want := []byte{0xAA, 0xBB, 0xCC}
	if !bytes.Equal(aus[0].Data, want) {
		t.Errorf("Data = % x, want % x", aus[0].Data, want)
	}
}

func TestInBandNoConfigFirst(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = d.Depacketize(v5, true, 0)
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v, want ErrNoConfig", err)
	}
}

func TestResetDropsInBandConfig(t *testing.T) {
	t.Parallel()
	d, err := New(Config{MuxConfigPresent: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := d.Depacketize(v4, true, 0); err != nil {
		t.Fatalf("Depacketize(v4): %v", err)
	}

	d.Reset()

	_, err = d.Depacketize(v5, true, 0)
	if !errors.Is(err, ErrNoConfig) {
		t.Fatalf("err = %v, want ErrNoConfig", err)
	}
}

func TestASCExtraction(t *testing.T) {
	t.Parallel()

	t.Run("frameLengthFlag 0", func(t *testing.T) {
		t.Parallel()
		smc, asc, frameLength, err := parseStreamMuxConfig(v1)
		if err != nil {
			t.Fatalf("parseStreamMuxConfig: %v", err)
		}
		wantASC := []byte{0x12, 0x10}
		if !bytes.Equal(asc, wantASC) {
			t.Errorf("asc = % x, want % x", asc, wantASC)
		}
		if frameLength != 1024 {
			t.Errorf("frameLength = %d, want 1024", frameLength)
		}
		if smc.numSubFrames != 0 {
			t.Errorf("numSubFrames = %d, want 0", smc.numSubFrames)
		}
	})

	t.Run("frameLengthFlag 1", func(t *testing.T) {
		t.Parallel()
		_, _, frameLength, err := parseStreamMuxConfig(v1FrameLength960)
		if err != nil {
			t.Fatalf("parseStreamMuxConfig: %v", err)
		}
		if frameLength != 960 {
			t.Errorf("frameLength = %d, want 960", frameLength)
		}
	})

	t.Run("explicit sampling rate", func(t *testing.T) {
		t.Parallel()
		_, asc, _, err := parseStreamMuxConfig(v1ExplicitRate)
		if err != nil {
			t.Fatalf("parseStreamMuxConfig: %v", err)
		}
		wantASC := []byte{0x17, 0x80, 0x5D, 0xC0, 0x10}
		if !bytes.Equal(asc, wantASC) {
			t.Errorf("asc = % x, want % x", asc, wantASC)
		}
	})

	t.Run("unsupported audio object type", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := parseStreamMuxConfig(v1UnsupportedAOT)
		if !errors.Is(err, ErrUnsupportedASC) {
			t.Fatalf("err = %v, want ErrUnsupportedASC", err)
		}
	})
}
