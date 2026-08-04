package doctor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wav "github.com/tphakala/go-wav"
	wavpcm "github.com/tphakala/go-wav/pcm"
)

// wavTestBody encodes a 16-bit PCM WAV (the shape httpsource accepts) from the
// given samples, so a test server can serve a bounded, self-describing stream.
func wavTestBody(t *testing.T, sampleRate, channels int, samples []int16) []byte {
	t.Helper()
	var buf bytes.Buffer
	cfg := wavpcm.Config{SampleRate: sampleRate, BitDepth: 16, Channels: channels, Format: wav.SampleFormatPCM}
	if err := wavpcm.EncodeInterleaved(&buf, cfg, int16sToLE(samples)); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	return buf.Bytes()
}

// rampSamples returns n incrementing int16 samples, a recognizable pattern for
// asserting the delivered PCM is byte-exact.
func rampSamples(n int) []int16 {
	s := make([]int16, n)
	for i := range s {
		s[i] = int16(i) //nolint:gosec // test pattern, i is bounded by the caller.
	}
	return s
}

// runHTTP opens a real httpProber against the target and drives a full Run. It
// returns the primary output stream (the report under -report, the walkthrough
// otherwise) and the mapped exit code.
func runHTTP(t *testing.T, opts Options) (output string, code int) {
	t.Helper()
	prober := newHTTPProber(opts)
	t.Cleanup(func() { _ = prober.Close() })
	var out, errOut strings.Builder
	res, err := Run(context.Background(), opts, prober, &out, &errOut, testEnv(), time.Now)
	return out.String(), mapExit(err, res)
}

// TestHTTPWAVFullRun serves a bounded 16-bit PCM WAV and asserts the full run:
// the OPEN step succeeds, the synthesized L16 track is rendered, the stream ends
// with EndStreamEnded because the body is shorter than the window, the run maps
// to ExitOK, and the -wav output parses back to the source rate and channels.
func TestHTTPWAVFullRun(t *testing.T) {
	t.Parallel()
	const sampleRate = 16000
	const channels = 1
	body := wavTestBody(t, sampleRate, channels, rampSamples(2000))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	wavPath := filepath.Join(t.TempDir(), "http_capture.wav")
	opts := Options{URL: srv.URL, Duration: 10 * time.Second, ReadIdle: 5 * time.Second, WAVPath: wavPath}

	got, code := runHTTP(t, opts)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want ExitOK\n%s", code, got)
	}
	for _, want := range []string{"OPEN", "track 0: audio, L16", "ended: stream-end"} {
		if !strings.Contains(got, want) {
			t.Errorf("walkthrough missing %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "clock 16000") {
		t.Errorf("walkthrough should carry the resolved WAV clock rate:\n%s", got)
	}

	wavBytes, rerr := os.ReadFile(wavPath)
	if rerr != nil {
		t.Fatalf("reading WAV output: %v", rerr)
	}
	info, decoded, derr := wavpcm.DecodeInterleaved(wavBytes)
	if derr != nil {
		t.Fatalf("decoding WAV output: %v", derr)
	}
	if info.SampleRate != sampleRate || info.Channels != channels || info.BitDepth != 16 {
		t.Errorf("decoded WAV = %+v, want %d Hz, %d ch, 16-bit", info, sampleRate, channels)
	}
	if len(decoded) == 0 {
		t.Error("decoded WAV PCM is empty")
	}
}

// TestHTTPRawL16FullRun serves raw audio/L16 with the rate and channels in the
// Content-Type parameters and asserts the delivered PCM lands byte-exact: the
// source defaults audio/L16 to little-endian, so a little-endian body is passed
// through verbatim to the output WAV.
func TestHTTPRawL16FullRun(t *testing.T) {
	t.Parallel()
	const sampleRate = 16000
	const channels = 1
	samples := rampSamples(1500)
	body := int16sToLE(samples)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/L16; rate=16000; channels=1")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	wavPath := filepath.Join(t.TempDir(), "http_l16.wav")
	opts := Options{URL: srv.URL, Duration: 10 * time.Second, ReadIdle: 5 * time.Second, WAVPath: wavPath}

	got, code := runHTTP(t, opts)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want ExitOK\n%s", code, got)
	}
	if !strings.Contains(got, "clock 16000") || !strings.Contains(got, "ch 1") {
		t.Errorf("walkthrough should carry the L16 rate and channels:\n%s", got)
	}

	wavBytes, rerr := os.ReadFile(wavPath)
	if rerr != nil {
		t.Fatalf("reading WAV output: %v", rerr)
	}
	info, decoded, derr := wavpcm.DecodeInterleaved(wavBytes)
	if derr != nil {
		t.Fatalf("decoding WAV output: %v", derr)
	}
	if info.SampleRate != sampleRate || info.Channels != channels {
		t.Errorf("decoded WAV = %+v, want %d Hz, %d ch", info, sampleRate, channels)
	}
	if !bytes.Equal(decoded, body) {
		t.Errorf("delivered PCM does not match the source: got %d bytes, want %d, first-diff-safe compare failed", len(decoded), len(body))
	}
}

