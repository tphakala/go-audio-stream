package httpsource

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// incompleteWAVHeader is a valid RIFF/WAVE lead-in with no following chunk, so
// parseWAVHeader completes the 12-byte header read and then blocks reading the
// next chunk header until the stream delivers more or the open phase ends.
var incompleteWAVHeader = []byte("RIFF\x00\x00\x00\x00WAVE")

// incompleteAAC is a 2-byte prefix, too short to confirm an ADTS frame or an
// ID3v2 header, so skipLeadingID3's first Peek is what stalls on it; the outcome
// then depends on the read result (a stall or a cancel), not the prefix content.
var incompleteAAC = []byte{0xFF, 0xF1}

// incompleteAACAfterProbe is 16 non-ID3, non-ADTS bytes: enough for
// skipLeadingID3's first Peek to succeed and return (no tag), so the stall lands
// later in probeADTS's own Peek, exercising the probe's post-scan classification
// rather than the ID3-detect one.
var incompleteAACAfterProbe = make([]byte, 16)

// truncatedID3ThenStall is a valid ID3v2 header declaring a 4096-byte body but
// carrying only 8 body bytes, so skipLeadingID3 detects the tag and then stalls
// inside Discard, exercising the Discard-branch classification.
var truncatedID3ThenStall = aacID3v2Tag(4096, false)[:id3v2HeaderLen+8]

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
// sniff path; naming it avoids adding another loose literal that would trip
// goconst on the occurrences already in the package tests. ctAudioAAC is named
// for the same reason: the three AAC cases below would otherwise repeat it.
const (
	ctOctetStream = "application/octet-stream"
	ctAudioAAC    = "audio/aac"
)

// openClassifyCases covers every Open-time body-read site that classifies a
// stall: WAV (parseWAVHeader), the three AAC sites (skipLeadingID3's detect Peek
// and its Discard, and probeADTS's own Peek), and the unlabeled sniff Peek.
var openClassifyCases = []openClassifyCase{
	{name: "wav", contentType: "audio/wav", prefix: incompleteWAVHeader},
	{name: "aac-id3-detect", contentType: ctAudioAAC, prefix: incompleteAAC},
	{name: "aac-probe", contentType: ctAudioAAC, prefix: incompleteAACAfterProbe},
	{name: "aac-id3-discard", contentType: ctAudioAAC, prefix: truncatedID3ThenStall},
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

// timeoutError is a net.Error whose Timeout reports true, so classifyOpenErr's
// net.Error branch is exercised without a real socket timeout.
type timeoutError struct{}

func (timeoutError) Error() string   { return "simulated i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

// TestClassifyHeaderReadErr unit-tests the open-phase read-error taxonomy
// directly, covering the branches the end-to-end stall/cancel tests do not reach
// on their own: a plain transport failure that is neither EOF, timeout, nor
// cancel classifies as ErrConnectionClosed, and an open deadline or a caller
// cancel takes precedence over an EOF-shaped read.
func TestClassifyHeaderReadErr(t *testing.T) {
	genericErr := errors.New("boom")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	timedOut := func() *atomic.Bool { b := &atomic.Bool{}; b.Store(true); return b }
	fresh := func() *atomic.Bool { return &atomic.Bool{} }

	cases := []struct {
		name     string
		ctx      context.Context
		err      error
		timedOut *atomic.Bool
		want     error // matched with errors.Is; nil means the parser reports its own format error
	}{
		{"clean EOF is a format error", context.Background(), io.EOF, fresh(), nil},
		{"clean unexpected EOF is a format error", context.Background(), io.ErrUnexpectedEOF, fresh(), nil},
		{"open deadline fired", context.Background(), genericErr, timedOut(), ErrRequestTimeout},
		{"open deadline beats an EOF-shaped read", context.Background(), io.ErrUnexpectedEOF, timedOut(), ErrRequestTimeout},
		{"caller cancel", cancelled, genericErr, fresh(), context.Canceled},
		{"caller cancel beats an EOF-shaped read", cancelled, io.EOF, fresh(), context.Canceled},
		{"transport timeout", context.Background(), timeoutError{}, fresh(), ErrRequestTimeout},
		{"other transport failure", context.Background(), genericErr, fresh(), ErrConnectionClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyHeaderReadErr(tc.ctx, tc.err, tc.timedOut)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("classifyHeaderReadErr = %v, want nil (format-error passthrough)", got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("classifyHeaderReadErr = %v, want errors.Is %v", got, tc.want)
			}
		})
	}
}
