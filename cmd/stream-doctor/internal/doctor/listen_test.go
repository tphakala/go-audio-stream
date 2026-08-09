package doctor

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	aac "github.com/tphakala/go-aac"
	"github.com/tphakala/go-opus/opus"
	wavpcm "github.com/tphakala/go-wav/pcm"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// fillSine fills buf (interleaved int16 PCM) with a low-amplitude sine
// wave, continuing the phase from startSample so consecutive calls produce
// a continuous tone across packet or frame boundaries.
func fillSine(buf []int16, sampleRate, channels, startSample int) {
	const freq = 440.0
	const amp = 8000
	n := len(buf) / channels
	for i := range n {
		s := int16(amp * math.Sin(2*math.Pi*freq*float64(startSample+i)/float64(sampleRate)))
		for c := range channels {
			buf[i*channels+c] = s
		}
	}
}

// frameWriter records each Write call as one element, so the AAC test can
// recover the individual ADTS frames go-aac/pcm.Encoder writes. go-aac
// v0.3.0 (the version this module depends on, resolved from the module
// proxy) writes ADTS-framed output only; the raw-access-unit FrameEncoder
// landed after that release. Each go-aac/pcm.Encoder write of one complete
// access unit is a single io.Writer.Write call, so stripping the fixed
// 7-byte ADTS header from each recorded call recovers the same raw access
// units RTP AAC depacketization would have delivered.
type frameWriter struct {
	frames [][]byte
}

func (fw *frameWriter) Write(p []byte) (int, error) {
	fw.frames = append(fw.frames, append([]byte(nil), p...))
	return len(p), nil
}

func TestWriteWAVG711MuLaw(t *testing.T) {
	t.Parallel()
	const sampleRate = 8000
	const channels = 1

	// A known s16le ramp, as the library would deliver for PCMU: Frame.Data
	// is already companded to linear PCM by the time it reaches the doctor.
	ramp := make([]int16, 300)
	for i := range ramp {
		ramp[i] = int16(i*100 - 15000)
	}
	pcmBytes := int16sToLE(ramp)

	frames := []CapturedFrame{
		{Data: append([]byte(nil), pcmBytes[:200]...)},
		{Data: append([]byte(nil), pcmBytes[200:400]...)},
		{Data: append([]byte(nil), pcmBytes[400:]...)},
	}
	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecG711{Law: audiostream.MuLaw},
		ClockRate: sampleRate, Channels: channels,
	}

	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV: %v", err)
	}
	if !res.Written || res.Skipped {
		t.Fatalf("res = %+v, want Written", res)
	}
	if res.SampleRate != sampleRate || res.Channels != channels {
		t.Errorf("res sample rate/channels = %d/%d, want %d/%d", res.SampleRate, res.Channels, sampleRate, channels)
	}
	if res.Frames != len(ramp) {
		t.Errorf("res.Frames = %d, want %d", res.Frames, len(ramp))
	}

	info, decoded, err := wavpcm.DecodeInterleaved(buf.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.SampleRate != sampleRate || info.Channels != channels || info.BitDepth != 16 {
		t.Errorf("decoded StreamInfo = %+v, want %d Hz, %d ch, 16-bit", info, sampleRate, channels)
	}
	if !bytes.Equal(decoded, pcmBytes) {
		t.Error("decoded PCM does not round-trip byte for byte")
	}
}

