package rtsp_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

// These tests exercise the DESCRIBE, SETUP, and pipeline error paths and the
// hostile SDP shapes that the happy-path suite leaves uncovered (issue #16).
// None asserts a known bug; they pin the branches where a bug would hide.

// capturingHandler is a thread-safe slog.Handler that records log messages for
// assertions. The reader goroutine and the caller goroutine can both log
// through the client's logger, so the record slice is mutex-guarded.
type capturingHandler struct {
	mu   *sync.Mutex
	msgs *[]string
}

func (h capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

//nolint:gocritic // hugeParam: slog.Handler.Handle takes slog.Record by value; the interface fixes the signature.
func (h capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.msgs = append(*h.msgs, r.Message)
	return nil
}

func (h capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h capturingHandler) WithGroup(string) slog.Handler      { return h }

// newCapturingLogger returns a logger and a func snapshotting the messages
// logged so far. Setting Config.Logger also exercises logWarn's non-nil-logger
// branch, which every test in the package otherwise leaves unevaluated.
func newCapturingLogger() (logger *slog.Logger, snapshot func() []string) {
	mu := &sync.Mutex{}
	msgs := &[]string{}
	logger = slog.New(capturingHandler{mu: mu, msgs: msgs})
	return logger, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(*msgs)
	}
}

// loggedContains reports whether any captured message contains sub.
func loggedContains(msgs []string, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// serveDescribe answers the OPTIONS probe and the DESCRIBE with the given
// headers and body, then drains. It is the scaffolding shared by the
// DESCRIBE error-path tests.
func serveDescribe(t *testing.T, h rtsp.Header, body string) *testserver.Server {
	t.Helper()
	return testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", h, []byte(body))
		drainRequests(sc)
	}})
}

// TestDescribeSessionLevelControl covers the session-level a=control base
// override in resolveTracks: an a=control before any m= line sets the base
// every track control then resolves against, unless it is the "*" sentinel.
func TestDescribeSessionLevelControl(t *testing.T) {
	cases := []struct {
		name       string
		sessionCtl string
		wantSuffix string // appended to scheme://host of the dial URL
	}{
		// An absolute session control overrides the header base: its path (only)
		// becomes the base, so the track control lands under /media/, not the
		// /stream dial path.
		{name: "absolute override", sessionCtl: "rtsp://ignored.example/media/", wantSuffix: "/media/trackID=1"},
		// "*" is the sentinel for "no session base"; the header base (the /stream
		// dial path) governs, exercising the != "*" guard being false.
		{name: "star sentinel", sessionCtl: "*", wantSuffix: "/stream/trackID=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := "v=0\r\n" +
				"o=- 0 0 IN IP4 127.0.0.1\r\n" +
				"s=Stream\r\n" +
				"a=control:" + tc.sessionCtl + "\r\n" +
				"m=audio 0 RTP/AVP 0\r\n" +
				"a=control:trackID=1\r\n"
			s := serveDescribe(t, sdpHeaders(""), body)
			c := dialIdle(t, s.URL("/stream"))
			defer closeAndWait(t, c)

			tracks, err := c.Describe(t.Context())
			if err != nil {
				t.Fatalf("Describe: %v", err)
			}
			if len(tracks) != 1 {
				t.Fatalf("track count = %d, want 1", len(tracks))
			}
			want := strings.TrimSuffix(s.URL(""), "/") + tc.wantSuffix
			if tracks[0].Control != want {
				t.Errorf("Control = %q, want %q", tracks[0].Control, want)
			}
		})
	}
}

// TestDescribeSessionControlResolveError covers the error return of the
// session-level a=control resolution: a control that will not re-parse as a
// URL fails before any track is resolved.
func TestDescribeSessionControlResolveError(t *testing.T) {
	body := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"a=control:rtsp://cam/%zz\r\n" + // invalid percent-escape
		"m=audio 0 RTP/AVP 0\r\n" +
		"a=control:trackID=1\r\n"
	s := serveDescribe(t, sdpHeaders(""), body)
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	if _, err := c.Describe(t.Context()); !errors.Is(err, rtsp.ErrInvalidURL) {
		t.Fatalf("Describe = %v, want ErrInvalidURL", err)
	}
}

// TestDescribeSDPParseError covers the sdp.Parse error return. Parse is lenient
// about garbage (it skips any line without "x="), so the body must fail for a
// structural reason: seventeen m= sections exceed sdp.MaxMediaSections.
func TestDescribeSDPParseError(t *testing.T) {
	body := "v=0\r\n" + strings.Repeat("m=audio 0 RTP/AVP 0\r\n", 17)
	s := serveDescribe(t, sdpHeaders(""), body)
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	if _, err := c.Describe(t.Context()); !errors.Is(err, sdp.ErrTooManyMedia) {
		t.Fatalf("Describe = %v, want sdp.ErrTooManyMedia", err)
	}
}

// TestDescribeBaseURLError covers the ResolveBaseURL error return: a
// Content-Base that will not parse as a URL.
func TestDescribeBaseURLError(t *testing.T) {
	s := serveDescribe(t, sdpHeaders("rtsp://cam/%zz"), aacSDP)
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	if _, err := c.Describe(t.Context()); !errors.Is(err, rtsp.ErrInvalidURL) {
		t.Fatalf("Describe = %v, want ErrInvalidURL", err)
	}
}

