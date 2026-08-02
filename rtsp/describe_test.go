package rtsp_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/testserver"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// Method tokens the external tests use for SETUP, PLAY and TEARDOWN. OPTIONS
// and DESCRIBE are already declared by other rtsp_test files.
const (
	methodSetup    = "SETUP"
	methodPlay     = "PLAY"
	methodTeardown = "TEARDOWN"
)

// Shared SDP fixtures for the Describe and Setup tests.
const (
	aacSDP = "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"m=audio 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 MPEG4-GENERIC/16000/1\r\n" +
		"a=fmtp:97 mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1408\r\n" +
		"a=control:audio\r\n"

	audioVideoSDP = "v=0\r\n" +
		"o=- 0 0 IN IP4 127.0.0.1\r\n" +
		"s=Stream\r\n" +
		"m=audio 0 RTP/AVP 97\r\n" +
		"a=rtpmap:97 MPEG4-GENERIC/16000/1\r\n" +
		"a=fmtp:97 mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1408\r\n" +
		"a=control:audio\r\n" +
		"m=video 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=control:video\r\n"

	testSessionID = "sess-42"
	// Deliberately NOT 60: DefaultSessionTimeout is 60s and ParseSession seeds
	// it before parsing, so a 60 here would let every SessionTimeout assertion
	// pass with the timeout parameter ignored entirely.
	testTimeoutS = 90
)

// serve reads the next client request, asserts its method, and answers it with
// the given status, headers, and body. It returns the request so a test can
// inspect the request URL. It reports any read or write failure via t.
func serve(t *testing.T, sc *testserver.ServerConn, wantMethod string, code int, reason string, h rtsp.Header, body []byte) *rtsp.Request {
	t.Helper()
	req, err := sc.ReadRequest()
	if err != nil {
		t.Errorf("read %s: %v", wantMethod, err)
		return nil
	}
	if req.Method != wantMethod {
		t.Errorf("got method %s, want %s", req.Method, wantMethod)
	}
	if err := sc.Respond(req, code, reason, h, body); err != nil {
		t.Errorf("respond %s: %v", wantMethod, err)
	}
	return req
}

// sdpHeaders builds a DESCRIBE response header set with an application/sdp
// Content-Type and an optional Content-Base.
func sdpHeaders(contentBase string) rtsp.Header {
	return sdpHeadersFull(contentBase, "")
}

// sdpHeadersFull additionally sets Content-Location, which ResolveBaseURL falls
// back to when Content-Base is absent.
func sdpHeadersFull(contentBase, contentLocation string) rtsp.Header {
	h := rtsp.Header{}
	h.Set("Content-Type", "application/sdp")
	if contentBase != "" {
		h.Set("Content-Base", contentBase)
	}
	if contentLocation != "" {
		h.Set("Content-Location", contentLocation)
	}
	return h
}

// sessionValue renders a Session header value, appending ";timeout=" when a
// positive timeout is given.
func sessionValue(id string, timeout int) string {
	if timeout > 0 {
		return id + ";timeout=" + strconv.Itoa(timeout)
	}
	return id
}

// setupHeaders builds a SETUP response header set assigning the given
// interleaved channel pair and the session id.
func setupHeaders(rtpCh, rtcpCh int, id string, timeout int) rtsp.Header {
	h := rtsp.Header{}
	h.Set("Transport", rtsp.BuildTransport(rtpCh, rtcpCh))
	h.Set("Session", sessionValue(id, timeout))
	return h
}

// drainRequests answers every remaining client request 200 OK until the client
// disconnects, so a handler goroutine exits cleanly and the client's
// best-effort TEARDOWN on Close is answered promptly instead of waiting out the
// teardown read deadline.
func drainRequests(sc *testserver.ServerConn) {
	for {
		req, err := sc.ReadRequest()
		if err != nil {
			return
		}
		if err := sc.Respond(req, 200, "OK", nil, nil); err != nil {
			return
		}
	}
}

// closeAndWait closes the client and asserts Wait returns ErrClosed (or nil).
//
// Every test in both files defers this helper, so a regression that stops the
// reader from finishing must fail one test rather than hang the package until
// the go test timeout. Passing a deadline to Wait does NOT achieve that: on
// ctx expiry Wait initiates shutdown and then blocks on the reader's done
// channel with no bound of its own, and it would return the ErrClosed that
// Close already recorded, so the assertion could not tell a timeout from
// success. Waiting off the test goroutine is what actually bounds it.
func closeAndWait(t *testing.T, c *rtsp.Client) {
	t.Helper()
	_ = c.Close() // documented to always return nil
	done := make(chan error, 1)
	go func() { done <- c.Wait(context.Background()) }()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, audiostream.ErrClosed) {
			t.Errorf("Wait after Close = %v, want ErrClosed", err)
		}
	case <-time.After(testTimeout):
		t.Errorf("Wait did not return within %v after Close", testTimeout)
	}
}

