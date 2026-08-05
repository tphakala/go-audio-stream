package doctor

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wav "github.com/tphakala/go-wav"
	wavpcm "github.com/tphakala/go-wav/pcm"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// TestBuildBextBodyLength asserts the bext body is exactly 602 bytes, the
// fixed size the EBU Tech 3285 layout this package writes requires and that
// the RIFF splice math (an 8-byte chunk header plus this body, 610 bytes
// total, growing the RIFF ckSize by exactly that much) depends on.
func TestBuildBextBodyLength(t *testing.T) {
	t.Parallel()
	body := buildBextBody(time.Date(2026, 8, 4, 9, 12, 0, 0, time.UTC), 8000)
	if len(body) != bextBodySize {
		t.Fatalf("len(body) = %d, want %d", len(body), bextBodySize)
	}
	if bextBodySize != 602 {
		t.Fatalf("bextBodySize = %d, want 602", bextBodySize)
	}
}

// TestBuildBextChunkLayout asserts the chunk id and the little-endian size
// field, and that the chunk is 610 bytes total (8-byte header plus the
// 602-byte body), so it never needs a RIFF pad byte.
func TestBuildBextChunkLayout(t *testing.T) {
	t.Parallel()
	chunk := buildBextChunk(time.Date(2026, 8, 4, 9, 12, 0, 0, time.UTC), 8000)
	if len(chunk) != 610 {
		t.Fatalf("len(chunk) = %d, want 610", len(chunk))
	}
	if string(chunk[0:4]) != bextChunkID {
		t.Errorf("chunk id = %q, want %q", chunk[0:4], bextChunkID)
	}
	if size := binary.LittleEndian.Uint32(chunk[4:8]); size != bextBodySize {
		t.Errorf("chunk size field = %d, want %d", size, bextBodySize)
	}
}

// TestBuildBextBodyOriginationDateTime pins a fixed UTC instant and checks
// the OriginationDate and OriginationTime fields decode to the expected
// strings, and Version stays 0.
func TestBuildBextBodyOriginationDateTime(t *testing.T) {
	t.Parallel()
	instant := time.Date(2026, 8, 4, 9, 12, 34, 0, time.UTC)
	body := buildBextBody(instant, 8000)

	date := strings.TrimRight(string(body[320:330]), "\x00")
	if date != "2026-08-04" {
		t.Errorf("OriginationDate = %q, want %q", date, "2026-08-04")
	}
	tm := strings.TrimRight(string(body[330:338]), "\x00")
	if tm != "09:12:34" {
		t.Errorf("OriginationTime = %q, want %q", tm, "09:12:34")
	}
	if version := binary.LittleEndian.Uint16(body[346:348]); version != 0 {
		t.Errorf("Version = %d, want 0", version)
	}
}

// TestBuildBextBodyDescriptionAndOriginator checks the Description carries
// the millisecond-precision sender clock start and the Originator names the
// tool, matching the spec's fixed text.
func TestBuildBextBodyDescriptionAndOriginator(t *testing.T) {
	t.Parallel()
	instant := time.Date(2026, 8, 4, 9, 12, 0, 500_000_000, time.UTC)
	body := buildBextBody(instant, 8000)

	desc := strings.TrimRight(string(body[0:256]), "\x00")
	wantDesc := "Captured by stream-doctor. Sender clock start (UTC): 2026-08-04T09:12:00.500Z"
	if desc != wantDesc {
		t.Errorf("Description = %q, want %q", desc, wantDesc)
	}
	orig := strings.TrimRight(string(body[256:288]), "\x00")
	if orig != "stream-doctor" {
		t.Errorf("Originator = %q, want %q", orig, "stream-doctor")
	}
}

// TestBuildBextBodyTimeReference checks the 64-bit TimeReference (low and
// high uint32 halves) decodes to the sample count from midnight UTC of the
// origination date to the instant, at a known sample rate.
func TestBuildBextBodyTimeReference(t *testing.T) {
	t.Parallel()
	// 09:12:00 UTC is 9*3600 + 12*60 = 33120 seconds after midnight.
	instant := time.Date(2026, 8, 4, 9, 12, 0, 0, time.UTC)
	const sampleRate = 8000
	body := buildBextBody(instant, sampleRate)

	low := binary.LittleEndian.Uint32(body[338:342])
	high := binary.LittleEndian.Uint32(body[342:346])
	got := uint64(low) | uint64(high)<<32
	want := uint64(33120 * sampleRate)
	if got != want {
		t.Errorf("TimeReference = %d, want %d", got, want)
	}
}

