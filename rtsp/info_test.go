package rtsp_test

import (
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// TestInfo checks the source-neutral snapshot Info reports: the dial URL with
// credentials stripped, and the RTSP Server header captured at Dial. The
// serveOptionsThenIdle handler answers the OPTIONS probe with Server:
// TestCam/1.0, so Info().Server has a value to assert and must agree with the
// same field SessionInfo already exposes.
func TestInfo(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: serveOptionsThenIdle})

	// Dial with userinfo in the URL; Info must report the URL without it.
	// parseTarget strips the credentials into the request URL, so the expected
	// value is exactly the credential-free URL the tests already build.
	wantURL := s.URL("/stream")
	withCreds := strings.Replace(wantURL, "rtsp://", "rtsp://user:pass@", 1)

	c, err := rtsp.Dial(t.Context(), rtsp.Config{URL: withCreds, Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer closeAndWait(t, c)

	info := c.Info()
	if info.URL != wantURL {
		t.Errorf("Info().URL = %q, want %q (credentials stripped)", info.URL, wantURL)
	}
	if info.Server != "TestCam/1.0" {
		t.Errorf("Info().Server = %q, want TestCam/1.0", info.Server)
	}
	// Info and SessionInfo read the same captured Server header, so a divergence
	// would mean one of them drifted from the field.
	if got, want := info.Server, c.SessionInfo().Server; got != want {
		t.Errorf("Info().Server = %q, SessionInfo().Server = %q, want equal", got, want)
	}
}

// TestInfoStableAcrossDescribe pins the dialURL-vs-baseURL distinction. Describe
// rewrites the session base from the response's Content-Base (here a foreign
// authority and a path, /onvif/, distinct from the /stream dial path;
// ResolveBaseURL forces the authority back to the dial host but keeps the
// header's path). Info.URL is backed by dialURL, which never moves, so it must
// still report the original /stream dial target rather than the rewritten base.
func TestInfoStableAcrossDescribe(t *testing.T) {
	const foreignBase = "rtsp://cam.example/onvif/"
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeadersFull(foreignBase, ""), []byte(aacSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	if _, err := c.Describe(t.Context()); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	// baseURL is now rtsp://<dialhost>/onvif/, but dialURL is untouched.
	if got, want := c.Info().URL, s.URL("/stream"); got != want {
		t.Errorf("Info().URL after Content-Base rewrite = %q, want %q (dial target, not the rewritten base)", got, want)
	}
}
