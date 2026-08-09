package adts

import (
	"bytes"
	"errors"
	"testing"
)

// adtsFields describes the fields of one ADTS header for buildADTS, so the test
// vectors below read as named fields rather than magic bytes.
type adtsFields struct {
	mpeg2    bool
	crc      bool // protection_absent == 0: a 2-byte CRC follows, header is 9 bytes
	profile  int  // 2-bit profile; audioObjectType is profile+1
	srIdx    int  // 4-bit sampling_frequency_index
	chanCfg  int  // 3-bit channel_configuration
	frameLen int  // 13-bit aac_frame_length (whole frame, header included)
	nrdb     int  // 2-bit number_of_raw_data_blocks_in_frame
}

// buildADTS hand-encodes an ADTS header (the test's own encoder, so a vector is
// a set of fields, not a byte blob). It sets buffer_fullness and the flag bits
// to 0, which Parse ignores.
func buildADTS(f adtsFields) []byte {
	hlen := MinHeaderLen
	if f.crc {
		hlen = CRCHeaderLen
	}
	b := make([]byte, hlen)
	b[0] = 0xFF
	b[1] = 0xF0 // syncword low nibble (1111) + layer 00
	if f.mpeg2 {
		b[1] |= 0x08 // ID bit
	}
	if !f.crc {
		b[1] |= 0x01 // protection_absent
	}
	b[2] = byte(f.profile&0x03)<<6 | byte(f.srIdx&0x0F)<<2 | byte((f.chanCfg>>2)&0x01)
	b[3] = byte(f.chanCfg&0x03)<<6 | byte((f.frameLen>>11)&0x03)
	b[4] = byte((f.frameLen >> 3) & 0xFF)
	b[5] = byte((f.frameLen & 0x07) << 5) // low 3 bits of frame length; buffer_fullness left 0
	b[6] = byte(f.nrdb & 0x03)            // buffer_fullness low bits 0 + num_raw_data_blocks
	return b
}

func TestParseValidLC(t *testing.T) {
	t.Parallel()
	// AAC-LC (profile 1 -> AOT 2), 44100 Hz (index 4), stereo (config 2).
	h, err := Parse(buildADTS(adtsFields{profile: 1, srIdx: 4, chanCfg: 2, frameLen: 100}))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	want := Header{
		AudioObjectType: 2, SampleRateIndex: 4, SampleRate: 44100,
		ChannelConfig: 2, Channels: 2, HeaderLen: MinHeaderLen, FrameLen: 100,
		NumRawBlocks: 0, MPEG2: false,
	}
	if h != want {
		t.Errorf("Parse() = %+v, want %+v", h, want)
	}
	// The synthesized ASC for LC/44100/stereo is the well-known 0x1210, the same
	// value the RTSP AAC path and the stream-doctor render test use.
	if asc := h.AudioSpecificConfig(); !bytes.Equal(asc, []byte{0x12, 0x10}) {
		t.Errorf("AudioSpecificConfig() = % x, want 12 10", asc)
	}
}

func TestParseCRCHeaderLength(t *testing.T) {
	t.Parallel()
	// protection_absent == 0: the header is 9 bytes and the AU starts after the
	// 2-byte CRC.
	h, err := Parse(buildADTS(adtsFields{crc: true, profile: 1, srIdx: 3, chanCfg: 1, frameLen: 50}))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if h.HeaderLen != CRCHeaderLen {
		t.Errorf("HeaderLen = %d, want %d (CRC present)", h.HeaderLen, CRCHeaderLen)
	}
}

func TestParseMPEG2IDBit(t *testing.T) {
	t.Parallel()
	// The MPEG-2 ID bit is accepted and does not change the object-type mapping
	// or the ASC; only MPEG2 reports it.
	h, err := Parse(buildADTS(adtsFields{mpeg2: true, profile: 1, srIdx: 4, chanCfg: 2, frameLen: 100}))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if !h.MPEG2 {
		t.Error("MPEG2 = false, want true for the ID bit set")
	}
	if h.AudioObjectType != 2 {
		t.Errorf("AudioObjectType = %d, want 2 (unchanged by the ID bit)", h.AudioObjectType)
	}
}