// TestBuildBextBodyTimeReferenceFractionalSecond checks a sub-second offset
// rounds to the nearest sample rather than truncating.
func TestBuildBextBodyTimeReferenceFractionalSecond(t *testing.T) {
	t.Parallel()
	instant := time.Date(2026, 8, 4, 0, 0, 0, 500_000_000, time.UTC)
	const sampleRate = 8000
	body := buildBextBody(instant, sampleRate)

	low := binary.LittleEndian.Uint32(body[338:342])
	high := binary.LittleEndian.Uint32(body[342:346])
	got := uint64(low) | uint64(high)<<32
	if got != 4000 {
		t.Errorf("TimeReference = %d, want 4000 (500ms at 8kHz)", got)
	}
}

// TestSpliceBext splices a bext chunk into a minimal valid RIFF/WAVE stream
// (fmt + data written by go-wav/pcm) and asserts: the RIFF ckSize grows by
// exactly 610 (8-byte chunk header plus the 602-byte body), the bext chunk
// sits immediately after the 12-byte RIFF header, the original fmt+data
// bytes follow it unchanged, and the spliced file still decodes to the same
// PCM payload byte for byte.
func TestSpliceBext(t *testing.T) {
	t.Parallel()
	const sampleRate = 8000
	const channels = 1
	pcm := int16sToLE([]int16{100, -200, 300, -400, 500})

	var buf bytes.Buffer
	cfg := wavpcm.Config{SampleRate: sampleRate, BitDepth: 16, Channels: channels, Format: wav.SampleFormatPCM}
	if err := wavpcm.EncodeInterleaved(&buf, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	original := buf.Bytes()
	oldSize := binary.LittleEndian.Uint32(original[4:8])

	instant := time.Date(2026, 8, 4, 9, 12, 0, 0, time.UTC)
	chunk := buildBextChunk(instant, sampleRate)

	spliced, err := spliceBext(original, chunk)
	if err != nil {
		t.Fatalf("spliceBext: %v", err)
	}

	if len(spliced) != len(original)+len(chunk) {
		t.Errorf("len(spliced) = %d, want %d", len(spliced), len(original)+len(chunk))
	}
	newSize := binary.LittleEndian.Uint32(spliced[4:8])
	if newSize != oldSize+uint32(len(chunk)) {
		t.Errorf("new RIFF ckSize = %d, want %d (old %d + %d)", newSize, oldSize+uint32(len(chunk)), oldSize, len(chunk))
	}
	if string(spliced[0:4]) != riffChunkID || string(spliced[8:12]) != waveFormType {
		t.Fatalf("spliced header = %q/%q, want RIFF/WAVE", spliced[0:4], spliced[8:12])
	}
	if string(spliced[12:16]) != bextChunkID {
		t.Errorf("chunk id at offset 12 = %q, want %q", spliced[12:16], bextChunkID)
	}
	if !bytes.Contains(spliced[12:12+len(chunk)], []byte("2026-08-04")) {
		t.Error("spliced bext chunk is missing the OriginationDate text")
	}
	if !bytes.Equal(spliced[12+len(chunk):], original[12:]) {
		t.Error("bytes after the spliced chunk do not match the original fmt+data unchanged")
	}

	info, decoded, derr := wavpcm.DecodeInterleaved(spliced)
	if derr != nil {
		t.Fatalf("DecodeInterleaved(spliced): %v", derr)
	}
	if info.SampleRate != sampleRate || info.Channels != channels || info.BitDepth != 16 {
		t.Errorf("decoded info = %+v, want %d Hz, %d ch, 16-bit", info, sampleRate, channels)
	}
	if !bytes.Equal(decoded, pcm) {
		t.Error("decoded PCM after the splice does not match the original payload byte for byte")
	}
}

// TestSpliceBextRejectsNonRIFF drives the splice's input validation: a
// stream that is not a well-formed RIFF/WAVE header must return an error, so
// the runner's graceful fallback (keep the plain WAV, skip the bext chunk)
// has something to trigger on.
func TestSpliceBextRejectsNonRIFF(t *testing.T) {
	t.Parallel()
	instant := time.Date(2026, 8, 4, 9, 12, 0, 0, time.UTC)
	chunk := buildBextChunk(instant, 8000)

	cases := map[string][]byte{
		"plain text":        []byte("this is not a RIFF stream at all, just text padding"),
		"too short":         []byte("RIFF"),
		"wrong magic":       append([]byte("FORM"), []byte{0, 0, 0, 0, 'W', 'A', 'V', 'E'}...),
		"wrong form type":   append([]byte("RIFF"), []byte{0, 0, 0, 0, 'A', 'V', 'I', ' '}...),
		"RF64 out of scope": append([]byte("RF64"), []byte{0xff, 0xff, 0xff, 0xff, 'W', 'A', 'V', 'E'}...),
		"empty":             {},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := spliceBext(in, chunk); err == nil {
				t.Errorf("spliceBext(%s) returned nil error, want an error", name)
			}
		})
	}
}

