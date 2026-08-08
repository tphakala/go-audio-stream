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

// v1ExtensionFlag is v1 with the ASC extensionFlag bit set to 1 instead of 0.
// In v1 that bit is bit 30 of the config (the last bit of the 16-bit ASC): the
// 6-bit position within byte3, whose value 1<<(7-6) == 0x02, so byte3 flips
// from 0x20 to 0x22. Everything else matches v1 (AAC-LC, 44.1 kHz, stereo).
// parseASC must reject it with ErrUnsupportedASC rather than mis-parse the
// unconsumed GASpecificConfig bits it announces.
var v1ExtensionFlag = []byte{0x40, 0x00, 0x24, 0x22, 0x3F, 0xC0}

// v1ChannelConfigZero is v1 with the ASC channelConfiguration field set to 0
// (byte3 0x20 -> 0x00, clearing the one set bit of the 4-bit field). Per ISO
// 14496-3 that signals the channel layout comes from a program_config_element
// this minimal parse does not decode, so parseASC must reject it with
// ErrUnsupportedASC rather than mis-parse the PCE bits as later mux fields.
var v1ChannelConfigZero = []byte{0x40, 0x00, 0x24, 0x00, 0x3F, 0xC0}

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

	t.Run("extensionFlag set", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := parseStreamMuxConfig(v1ExtensionFlag)
		if !errors.Is(err, ErrUnsupportedASC) {
			t.Fatalf("err = %v, want ErrUnsupportedASC", err)
		}
	})

	t.Run("channelConfiguration zero", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := parseStreamMuxConfig(v1ChannelConfigZero)
		if !errors.Is(err, ErrUnsupportedASC) {
			t.Fatalf("err = %v, want ErrUnsupportedASC", err)
		}
	})
}

// TestParseStreamMuxConfigRejects covers every ErrUnsupportedMux trigger in
// parseStreamMuxConfigBits reachable by flipping bits in v1
// (40 00 24 20 3F C0). numProgram != 0 is already covered by latm.New's
// TestNewRejects, so it is not repeated here. Each variant must be rejected
// with ErrUnsupportedMux.
func TestParseStreamMuxConfigRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		buf  []byte
	}{
		// audioMuxVersion is bit 0 (v1 byte0 0x40 = 0b01000000); set it -> 0xC0.
		{"audioMuxVersion", []byte{0xC0, 0x00, 0x24, 0x20, 0x3F, 0xC0}},
		// allStreamsSameTimeFraming is bit 1 (the set bit of 0x40); clear it -> 0x00.
		{"allStreamsSameTimeFraming", []byte{0x00, 0x00, 0x24, 0x20, 0x3F, 0xC0}},
		// numLayer is bits 12-14, in byte1; set bit 14 (0x02) -> numLayer 1,
		// leaving numProgram (bits 8-11) still 0.
		{"numLayer", []byte{0x40, 0x02, 0x24, 0x20, 0x3F, 0xC0}},
		// frameLengthType is bits 31-33 (spanning byte3/byte4), read after the
		// ASC; set bit 33 (byte4 0x3F -> 0x7F) -> frameLengthType 1. The ASC
		// bytes are untouched, so parseASC still succeeds before the reject.
		{"frameLengthType", []byte{0x40, 0x00, 0x24, 0x20, 0x7F, 0xC0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := parseStreamMuxConfig(tc.buf)
			if !errors.Is(err, ErrUnsupportedMux) {
				t.Fatalf("err = %v, want ErrUnsupportedMux", err)
			}
		})
	}
}

// TestParseStreamMuxConfigTruncated covers a StreamMuxConfig that ends
// mid-field inside parseASC: v1 truncated to 2 bytes consumes the 15 mux-header
// bits, leaving one bit before audioObjectType's 5-bit read runs off the end.
func TestParseStreamMuxConfigTruncated(t *testing.T) {
	t.Parallel()
	_, _, _, err := parseStreamMuxConfig(v1[:2])
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v, want ErrTruncated", err)
	}
}

// TestParseStreamMuxConfigCoreCoderDelay parses buildStreamMuxConfigCoreCoderDelay
// (dependsOnCoreCoder 1, coreCoderDelay 0x123) deterministically, asserting the
// success path: the extracted ASC and derived frameLength, not just no-panic
// under the fuzzer. The 14 coreCoderDelay bits land inside the ASC, so this is
// the only test that pins the exact ASC bytes for that branch.
func TestParseStreamMuxConfigCoreCoderDelay(t *testing.T) {
	t.Parallel()
	smc, asc, frameLength, err := parseStreamMuxConfig(buildStreamMuxConfigCoreCoderDelay())
	if err != nil {
		t.Fatalf("parseStreamMuxConfig: %v", err)
	}
	wantASC := []byte{0x12, 0x12, 0x09, 0x18}
	if !bytes.Equal(asc, wantASC) {
		t.Errorf("asc = % x, want % x", asc, wantASC)
	}
	if frameLength != 1024 {
		t.Errorf("frameLength = %d, want 1024", frameLength)
	}
	if smc.numSubFrames != 0 {
		t.Errorf("numSubFrames = %d, want 0", smc.numSubFrames)
	}
}
