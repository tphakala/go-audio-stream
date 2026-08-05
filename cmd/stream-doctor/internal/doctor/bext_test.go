package doctor

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wavpcm "github.com/tphakala/go-wav/pcm"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// Byte offsets of the fixed fields inside a bext chunk body, in wire order
// (EBU Tech 3285). The tests below index go-wav's serialized bext body
// directly to confirm the doctor's descriptor reached the wire intact; these
// mirror the standard layout go-wav writes, not any constant this package
// still owns.
const (
	testBextOffDescription     = 0   // [256]byte, ASCII, NUL-padded.
	testBextOffOriginator      = 256 // [32]byte, ASCII, NUL-padded.
	testBextOffOriginationDate = 320 // [10]byte, "YYYY-MM-DD".
	testBextOffOriginationTime = 330 // [8]byte, "HH:MM:SS".
	testBextOffTimeReferenceLo = 338 // uint32 LE.
	testBextOffTimeReferenceHi = 342 // uint32 LE.
	testBextOffVersion         = 346 // uint16 LE.
)

// findRIFFChunk walks a RIFF/WAVE byte stream and returns the body of the
// first chunk whose id is want, or nil and false when no such chunk exists.
// It walks the actual chunk table (id, uint32 LE size, body, then a pad byte
// after an odd-sized body) rather than scanning for the literal bytes, so a
// coincidental "bext" inside the audio data cannot be mistaken for the chunk.
func findRIFFChunk(t *testing.T, wavBytes []byte, want string) ([]byte, bool) {
	t.Helper()
	if len(wavBytes) < 12 || string(wavBytes[0:4]) != "RIFF" || string(wavBytes[8:12]) != "WAVE" {
		t.Fatalf("not a RIFF/WAVE stream: % x", wavBytes[:min(12, len(wavBytes))])
	}
	for off := 12; off+8 <= len(wavBytes); {
		id := string(wavBytes[off : off+4])
		size := int(binary.LittleEndian.Uint32(wavBytes[off+4 : off+8]))
		body := off + 8
		if body+size > len(wavBytes) {
			break
		}
		if id == want {
			return wavBytes[body : body+size], true
		}
		off = body + size
		if size%2 == 1 {
			off++ // RIFF pads an odd-sized body to an even boundary.
		}
	}
	return nil, false
}

// bextTimeReference reassembles the 64-bit TimeReference from its two
// little-endian uint32 halves in a bext body.
func bextTimeReference(body []byte) uint64 {
	low := binary.LittleEndian.Uint32(body[testBextOffTimeReferenceLo:])
	high := binary.LittleEndian.Uint32(body[testBextOffTimeReferenceHi:])
	return uint64(low) | uint64(high)<<32
}

// TestBuildBextReturnsNilWithoutSenderStart asserts a zero senderStart yields
// no descriptor, so the encoder writes a plain WAV with no bext chunk. This is
// the HTTP-source and no-Sender-Report path.
func TestBuildBextReturnsNilWithoutSenderStart(t *testing.T) {
	t.Parallel()
	if b := buildBext(time.Time{}, 8000); b != nil {
		t.Errorf("buildBext(zero, 8000) = %+v, want nil", b)
	}
}

// TestBuildBextFields pins the descriptor buildBext returns for a fixed UTC
// instant: Description carries the millisecond-precision sender clock start,
// Originator names the tool, OriginationDate/OriginationTime match the
// instant, TimeReference is the sample count since UTC midnight, and Version
// stays 0 (no UMID or loudness fields).
func TestBuildBextFields(t *testing.T) {
	t.Parallel()
	// 09:12:00 UTC is 9*3600 + 12*60 = 33120 seconds after midnight.
	instant := time.Date(2026, 8, 4, 9, 12, 0, 500_000_000, time.UTC)
	const sampleRate = 8000

	b := buildBext(instant, sampleRate)
	if b == nil {
		t.Fatal("buildBext returned nil for a valid senderStart")
	}

	wantDesc := "Captured by stream-doctor. Sender clock start (UTC): 2026-08-04T09:12:00.500Z"
	if b.Description != wantDesc {
		t.Errorf("Description = %q, want %q", b.Description, wantDesc)
	}
	if b.Originator != "stream-doctor" {
		t.Errorf("Originator = %q, want %q", b.Originator, "stream-doctor")
	}
	if b.OriginationDate != "2026-08-04" {
		t.Errorf("OriginationDate = %q, want %q", b.OriginationDate, "2026-08-04")
	}
	if b.OriginationTime != "09:12:00" {
		t.Errorf("OriginationTime = %q, want %q", b.OriginationTime, "09:12:00")
	}
	// 33120.5s * 8000Hz = 264960400 samples (the .5s adds exactly 4000).
	if want := uint64(33120*sampleRate) + 4000; b.TimeReference != want {
		t.Errorf("TimeReference = %d, want %d", b.TimeReference, want)
	}
	if b.Version != 0 {
		t.Errorf("Version = %d, want 0", b.Version)
	}
}

// TestBuildBextConvertsToUTC passes a senderStart in a non-UTC zone that rolls
// to a different UTC calendar date and asserts OriginationDate is the UTC
// date, OriginationTime is the UTC time, and TimeReference counts from UTC
// midnight of that date, not the input zone's midnight.
func TestBuildBextConvertsToUTC(t *testing.T) {
	t.Parallel()
	zone := time.FixedZone("test-5", -5*3600)
	// 21:00:00 at UTC-5 is 02:00:00 UTC on the next calendar day.
	instant := time.Date(2026, 8, 4, 21, 0, 0, 0, zone)
	const sampleRate = 8000

	b := buildBext(instant, sampleRate)
	if b == nil {
		t.Fatal("buildBext returned nil for a valid senderStart")
	}
	if b.OriginationDate != "2026-08-05" {
		t.Errorf("OriginationDate = %q, want %q (the UTC date, not the input zone's)", b.OriginationDate, "2026-08-05")
	}
	if b.OriginationTime != "02:00:00" {
		t.Errorf("OriginationTime = %q, want %q", b.OriginationTime, "02:00:00")
	}
	if want := uint64(2 * 3600 * sampleRate); b.TimeReference != want {
		t.Errorf("TimeReference = %d, want %d (samples since UTC midnight)", b.TimeReference, want)
	}
}