// TestRunListenSpliceBextWithValidSenderClock drives Run end to end with a
// valid RTCP sender clock and asserts the written --wav file gained a bext
// chunk anchoring it to the sender clock start, and that the walkthrough
// still carries the sender-clock-start line.
func TestRunListenSpliceBextWithValidSenderClock(t *testing.T) {
	t.Parallel()
	anchor := time.Date(2026, 8, 4, 9, 12, 0, 0, time.UTC)
	g711 := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecG711{Law: audiostream.MuLaw}, ClockRate: 8000, Channels: 1}
	f := &fakeProber{
		tracks:  []rtsp.Track{g711},
		session: happySession(),
		result: CaptureResult{
			// frames[0].RTPTime 0 maps to the sender-report anchor, so the
			// derived start is exactly anchor.
			Frames: []CapturedFrame{{Data: make([]byte, 320), RTPTime: 0}},
			Stats:  audiostream.TrackStats{SenderClock: audiostream.SenderClock{RTPTime: 0, NTPTime: anchor, ClockRate: 8000, Valid: true}},
			Reason: EndCompleted,
		},
	}
	wavPath := filepath.Join(t.TempDir(), testWAVName)
	opts := Options{URL: testTargetURL, Duration: time.Second, WAVPath: wavPath}

	var out strings.Builder
	_, _ = Run(context.Background(), opts, f, &out, os.Stderr, testEnv(), fixedClock(time.Millisecond))

	wavBytes, rerr := os.ReadFile(wavPath)
	if rerr != nil {
		t.Fatalf("reading --wav output: %v", rerr)
	}
	if len(wavBytes) < 16 || string(wavBytes[0:4]) != "RIFF" || string(wavBytes[8:12]) != "WAVE" {
		t.Fatalf("output is not a RIFF/WAVE stream")
	}
	if string(wavBytes[12:16]) != "bext" {
		t.Fatalf("output is missing a bext chunk right after the RIFF header, got id %q", wavBytes[12:16])
	}
	if !bytes.Contains(wavBytes[12:12+610], []byte("2026-08-04")) {
		t.Error("bext chunk does not carry the expected OriginationDate")
	}

	// The bext chunk must not break decoding: the rest of the file is still
	// a valid fmt+data stream.
	if _, _, derr := wavpcm.DecodeInterleaved(wavBytes); derr != nil {
		t.Errorf("DecodeInterleaved on the spliced output: %v", derr)
	}

	if !strings.Contains(out.String(), "sender clock start 2026-08-04T09:12:00.000Z") {
		t.Errorf("walkthrough missing the sender-clock start line:\n%s", out.String())
	}
}

// TestRunListenNoBextWithoutSenderClock drives Run end to end with no RTCP
// sender clock (the zero value: Valid false) and asserts the written --wav
// file has no bext chunk and is a plain RIFF/WAVE stream, matching today's
// behavior for HTTP sources and cameras that never send Sender Reports.
func TestRunListenNoBextWithoutSenderClock(t *testing.T) {
	t.Parallel()
	g711 := rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecG711{Law: audiostream.MuLaw}, ClockRate: 8000, Channels: 1}
	f := &fakeProber{
		tracks:  []rtsp.Track{g711},
		session: happySession(),
		result: CaptureResult{
			Frames: []CapturedFrame{{Data: make([]byte, 320)}},
			Reason: EndCompleted,
		},
	}
	wavPath := filepath.Join(t.TempDir(), testWAVName)
	opts := Options{URL: testTargetURL, Duration: time.Second, WAVPath: wavPath}

	var out strings.Builder
	_, _ = Run(context.Background(), opts, f, &out, os.Stderr, testEnv(), fixedClock(time.Millisecond))

	wavBytes, rerr := os.ReadFile(wavPath)
	if rerr != nil {
		t.Fatalf("reading --wav output: %v", rerr)
	}
	if string(wavBytes[0:4]) != "RIFF" || string(wavBytes[8:12]) != "WAVE" {
		t.Fatalf("output is not a RIFF/WAVE stream")
	}
	if bytes.Contains(wavBytes, []byte("bext")) {
		t.Error("output contains a bext chunk, want a plain WAV with no sender clock available")
	}
	if strings.Contains(out.String(), "sender clock start") {
		t.Errorf("walkthrough carries a sender-clock start line without a valid sender clock:\n%s", out.String())
	}
}