func TestWriteWAVL16(t *testing.T) {
	t.Parallel()
	const sampleRate = 8000
	const channels = 1

	// A known s16le ramp, as the library delivers for L16: the RTP payload
	// arrives big-endian and the library byte-swaps it to little-endian
	// before Frame.Data reaches the doctor, so this is already the shape
	// writeWAVPCM's pass-through expects.
	ramp := make([]int16, 300)
	for i := range ramp {
		ramp[i] = int16(i*100 - 15000)
	}
	pcmBytes := int16sToLE(ramp)

	frames := []CapturedFrame{
		{Data: append([]byte(nil), pcmBytes[:200]...)},
		{Data: append([]byte(nil), pcmBytes[200:400]...)},
		{Data: append([]byte(nil), pcmBytes[400:]...)},
	}
	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecL16{ClockRate: sampleRate, Channels: channels},
		ClockRate: sampleRate, Channels: channels,
	}

	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV: %v", err)
	}
	if !res.Written || res.Skipped {
		t.Fatalf("res = %+v, want Written", res)
	}
	if res.SampleRate != sampleRate || res.Channels != channels {
		t.Errorf("res sample rate/channels = %d/%d, want %d/%d", res.SampleRate, res.Channels, sampleRate, channels)
	}
	if res.Frames != len(ramp) {
		t.Errorf("res.Frames = %d, want %d", res.Frames, len(ramp))
	}

	info, decoded, err := wavpcm.DecodeInterleaved(buf.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.SampleRate != sampleRate || info.Channels != channels || info.BitDepth != 16 {
		t.Errorf("decoded StreamInfo = %+v, want %d Hz, %d ch, 16-bit", info, sampleRate, channels)
	}
	if !bytes.Equal(decoded, pcmBytes) {
		t.Error("decoded PCM does not round-trip byte for byte")
	}
}

func TestWriteWAVOpus(t *testing.T) {
	t.Parallel()
	const sampleRate = 48000
	const channels = 2
	const samplesPerFrame = sampleRate / 1000 * 20 // 20 ms, a valid Opus frame duration
	const numPackets = 10

	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: sampleRate, Channels: channels})
	if err != nil {
		t.Fatalf("opus.NewEncoder: %v", err)
	}

	pcmBuf := make([]int16, samplesPerFrame*channels)
	packetBuf := make([]byte, 1276)
	frames := make([]CapturedFrame, 0, numPackets)
	for p := range numPackets {
		fillSine(pcmBuf, sampleRate, channels, p*samplesPerFrame)
		n, encErr := enc.Encode(pcmBuf, packetBuf)
		if encErr != nil {
			t.Fatalf("Encode packet %d: %v", p, encErr)
		}
		frames = append(frames, CapturedFrame{Data: append([]byte(nil), packetBuf[:n]...)})
	}

	track := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecOpus{}, ClockRate: sampleRate, Channels: channels}
	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV: %v", err)
	}
	if !res.Written || res.Skipped {
		t.Fatalf("res = %+v, want Written", res)
	}
	if res.SampleRate != sampleRate || res.Channels != channels {
		t.Errorf("res sample rate/channels = %d/%d, want %d/%d", res.SampleRate, res.Channels, sampleRate, channels)
	}
	wantSamples := numPackets * samplesPerFrame
	if res.Frames == 0 || res.Frames > wantSamples*2 {
		t.Errorf("res.Frames = %d, want a plausible nonzero count near %d", res.Frames, wantSamples)
	}

	info, decoded, err := wavpcm.DecodeInterleaved(buf.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.SampleRate != sampleRate || info.Channels != channels || info.BitDepth != 16 {
		t.Errorf("decoded StreamInfo = %+v, want %d Hz, %d ch, 16-bit", info, sampleRate, channels)
	}
	if len(decoded) == 0 {
		t.Error("decoded PCM is empty")
	}
}

func TestWriteWAVAAC(t *testing.T) {
	t.Parallel()
	// go-aac's low-level encoder codes at 44100 or 48000 Hz only; there is
	// no way to encode at the walkthrough's illustrative 16000 Hz, so this
	// integration test uses the encoder's own supported rate and asserts
	// against that.
	const sampleRate = 48000
	const channels = 1
	const numFrames = 5

	frames, asc := aacListenFrames(t, sampleRate, channels, numFrames)

	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecAAC{AudioSpecificConfig: asc},
		ClockRate: sampleRate, Channels: channels,
	}
	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV: %v", err)
	}
	if !res.Written || res.Skipped {
		t.Fatalf("res = %+v, want Written", res)
	}
	if res.SampleRate != sampleRate || res.Channels != channels {
		t.Errorf("res sample rate/channels = %d/%d, want %d/%d", res.SampleRate, res.Channels, sampleRate, channels)
	}
	wantSamples := numFrames * aac.FrameSize
	if res.Frames == 0 || res.Frames > wantSamples*2 {
		t.Errorf("res.Frames = %d, want a plausible nonzero count near %d", res.Frames, wantSamples)
	}

	info, decoded, err := wavpcm.DecodeInterleaved(buf.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.SampleRate != sampleRate || info.Channels != channels || info.BitDepth != 16 {
		t.Errorf("decoded StreamInfo = %+v, want %d Hz, %d ch, 16-bit", info, sampleRate, channels)
	}
	if len(decoded) == 0 {
		t.Error("decoded PCM is empty")
	}
}

