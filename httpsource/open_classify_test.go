package httpsource

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

// incompleteWAVHeader is a valid RIFF/WAVE lead-in with no following chunk, so
// parseWAVHeader completes the 12-byte header read and then blocks reading the
// next chunk header until the stream delivers more or the open phase ends.
var incompleteWAVHeader = []byte("RIFF\x00\x00\x00\x00WAVE")

// incompleteAAC is a 2-byte partial ADTS sync, too short to confirm a frame, so
// probeADTS scans an inconclusive prefix and then depends on the read outcome
// (a stall or a cancel) rather than the prefix content.
var incompleteAAC = []byte{0xFF, 0xF1}

// openClassifyCase drives the Open-time format parsers through the same
// stall/cancel scenario, so the WAV, ADTS AAC, and unlabeled-sniff open-phase
// paths are asserted to classify a transient failure identically (the
// consistency #92 is about).
type openClassifyCase struct {
	name        string
	contentType string
	prefix      []byte
	// format is set only for the unlabeled-sniff case, where a valid Config.Format
	// means an unfixed stall would spuriously succeed as raw PCM rather than fail.
	// It makes the test a true red against the pre-#92 behavior.
	format PCMFormat
}

// ctOctetStream is the unlabeled-body Content-Type that routes Open through the
// sniff path; a constant keeps goconst quiet across the test files that use it.
const ctOctetStream = "application/octet-stream"

var openClassifyCases = []openClassifyCase{
	{name: "wav", contentType: "audio/wav", prefix: incompleteWAVHeader},
	{name: "aac", contentType: "audio/aac", prefix: incompleteAAC},
	{name: "sniff", contentType: ctOctetStream, prefix: []byte{0x00, 0x00}, format: PCMFormat{SampleRate: 8000, Channels: 1}},
}

// TestOpenHeaderStallClassifiesAsTimeout covers issue #92: when the server
// answers with a success status and a supported Content-Type but then stalls
// mid-header, the open-phase deadline firing must surface as ErrRequestTimeout,
// not a permanent format error, for both the WAV and the ADTS AAC parsers. A
// consumer that retries on ErrRequestTimeout must not be told the source is
// malformed.
func TestOpenHeaderStallClassifiesAsTimeout(t *testing.T) {
	for _, tc := range openClassifyCases {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			srv := httptest.NewServer(serveThenPark(tc.contentType, tc.prefix, release))
			defer srv.Close()

			_, err := Open(context.Background(), Config{URL: srv.URL, Timeout: 200 * time.Millisecond, Format: tc.format})
			if !errors.Is(err, ErrRequestTimeout) {
				t.Fatalf("Open on a stalled %s header = %v, want ErrRequestTimeout", tc.name, err)
			}
			if errors.Is(err, ErrMalformedWAV) || errors.Is(err, ErrFormatUnknown) {
				t.Fatalf("Open error = %v, still classified as a format error", err)
			}
		})
	}
}

// TestOpenHeaderCallerCancelClassifiesAsCanceled covers issue #92: a caller
// cancellation mid-header returns context.Canceled, not a format error, so a
// consumer that retries on cancellation is not misled into abandoning a source
// that merely had its open interrupted.
func TestOpenHeaderCallerCancelClassifiesAsCanceled(t *testing.T) {
	for _, tc := range openClassifyCases {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			srv := httptest.NewServer(serveThenPark(tc.contentType, tc.prefix, release))
			defer srv.Close()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				time.Sleep(50 * time.Millisecond)
				cancel()
			}()

			_, err := Open(ctx, Config{URL: srv.URL, Timeout: 10 * time.Second, Format: tc.format})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Open on caller-cancelled %s header = %v, want context.Canceled", tc.name, err)
			}
			if errors.Is(err, ErrMalformedWAV) || errors.Is(err, ErrFormatUnknown) {
				t.Fatalf("Open error = %v, still classified as a format error", err)
			}
		})
	}
}