func TestParseChannelConfig7Is8Channels(t *testing.T) {
	t.Parallel()
	h, err := Parse(buildADTS(adtsFields{profile: 1, srIdx: 4, chanCfg: 7, frameLen: 100}))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if h.Channels != 8 {
		t.Errorf("Channels = %d, want 8 (config 7 is 7.1)", h.Channels)
	}
}

func TestParseNumRawBlocksAccepted(t *testing.T) {
	t.Parallel()
	// A multi-block header parses (its boundary is known); the framer, not the
	// parser, decides such a frame is undeliverable.
	h, err := Parse(buildADTS(adtsFields{profile: 1, srIdx: 4, chanCfg: 2, frameLen: 100, nrdb: 1}))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if h.NumRawBlocks != 1 {
		t.Errorf("NumRawBlocks = %d, want 1", h.NumRawBlocks)
	}
}

func TestParseFrameLenPacking(t *testing.T) {
	t.Parallel()
	// The 13-bit frame length spans three bytes; exercise a value that uses the
	// high, middle, and low fields together.
	const frameLen = 8191 // max 13-bit value
	h, err := Parse(buildADTS(adtsFields{profile: 1, srIdx: 4, chanCfg: 2, frameLen: frameLen}))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	if h.FrameLen != frameLen {
		t.Errorf("FrameLen = %d, want %d", h.FrameLen, frameLen)
	}
}

func TestAudioSpecificConfig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                    string
		profile, srIdx, chanCfg int
		want                    []byte
	}{
		{"LC 44100 stereo", 1, 4, 2, []byte{0x12, 0x10}},
		{"LC 48000 mono", 1, 3, 1, []byte{0x11, 0x88}},
		{"Main 96000 mono", 0, 0, 1, []byte{0x08, 0x08}},
		{"LTP 8000 5.1", 3, 11, 6, []byte{0x25, 0xB0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, err := Parse(buildADTS(adtsFields{profile: tc.profile, srIdx: tc.srIdx, chanCfg: tc.chanCfg, frameLen: 100}))
			if err != nil {
				t.Fatalf("Parse() error = %v, want nil", err)
			}
			if asc := h.AudioSpecificConfig(); !bytes.Equal(asc, tc.want) {
				t.Errorf("AudioSpecificConfig() = % x, want % x", asc, tc.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	t.Parallel()
	valid := buildADTS(adtsFields{profile: 1, srIdx: 4, chanCfg: 2, frameLen: 100})

	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"too short", valid[:MinHeaderLen-1]},
		{"empty", nil},
		{"bad syncword high byte", func() []byte { b := append([]byte(nil), valid...); b[0] = 0xFE; return b }()},
		{"bad syncword low nibble", func() []byte { b := append([]byte(nil), valid...); b[1] &^= 0xF0; return b }()},
		{"layer not zero", func() []byte { b := append([]byte(nil), valid...); b[1] |= 0x02; return b }()},
		{"sampling index 13 reserved", buildADTS(adtsFields{profile: 1, srIdx: 13, chanCfg: 2, frameLen: 100})},
		{"sampling index 14 reserved", buildADTS(adtsFields{profile: 1, srIdx: 14, chanCfg: 2, frameLen: 100})},
		{"sampling index 15 escape", buildADTS(adtsFields{profile: 1, srIdx: 15, chanCfg: 2, frameLen: 100})},
		{"channel config 0 (PCE)", buildADTS(adtsFields{profile: 1, srIdx: 4, chanCfg: 0, frameLen: 100})},
		{"frame length below header", buildADTS(adtsFields{profile: 1, srIdx: 4, chanCfg: 2, frameLen: MinHeaderLen - 1})},
		{"frame length below CRC header", buildADTS(adtsFields{crc: true, profile: 1, srIdx: 4, chanCfg: 2, frameLen: CRCHeaderLen - 1})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(tc.in); !errors.Is(err, ErrInvalidHeader) {
				t.Errorf("Parse() error = %v, want ErrInvalidHeader", err)
			}
		})
	}
}