// keepaliveHeader advertises GET_PARAMETER so a KeepaliveMethod assertion is
// discriminating. KeepaliveMethod falls back to OPTIONS for a nil list, an
// empty list, and a list that merely omits GET_PARAMETER, so asserting OPTIONS
// against publicHeader would hold even if the Public header were never read.
func keepaliveHeader() rtsp.Header {
	h := rtsp.Header{}
	h.Set("Public", "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER")
	return h
}

// dialIdle dials the server and returns a client in the idle state.
func dialIdle(t *testing.T, url string) *rtsp.Client {
	t.Helper()
	c, err := rtsp.Dial(context.Background(), rtsp.Config{URL: url, Timeout: testTimeout})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return c
}

// TestDescribeWrongContentType covers the media-type comparison itself. The
// sibling test that sends no Content-Type at all exits one branch earlier, at
// the empty guard, so without this case the comparison could be deleted with
// the suite still green.
func TestDescribeWrongContentType(t *testing.T) {
	for _, ct := range []string{"text/html", "application/xml; charset=utf-8", "application/sdpx"} {
		t.Run(ct, func(t *testing.T) {
			h := rtsp.Header{}
			h.Set("Content-Type", ct)
			s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
				serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
				serve(t, sc, methodDescribe, 200, "OK", h, []byte(aacSDP))
				drainRequests(sc)
			}})
			c := dialIdle(t, s.URL("/stream"))
			defer closeAndWait(t, c)
			if _, err := c.Describe(context.Background()); !errors.Is(err, rtsp.ErrNotSDP) {
				t.Errorf("Describe with Content-Type %q = %v, want ErrNotSDP", ct, err)
			}
		})
	}
}

func TestDescribeHappyPath(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(aacSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}
	tr := tracks[0]
	if tr.Media != audiostream.MediaAudio {
		t.Errorf("Media = %v, want audio", tr.Media)
	}
	aac, ok := tr.Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("Codec = %T, want CodecAAC", tr.Codec)
	}
	if !bytes.Equal(aac.AudioSpecificConfig, []byte{0x14, 0x08}) {
		t.Errorf("ASC = % x, want 14 08", aac.AudioSpecificConfig)
	}
	if tr.ClockRate != 16000 {
		t.Errorf("ClockRate = %d, want 16000", tr.ClockRate)
	}
	wantControl := s.URL("/stream") + "/audio"
	if tr.Control != wantControl {
		t.Errorf("Control = %q, want %q", tr.Control, wantControl)
	}
	// The raw fmtp propagates from the SDP through to the Track for diagnostics.
	if wantFMTP := "mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1408"; tr.FMTP != wantFMTP {
		t.Errorf("FMTP = %q, want %q", tr.FMTP, wantFMTP)
	}
	// The RTP payload type from the m= line is exposed so a caller can identify
	// the track on the wire.
	if tr.PayloadType != 97 {
		t.Errorf("PayloadType = %d, want 97 (from the m= line)", tr.PayloadType)
	}
}

func TestDescribeMultiTrack(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(audioVideoSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("track count = %d, want 2", len(tracks))
	}
	if tracks[0].Media != audiostream.MediaAudio {
		t.Errorf("track[0].Media = %v, want audio", tracks[0].Media)
	}
	if _, ok := tracks[0].Codec.(audiostream.CodecAAC); !ok {
		t.Errorf("track[0].Codec = %T, want CodecAAC", tracks[0].Codec)
	}
	if tracks[1].Media != audiostream.MediaVideo {
		t.Errorf("track[1].Media = %v, want video", tracks[1].Media)
	}
	if _, ok := tracks[1].Codec.(audiostream.CodecUnknown); !ok {
		t.Errorf("track[1].Codec = %T, want CodecUnknown", tracks[1].Codec)
	}
	// PayloadType is exposed per track from the m= line. It is the one field
	// that still identifies the video track, whose codec resolved to
	// CodecUnknown, which is the case this field exists for.
	if tracks[0].PayloadType != 97 {
		t.Errorf("track[0].PayloadType = %d, want 97", tracks[0].PayloadType)
	}
	if tracks[1].PayloadType != 96 {
		t.Errorf("track[1].PayloadType = %d, want 96", tracks[1].PayloadType)
	}
}