func TestWriteWAVUnsupported(t *testing.T) {
	t.Parallel()
	track := rtsp.Track{ID: 1, Media: audiostream.MediaVideo, Codec: audiostream.CodecUnknown{RTPMap: testH264}, ClockRate: 90000}
	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, []CapturedFrame{{Data: []byte{1, 2, 3}}}, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV: %v", err)
	}
	if !res.Skipped || res.Written {
		t.Fatalf("res = %+v, want Skipped and not Written", res)
	}
	if res.SkipReason == "" {
		t.Error("SkipReason is empty")
	}
	if buf.Len() != 0 {
		t.Errorf("buf.Len() = %d, want 0 for an unsupported codec", buf.Len())
	}
}

func TestWriteWAVOpusAllCorrupt(t *testing.T) {
	t.Parallel()
	// Structurally invalid Opus packets (RFC 6716: a code 3 packet needs a
	// frame count byte after the TOC; a code 1 packet needs an even payload
	// so the two frames split equally), so every decode fails. A capture in
	// which nothing decodes must be reported as Skipped, never as a
	// successfully written, empty WAV.
	frames := []CapturedFrame{
		{Data: []byte{0xff}},
		{Data: []byte{0x01, 0x00}},
	}
	track := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecOpus{}, ClockRate: 48000, Channels: 2}

	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV returned an error instead of a Skipped result: %v", err)
	}
	if !res.Skipped || res.Written {
		t.Fatalf("res = %+v, want Skipped and not Written", res)
	}
	if res.SkipReason == "" {
		t.Error("SkipReason is empty")
	}
}

func TestWriteWAVAACCorruptASC(t *testing.T) {
	t.Parallel()
	// A single byte cannot hold a valid AudioSpecificConfig (object type,
	// sample rate index, and channel config need at least 13 bits), so
	// decoder construction fails; the quirk must be reported as Skipped,
	// never as an error or a panic.
	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecAAC{AudioSpecificConfig: []byte{0x00}},
		ClockRate: 48000, Channels: 1,
	}
	frames := []CapturedFrame{{Data: []byte{0xde, 0xad, 0xbe, 0xef}}}

	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV returned an error instead of a Skipped result: %v", err)
	}
	if !res.Skipped || res.Written {
		t.Fatalf("res = %+v, want Skipped and not Written", res)
	}
	if !strings.HasPrefix(res.SkipReason, "aac:") {
		t.Errorf("SkipReason = %q, want an \"aac:\" prefix", res.SkipReason)
	}
}

func TestWriteWAVLATM(t *testing.T) {
	t.Parallel()
	// MP4A-LATM delivers the same AAC access units as CodecAAC, so the doctor
	// decodes them through the shared AAC path using the LATM track's ASC. Reuse
	// the AAC listen fixtures (go-aac codes at 44100/48000 Hz only) and assert an
	// out-of-band LATM track decodes to the same PCM shape.
	const sampleRate = 48000
	const channels = 1
	const numFrames = 5

	frames, asc := aacListenFrames(t, sampleRate, channels, numFrames)

	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecMP4ALATM{AudioSpecificConfig: asc},
		ClockRate: sampleRate, Channels: channels,
	}
	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV: %v", err)
	}
	if !res.Written || res.Skipped {
		t.Fatalf("res = %+v, want Written", res)
	}
	if res.SampleRate != sampleRate || res.Channels != channels {
		t.Errorf("res sample rate/channels = %d/%d, want %d/%d", res.SampleRate, res.Channels, sampleRate, channels)
	}
	info, decoded, err := wavpcm.DecodeInterleaved(buf.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if info.SampleRate != sampleRate || info.Channels != channels || info.BitDepth != 16 {
		t.Errorf("decoded StreamInfo = %+v, want %d Hz, %d ch, 16-bit", info, sampleRate, channels)
	}
	if len(decoded) == 0 {
		t.Error("decoded PCM is empty")
	}

	// MP4A-LATM delivers AAC access units, so decoding the same frames and ASC
	// through a CodecAAC track must yield byte-identical WAV output. This proves
	// the LATM path shares the AAC decode rather than merely producing some
	// decodable WAV.
	aacTrack := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecAAC{AudioSpecificConfig: asc},
		ClockRate: sampleRate, Channels: channels,
	}
	var aacBuf bytes.Buffer
	if _, aerr := writeWAV(&aacBuf, aacTrack, frames, time.Time{}); aerr != nil {
		t.Fatalf("writeWAV(aac reference): %v", aerr)
	}
	if !bytes.Equal(buf.Bytes(), aacBuf.Bytes()) {
		t.Error("LATM WAV output differs from the AAC decode of the same access units and ASC")
	}
}

