package doctor

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"

	aacpcm "github.com/tphakala/go-aac/pcm"
	"github.com/tphakala/go-opus/opus"
	wav "github.com/tphakala/go-wav"
	wavpcm "github.com/tphakala/go-wav/pcm"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// unsupportedListenReason is the ListenResult.SkipReason for a track whose
// codec the listen check does not handle: CodecUnknown, and any non-audio
// track, since only CodecAAC, CodecOpus, CodecG711, and CodecL16 have a
// decode or pass-through path to PCM.
const unsupportedListenReason = "codec not supported for the listen check"

// opusMaxFrameSamples bounds a decode buffer for one Opus frame: 120 ms at
// 48 kHz, the longest frame duration RFC 6716 defines.
const opusMaxFrameSamples = 5760

// writeWAV decodes or passes through the captured frames for track and
// writes a WAV to w. It dispatches on the track codec: AAC via go-aac,
// Opus via go-opus, G.711 and L16 pass-through, all written with go-wav.
// It returns a ListenResult describing what was written, or Skipped with a
// reason when the track's codec or stream configuration cannot be turned
// into PCM at all (an unsupported codec, or input a decoder refuses to
// construct against, such as a quirky AudioSpecificConfig); a quirk of the
// captured stream must never crash the tool. A non-nil error means the
// WAV output itself could not be produced, for example a write failure on
// w.
func writeWAV(w io.Writer, track rtsp.Track, frames []CapturedFrame) (ListenResult, error) {
	switch track.Codec.(type) {
	case audiostream.CodecG711, audiostream.CodecL16:
		return writeWAVG711(w, track, frames)
	case audiostream.CodecOpus:
		return writeWAVOpus(w, track, frames)
	case audiostream.CodecAAC:
		return writeWAVAAC(w, track, frames)
	default:
		return ListenResult{Skipped: true, SkipReason: unsupportedListenReason}, nil
	}
}

// writeWAVG711 concatenates the already-linear s16le PCM the library
// delivers for G.711 (Frame.Data is decompanded on arrival) and for L16
// (Frame.Data is byte-swapped to little-endian on arrival), and writes it
// once with go-wav. Both codecs hand the doctor PCM in the same shape, so
// they share this one pass-through path.
func writeWAVG711(w io.Writer, track rtsp.Track, frames []CapturedFrame) (ListenResult, error) {
	channels := max(track.Channels, 1)

	total := 0
	for i := range frames {
		total += len(frames[i].Data)
	}
	pcm := make([]byte, 0, total)
	for i := range frames {
		pcm = append(pcm, frames[i].Data...)
	}

	cfg := wavpcm.Config{SampleRate: track.ClockRate, BitDepth: 16, Channels: channels, Format: wav.SampleFormatPCM}
	if err := wavpcm.EncodeInterleaved(w, cfg, pcm); err != nil {
		return ListenResult{}, err
	}

	return ListenResult{
		Written:    true,
		SampleRate: track.ClockRate,
		Channels:   channels,
		Frames:     len(pcm) / 2 / channels,
	}, nil
}

// writeWAVOpus decodes each RTP Opus packet (RFC 7587: always 48 kHz) and
// streams the PCM to go-wav. A packet the decoder rejects is a stream
// quirk: it is counted and skipped rather than aborting the whole capture.
// A track advertising a channel count the codec cannot decode (outside 1
// or 2) is Skipped at construction, the same treatment as a quirky AAC
// AudioSpecificConfig; a capture in which every packet fails to decode is
// likewise Skipped, never reported as a successfully written (empty) WAV.
func writeWAVOpus(w io.Writer, track rtsp.Track, frames []CapturedFrame) (ListenResult, error) {
	const opusSampleRate = 48000
	ch := max(track.Channels, 1)

	dec, err := opus.NewDecoder(opusSampleRate, ch)
	if err != nil {
		// A channel count the codec cannot decode is a stream quirk (the
		// same treatment as a quirky AAC AudioSpecificConfig), reported as
		// Skipped rather than failing the run.
		return ListenResult{Skipped: true, SkipReason: "opus: " + err.Error()}, nil //nolint:nilerr // intentional: see comment above.
	}

	enc, err := wavpcm.NewEncoder(w, wavpcm.Config{SampleRate: opusSampleRate, BitDepth: 16, Channels: ch, Format: wav.SampleFormatPCM})
	if err != nil {
		return ListenResult{}, err
	}

	buf := make([]int16, opusMaxFrameSamples*ch)
	var totalSamples int
	for i := range frames {
		n, decErr := dec.Decode(frames[i].Data, buf)
		if decErr != nil {
			// A malformed packet is a stream quirk, not a fatal error: skip
			// it and keep decoding the rest of the capture.
			continue
		}
		if _, werr := enc.Write(int16sToLE(buf[:n*ch])); werr != nil {
			return ListenResult{}, werr
		}
		totalSamples += n
	}
	if err := enc.Close(); err != nil {
		return ListenResult{}, err
	}
	if totalSamples == 0 && len(frames) > 0 {
		// Every captured packet failed to decode: the stream is not usable
		// Opus, a quirk worth surfacing rather than a clean, silent WAV.
		return ListenResult{Skipped: true, SkipReason: "opus: no packets decoded"}, nil
	}

	return ListenResult{
		Written:    true,
		SampleRate: opusSampleRate,
		Channels:   ch,
		Frames:     totalSamples,
	}, nil
}