// TestDescribeControlURLError covers the per-track ResolveControlURL error
// return: a track a=control that will not re-parse after being appended to the
// base.
func TestDescribeControlURLError(t *testing.T) {
	body := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"m=audio 0 RTP/AVP 0\r\n" +
		"a=control:trackID=%zz\r\n" // invalid percent-escape after the base append
	s := serveDescribe(t, sdpHeaders(""), body)
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	if _, err := c.Describe(t.Context()); !errors.Is(err, rtsp.ErrInvalidURL) {
		t.Fatalf("Describe = %v, want ErrInvalidURL", err)
	}
}

// TestDescribeZeroTracks covers the documented but untested zero-track case: an
// SDP with no m= line yields an empty track slice and a nil error, and still
// advances to the described state.
func TestDescribeZeroTracks(t *testing.T) {
	body := "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=Stream\r\n"
	s := serveDescribe(t, sdpHeaders(""), body)
	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	tracks, err := c.Describe(t.Context())
	if err != nil {
		t.Fatalf("Describe = %v, want nil", err)
	}
	if len(tracks) != 0 {
		t.Fatalf("track count = %d, want 0", len(tracks))
	}
	// It advanced: a second Describe is rejected as a state error.
	if _, err := c.Describe(t.Context()); !errors.Is(err, rtsp.ErrInvalidState) {
		t.Errorf("second Describe = %v, want ErrInvalidState (state advanced)", err)
	}
}

// TestSetupAACWithoutFmtpDegradesToRaw covers configureAAC's degrade branch
// driven by a real SDP: an MPEG4-GENERIC track with no a=fmtp resolves to
// CodecAAC with empty parameters (Mode ""), so no AAC-hbr depacketizer can be
// selected and the track degrades to raw delivery with a logged warning. The
// logger also exercises logWarn's non-nil-logger branch.
func TestSetupAACWithoutFmtpDegradesToRaw(t *testing.T) {
	body := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"m=audio 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 MPEG4-GENERIC/16000/1\r\n" + // no a=fmtp
		"a=control:audio\r\n"
	logger, snapshot := newCapturingLogger()
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(body))
		answerSetup(t, sc, 0, 1, 0, 1)
		drainRequests(sc)
	}})

	c, err := rtsp.Dial(t.Context(), rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout, Logger: logger})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer closeAndWait(t, c)

	tracks, err := c.Describe(t.Context())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if _, ok := tracks[0].Codec.(audiostream.CodecAAC); !ok {
		t.Fatalf("Codec = %T, want CodecAAC (MPEG4-GENERIC resolves to AAC even with no fmtp)", tracks[0].Codec)
	}
	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if msgs := snapshot(); !loggedContains(msgs, "aac track is not AAC-hbr") {
		t.Errorf("expected a degrade-to-raw warning, got messages %v", msgs)
	}
}

// TestSetupZeroClockRate covers a zero clock rate reaching newTrack through a
// real SDP: clockRateTicks maps it to the rate-unknown sentinel, so the track
// sets up without a panic and its ClockRate is 0 (PTS interpolation is skipped).
func TestSetupZeroClockRate(t *testing.T) {
	body := "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"m=audio 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 opus/0/2\r\n" + // zero clock rate
		"a=control:audio\r\n"
	c, tracks := describeOne(t, body, func(sc *testserver.ServerConn) {
		answerSetup(t, sc, 0, 1, 0, 1)
		drainRequests(sc)
	})
	defer closeAndWait(t, c)

	if tracks[0].ClockRate != 0 {
		t.Errorf("ClockRate = %d, want 0", tracks[0].ClockRate)
	}
	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup with zero clock rate: %v", err)
	}
}

// TestSetupDifferingSessionID covers recordSession's differing-id branch: a
// second SETUP that reports a different Session id than the first is a server
// quirk that is logged, not fatal, and the first id keeps governing the
// session. The logger also exercises logWarn's non-nil-logger branch.
func TestSetupDifferingSessionID(t *testing.T) {
	logger, snapshot := newCapturingLogger()
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(audioVideoSDP))
		// First SETUP establishes testSessionID.
		serve(t, sc, methodSetup, 200, "OK", setupHeaders(0, 1, testSessionID, testTimeoutS), nil)
		// Second SETUP reports a DIFFERENT id.
		serve(t, sc, methodSetup, 200, "OK", setupHeaders(2, 3, "sess-other", testTimeoutS), nil)
		drainRequests(sc)
	}})

	c, err := rtsp.Dial(t.Context(), rtsp.Config{URL: s.URL("/stream"), Timeout: testTimeout, Logger: logger})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer closeAndWait(t, c)

	tracks, err := c.Describe(t.Context())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if err := c.Setup(t.Context(), tracks[0], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 0: %v", err)
	}
	if err := c.Setup(t.Context(), tracks[1], rtsp.SetupOptions{}); err != nil {
		t.Fatalf("Setup track 1: %v", err)
	}
	if got := c.SessionInfo().SessionID; got != testSessionID {
		t.Errorf("SessionID = %q, want %q (the first id governs)", got, testSessionID)
	}
	if msgs := snapshot(); !loggedContains(msgs, "different from the established one") {
		t.Errorf("expected a differing-session-id warning, got messages %v", msgs)
	}
}