// TestTimeReferenceSamplesRounds checks a sub-second offset rounds to the
// nearest sample rather than truncating. The nanosecond offset is chosen so
// the exact sample count (987.654312 at 8kHz) has a genuine fraction: 500ms
// would give exactly 4000.0 samples, where rounding and truncating agree.
func TestTimeReferenceSamplesRounds(t *testing.T) {
	t.Parallel()
	u := time.Date(2026, 8, 4, 0, 0, 0, 123_456_789, time.UTC)
	// 0.123456789s * 8000Hz = 987.654312 samples, which rounds to 988 and
	// would truncate to 987; asserting 988 confirms rounding is in effect.
	if got := timeReferenceSamples(u, 8000); got != 988 {
		t.Errorf("timeReferenceSamples = %d, want 988 (0.123456789s at 8kHz, rounded)", got)
	}
}

// TestTimeReferenceSamplesMidnightExact checks an instant at exactly
// 00:00:00.000 UTC yields TimeReference 0.
func TestTimeReferenceSamplesMidnightExact(t *testing.T) {
	t.Parallel()
	u := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	if got := timeReferenceSamples(u, 8000); got != 0 {
		t.Errorf("timeReferenceSamples = %d, want 0 at exact midnight", got)
	}
}

// TestTimeReferenceSamplesBoundsSampleRate asserts an implausibly large sample
// rate (as could come from an unbounded, camera-supplied SDP clock rate)
// yields 0 rather than an overflowed value, while a normal rate is unaffected.
func TestTimeReferenceSamplesBoundsSampleRate(t *testing.T) {
	t.Parallel()
	u := time.Date(2026, 8, 4, 9, 12, 0, 0, time.UTC)

	if got := timeReferenceSamples(u, maxBextSampleRate+1); got != 0 {
		t.Errorf("timeReferenceSamples with an out-of-range sample rate = %d, want 0", got)
	}
	if got, want := timeReferenceSamples(u, 8000), uint64(33120*8000); got != want {
		t.Errorf("timeReferenceSamples with a normal sample rate = %d, want %d", got, want)
	}
}

// TestRunListenBextWithValidSenderClock drives Run end to end with a valid
// RTCP sender clock and asserts the written --wav file carries a native bext
// chunk anchoring it to the sender clock start (Description, OriginationDate,
// OriginationTime, TimeReference, Version 0), that the bext chunk does not
// break decoding, and that the walkthrough still carries the sender-clock
// start line.
func TestRunListenBextWithValidSenderClock(t *testing.T) {
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

	body, ok := findRIFFChunk(t, wavBytes, "bext")
	if !ok {
		t.Fatal("output is missing a bext chunk")
	}
	if len(body) < 348 {
		t.Fatalf("bext body is %d bytes, too short to carry the fixed fields", len(body))
	}

	desc := strings.TrimRight(string(body[testBextOffDescription:testBextOffOriginator]), "\x00")
	wantDesc := "Captured by stream-doctor. Sender clock start (UTC): 2026-08-04T09:12:00.000Z"
	if desc != wantDesc {
		t.Errorf("bext Description = %q, want %q", desc, wantDesc)
	}
	date := strings.TrimRight(string(body[testBextOffOriginationDate:testBextOffOriginationTime]), "\x00")
	if date != "2026-08-04" {
		t.Errorf("bext OriginationDate = %q, want %q", date, "2026-08-04")
	}
	tm := strings.TrimRight(string(body[testBextOffOriginationTime:testBextOffTimeReferenceLo]), "\x00")
	if tm != "09:12:00" {
		t.Errorf("bext OriginationTime = %q, want %q", tm, "09:12:00")
	}
	if got, want := bextTimeReference(body), uint64(33120*8000); got != want {
		t.Errorf("bext TimeReference = %d, want %d", got, want)
	}
	if v := binary.LittleEndian.Uint16(body[testBextOffVersion:]); v != 0 {
		t.Errorf("bext Version = %d, want 0", v)
	}

	// The bext chunk must not break decoding: the rest of the file is still a
	// valid fmt+data stream.
	if _, _, derr := wavpcm.DecodeInterleaved(wavBytes); derr != nil {
		t.Errorf("DecodeInterleaved on the bext-carrying output: %v", derr)
	}

	if !strings.Contains(out.String(), "sender clock start 2026-08-04T09:12:00.000Z") {
		t.Errorf("walkthrough missing the sender-clock start line:\n%s", out.String())
	}
}

// TestRunListenNoBextWithoutSenderClock drives Run end to end with no RTCP
// sender clock (the zero value: Valid false) and asserts the written --wav
// file has no bext chunk and is a plain RIFF/WAVE stream, matching the
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
	if _, ok := findRIFFChunk(t, wavBytes, "bext"); ok {
		t.Error("output contains a bext chunk, want a plain WAV with no sender clock available")
	}
	if strings.Contains(out.String(), "sender clock start") {
		t.Errorf("walkthrough carries a sender-clock start line without a valid sender clock:\n%s", out.String())
	}
}