// TestHTTP401IsAuth asserts a 401 response maps to ExitAuth.
func TestHTTP401IsAuth(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	opts := Options{URL: srv.URL, Duration: 5 * time.Second, Report: true}
	got, code := runHTTP(t, opts)
	if code != ExitAuth {
		t.Fatalf("exit code = %d, want ExitAuth\n%s", code, got)
	}
	if !strings.Contains(got, "result: authentication failed") {
		t.Errorf("report missing the auth result phrase:\n%s", got)
	}
}

// TestHTTPPlaintextCredentialsRefused asserts that credentials on a plaintext
// http URL without -insecure-auth are refused before connecting: ExitUsage with
// the flag hint. With -insecure-auth the same credentials are allowed through.
func TestHTTPPlaintextCredentialsRefused(t *testing.T) {
	t.Parallel()
	var gotAuth atomic.Bool
	body := wavTestBody(t, 16000, 1, rampSamples(1000))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok {
			gotAuth.Store(true)
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	// Refused: credentials over plaintext http, no opt-in.
	refuseOpts := Options{URL: srv.URL, Username: "u", Password: "p", Duration: 5 * time.Second, Report: true}
	got, code := runHTTP(t, refuseOpts)
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want ExitUsage\n%s", code, got)
	}
	if !strings.Contains(got, "-insecure-auth") {
		t.Errorf("report missing the -insecure-auth hint:\n%s", got)
	}
	if gotAuth.Load() {
		t.Error("credentials were transmitted despite the plaintext refusal")
	}

	// Allowed: the same credentials with the opt-in flag connect and capture.
	allowOpts := Options{URL: srv.URL, Username: "u", Password: "p", InsecureAuth: true, Duration: 5 * time.Second, ReadIdle: 5 * time.Second}
	got2, code2 := runHTTP(t, allowOpts)
	if code2 != ExitOK {
		t.Fatalf("exit code with -insecure-auth = %d, want ExitOK\n%s", code2, got2)
	}
	if !gotAuth.Load() {
		t.Error("credentials were not transmitted with -insecure-auth set")
	}
}

// TestHTTPRedirectNotFollowed asserts a 302 is surfaced as a failure and the
// redirect target is never fetched.
func TestHTTPRedirectNotFollowed(t *testing.T) {
	t.Parallel()
	var targetHit atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/moved", func(w http.ResponseWriter, _ *http.Request) {
		targetHit.Store(true)
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(wavTestBody(t, 16000, 1, rampSamples(500)))
	})
	mux.HandleFunc("/stream", func(w http.ResponseWriter, _ *http.Request) {
		// Emit a bare 302 with a Location header rather than http.Redirect, so
		// the test does not depend on the request for relative-URL resolution.
		w.Header().Set("Location", "/moved")
		w.WriteHeader(http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	opts := Options{URL: srv.URL + "/stream", Duration: 5 * time.Second, Report: true}
	got, code := runHTTP(t, opts)
	if code != ExitConnection {
		t.Fatalf("exit code = %d, want ExitConnection\n%s", code, got)
	}
	if !strings.Contains(got, "result: redirected (not followed)") {
		t.Errorf("report missing the redirect result phrase:\n%s", got)
	}
	if targetHit.Load() {
		t.Error("the redirect target was fetched; redirects must not be followed")
	}
}

// TestHTTPStalledBodyWatchdog asserts a source that flushes its headers then
// stops sending body ends with the read-idle watchdog (EndWatchdog).
func TestHTTPStalledBodyWatchdog(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/L16; rate=16000; channels=1")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Hold the connection open with no further body until the client tears
		// it down, so the read-idle watchdog is what ends the capture.
		<-r.Context().Done()
	}))
	defer srv.Close()

	opts := Options{URL: srv.URL, Duration: 10 * time.Second, ReadIdle: 200 * time.Millisecond}
	got, code := runHTTP(t, opts)
	if !strings.Contains(got, "ended: watchdog") {
		t.Errorf("walkthrough should end with the watchdog reason:\n%s", got)
	}
	// No frames arrived, so the run is a capture failure; the point of the test
	// is the end reason, but the exit code should agree.
	if code != ExitCapture {
		t.Errorf("exit code = %d, want ExitCapture (no frames before the watchdog)\n%s", code, got)
	}
}