func TestDescribeContentTypeCharset(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		h := rtsp.Header{}
		h.Set("Content-Type", "application/sdp;charset=utf-8")
		serve(t, sc, methodDescribe, 200, "OK", h, []byte(aacSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	tracks, err := c.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe with charset Content-Type: %v", err)
	}
	if len(tracks) != 1 {
		t.Fatalf("track count = %d, want 1", len(tracks))
	}
}

func TestDescribeNotSDP(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		// No Content-Type at all: a hard ErrNotSDP.
		serve(t, sc, methodDescribe, 200, "OK", rtsp.Header{}, []byte(aacSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	_, err := c.Describe(context.Background())
	if !errors.Is(err, rtsp.ErrNotSDP) {
		t.Fatalf("Describe = %v, want ErrNotSDP", err)
	}
}

func TestDescribeRedirect(t *testing.T) {
	const location = "rtsp://other.example/stream"
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		h := rtsp.Header{}
		h.Set("Location", location)
		serve(t, sc, methodDescribe, 302, "Found", h, nil)
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	_, err := c.Describe(context.Background())
	var re *audiostream.RedirectError
	if !errors.As(err, &re) {
		t.Fatalf("Describe = %v, want *audiostream.RedirectError", err)
	}
	if re.Location != location {
		t.Errorf("redirect Location = %q, want %q", re.Location, location)
	}
}

func TestDescribeTwiceRejected(t *testing.T) {
	s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
		serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
		serve(t, sc, methodDescribe, 200, "OK", sdpHeaders(""), []byte(aacSDP))
		drainRequests(sc)
	}})

	c := dialIdle(t, s.URL("/stream"))
	defer closeAndWait(t, c)

	if _, err := c.Describe(context.Background()); err != nil {
		t.Fatalf("first Describe: %v", err)
	}
	_, err := c.Describe(context.Background())
	var se *rtsp.StateError
	if !errors.As(err, &se) {
		t.Fatalf("second Describe = %v, want *StateError", err)
	}
	if !errors.Is(err, rtsp.ErrInvalidState) {
		t.Errorf("second Describe error does not match ErrInvalidState")
	}
}

func TestControlURLVariants(t *testing.T) {
	// The Content-Base and Content-Location rows carry a foreign host
	// (cam.example) the request was never dialed against, AND a path (/onvif/)
	// distinct from the dial path (/stream). ResolveBaseURL keeps only such a
	// header's path and takes the authority from the request URL, so the
	// resolved SETUP URL is rtsp://<dialhost>/onvif/trackID=1: the host proves
	// the authority was forced (see #14), and the /onvif/ path proves the header
	// (not the request URL) sourced the path and, for the last row, that the
	// Content-Location fallback actually fired.
	const foreignBase = "rtsp://cam.example/onvif/"
	cases := []struct {
		name            string
		control         string
		contentBase     string // "" omits Content-Base
		contentLocation string // "" omits Content-Location
		wantSuffix      string // appended to the dial host base (scheme://host:port)
	}{
		{name: "bare token", control: controlTrackID1, wantSuffix: "/stream/trackID=1"},
		{name: "content-base foreign host forced, header path kept", control: controlTrackID1, contentBase: foreignBase, wantSuffix: "/onvif/trackID=1"},
		{name: "absolute wrong host", control: "rtsp://192.168.1.99:554/wrong/track1", wantSuffix: "/wrong/track1"},
		{name: "query only", control: "?ctl=1", wantSuffix: "/stream?ctl=1"},
		{name: "star", control: "*", wantSuffix: "/stream"},
		// Content-Base absent, so ResolveBaseURL falls back to Content-Location.
		// The /onvif/ path (distinct from the /stream dial path) makes this row
		// fail if the fallback were dropped, so it genuinely exercises it.
		{name: "content-location fallback forced, header path kept", control: controlTrackID1, contentLocation: foreignBase, wantSuffix: "/onvif/trackID=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sdpBody := []byte("v=0\r\nm=audio 0 RTP/AVP 0\r\na=control:" + tc.control + "\r\n")
			contentBase, contentLocation := tc.contentBase, tc.contentLocation
			gotCh := make(chan string, 1)
			s := testserver.New(t, testserver.Options{Handle: func(sc *testserver.ServerConn) {
				serve(t, sc, methodOptions, 200, "OK", publicHeader(), nil)
				serve(t, sc, methodDescribe, 200, "OK", sdpHeadersFull(contentBase, contentLocation), sdpBody)
				req, err := sc.ReadRequest()
				if err != nil {
					return
				}
				gotCh <- req.URL
				_ = sc.Respond(req, 200, "OK", setupHeaders(0, 1, testSessionID, testTimeoutS), nil)
				drainRequests(sc)
			}})

			want := strings.TrimSuffix(s.URL(""), "/") + tc.wantSuffix

			c := dialIdle(t, s.URL("/stream"))
			defer closeAndWait(t, c)
			tracks, err := c.Describe(context.Background())
			if err != nil {
				t.Fatalf("Describe: %v", err)
			}
			if err := c.Setup(context.Background(), tracks[0], rtsp.SetupOptions{}); err != nil {
				t.Fatalf("Setup: %v", err)
			}
			if got := <-gotCh; got != want {
				t.Errorf("SETUP request URL = %q, want %q", got, want)
			}
		})
	}
}