// aacLengthPrefixMax is the largest access unit go-aac's raw-stream framing
// can length-prefix (a 2-byte big-endian field).
const aacLengthPrefixMax = math.MaxUint16

// writeWAVAAC rebuilds a length-prefixed raw AAC stream from the captured
// access units (go-aac/pcm's raw-stream framing contract: a 2-byte
// big-endian length followed by the access unit, repeated) and decodes it
// against the track's AudioSpecificConfig. A decoder that refuses to
// construct against the ASC (ErrUnsupported or ErrCorruptStream) is a
// quirk of the source stream, not a tool failure, and is reported as
// Skipped; any other failure to construct the decoder is a genuine error.
func writeWAVAAC(w io.Writer, track rtsp.Track, frames []CapturedFrame) (ListenResult, error) {
	codec, ok := track.Codec.(audiostream.CodecAAC)
	if !ok {
		return ListenResult{Skipped: true, SkipReason: unsupportedListenReason}, nil
	}

	var raw bytes.Buffer
	for i := range frames {
		data := frames[i].Data
		if len(data) > aacLengthPrefixMax {
			// An access unit this large cannot be framed by the 2-byte
			// length prefix; drop it rather than corrupt the byte stream
			// for every frame after it.
			continue
		}
		var lp [2]byte
		binary.BigEndian.PutUint16(lp[:], uint16(len(data))) //nolint:gosec // len(data) is bounded by the guard above.
		raw.Write(lp[:])
		raw.Write(data)
	}

	dec, err := aacpcm.NewDecoder(bytes.NewReader(raw.Bytes()), aacpcm.WithRawStream(codec.AudioSpecificConfig))
	if err != nil {
		if errors.Is(err, aacpcm.ErrUnsupported) || errors.Is(err, aacpcm.ErrCorruptStream) {
			return ListenResult{Skipped: true, SkipReason: "aac: " + err.Error()}, nil
		}
		return ListenResult{}, err
	}
	info := dec.Info()

	enc, err := wavpcm.NewEncoder(w, wavpcm.Config{SampleRate: info.SampleRate, BitDepth: 16, Channels: info.Channels, Format: wav.SampleFormatPCM})
	if err != nil {
		return ListenResult{}, err
	}
	written, err := dec.WriteTo(enc)
	if err != nil {
		return ListenResult{}, err
	}
	if err := enc.Close(); err != nil {
		return ListenResult{}, err
	}

	bytesPerFrame := int64(2 * info.Channels)
	var frameCount int
	if bytesPerFrame > 0 {
		// written is int64; divide before narrowing so a >2 GiB decode does
		// not wrap on 32-bit builds. The quotient (a frame count) fits int.
		frameCount = int(written / bytesPerFrame)
	}

	return ListenResult{
		Written:    true,
		SampleRate: info.SampleRate,
		Channels:   info.Channels,
		Frames:     frameCount,
	}, nil
}

// int16sToLE encodes interleaved little-endian s16 PCM from s.
func int16sToLE(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(v)) //nolint:gosec // int16 to uint16 is a same-width reinterpretation, not a truncation.
	}
	return b
}
