package adts

import "testing"

// FuzzParse drives the ADTS header parser with arbitrary bytes. It must never
// panic, and a nil error must always pair with a header whose fields are all in
// range and whose AudioSpecificConfig is the fixed 2 bytes: the field-specific
// acceptance and rejection rules have dedicated tests, so this is a totality
// check over untrusted input, not a behavioral one.
func FuzzParse(f *testing.F) {
	f.Add(buildADTS(adtsFields{profile: 1, srIdx: 4, chanCfg: 2, frameLen: 100}))
	f.Add(buildADTS(adtsFields{crc: true, profile: 0, srIdx: 3, chanCfg: 1, frameLen: CRCHeaderLen}))
	f.Add(buildADTS(adtsFields{profile: 3, srIdx: 12, chanCfg: 7, frameLen: 8191, nrdb: 3}))
	f.Add([]byte{0xFF, 0xF1, 0x50, 0x80, 0x00, 0x1F, 0xFC})
	f.Add([]byte{0x00, 0x01, 0x02})
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := Parse(data)
		if err != nil {
			return // a typed error is fine; the contract is no panic
		}
		if h.SampleRate <= 0 {
			t.Fatalf("SampleRate %d with nil error", h.SampleRate)
		}
		if h.ChannelConfig < 1 || h.ChannelConfig > 7 {
			t.Fatalf("ChannelConfig %d out of range with nil error", h.ChannelConfig)
		}
		if h.AudioObjectType < 1 || h.AudioObjectType > 4 {
			t.Fatalf("AudioObjectType %d out of range with nil error", h.AudioObjectType)
		}
		if h.HeaderLen != MinHeaderLen && h.HeaderLen != CRCHeaderLen {
			t.Fatalf("HeaderLen %d is neither %d nor %d", h.HeaderLen, MinHeaderLen, CRCHeaderLen)
		}
		if h.FrameLen <= h.HeaderLen {
			t.Fatalf("FrameLen %d <= HeaderLen %d with nil error (empty AU not rejected)", h.FrameLen, h.HeaderLen)
		}
		if asc := h.AudioSpecificConfig(); len(asc) != 2 {
			t.Fatalf("AudioSpecificConfig len %d, want 2", len(asc))
		}
	})
}