func TestWriteWAVLATMNoASC(t *testing.T) {
	t.Parallel()
	// An in-band LATM track (cpresent=1) learns its ASC from a packet after
	// describe, which the doctor does not capture, so the describe-time track
	// has no ASC. The listen check must Skip cleanly, not error or panic, and
	// label the reason "latm:" rather than "aac:".
	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecMP4ALATM{MuxConfigPresent: true},
		ClockRate: 48000, Channels: 1,
	}
	frames := []CapturedFrame{{Data: []byte{0xde, 0xad, 0xbe, 0xef}}}

	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV returned an error instead of a Skipped result: %v", err)
	}
	if !res.Skipped || res.Written {
		t.Fatalf("res = %+v, want Skipped and not Written", res)
	}
	// Assert the guard's exact message, not just a "latm:" prefix: the shared
	// decoder also rejects an empty ASC with a "latm:"-prefixed reason, so a
	// prefix check would still pass if the guard were removed. The full message
	// flips only when the empty-ASC guard produces it.
	const wantReason = "latm: no AudioSpecificConfig (in-band config not yet learned)"
	if res.SkipReason != wantReason {
		t.Errorf("SkipReason = %q, want %q", res.SkipReason, wantReason)
	}
}

func TestWriteWAVLATMOutOfBandNoASC(t *testing.T) {
	t.Parallel()
	// An out-of-band LATM track (cpresent=0) whose StreamMuxConfig failed to
	// parse reaches the doctor with no ASC (rtsp.resolveLATMASC leaves it nil).
	// The skip reason must name the out-of-band cause, not the in-band one.
	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecMP4ALATM{MuxConfigPresent: false},
		ClockRate: 48000, Channels: 1,
	}
	frames := []CapturedFrame{{Data: []byte{0xde, 0xad}}}

	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV: %v", err)
	}
	const wantReason = "latm: no AudioSpecificConfig (out-of-band StreamMuxConfig missing or did not parse)"
	if !res.Skipped || res.Written || res.SkipReason != wantReason {
		t.Fatalf("res = %+v, want Skipped with reason %q", res, wantReason)
	}
}

func TestWriteWAVLATMCorruptASC(t *testing.T) {
	t.Parallel()
	// A non-empty but invalid ASC passes the empty-ASC guard and reaches the
	// shared AAC decoder, which refuses to construct against it. The quirk must
	// Skip (never error or panic), and the reason must be labeled "latm:", which
	// pins the reasonPrefix the LATM wrapper passes into the shared helper (an
	// "aac" typo there would surface an "aac:" reason on a LATM track).
	track := rtsp.Track{
		ID: 0, Media: audiostream.MediaAudio,
		Codec:     audiostream.CodecMP4ALATM{AudioSpecificConfig: []byte{0x00}},
		ClockRate: 48000, Channels: 1,
	}
	frames := []CapturedFrame{{Data: []byte{0xde, 0xad, 0xbe, 0xef}}}

	var buf bytes.Buffer
	res, err := writeWAV(&buf, track, frames, time.Time{})
	if err != nil {
		t.Fatalf("writeWAV returned an error instead of a Skipped result: %v", err)
	}
	if !res.Skipped || res.Written {
		t.Fatalf("res = %+v, want Skipped and not Written", res)
	}
	if !strings.HasPrefix(res.SkipReason, "latm:") {
		t.Errorf("SkipReason = %q, want a \"latm:\" prefix", res.SkipReason)
	}
}
