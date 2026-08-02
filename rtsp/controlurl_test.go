package rtsp_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// Fixture strings reused across the control-URL tests and fuzz seeds,
// factored out to keep the linter's duplicate-string check quiet.
const (
	requestURLCamExample   = "rtsp://user:pass@cam.example:554/stream"
	streamURLWithAuth      = "rtsp://user:pass@cam:554/stream"
	streamURLNoAuth        = "rtsp://cam:554/stream"
	controlTrackID1        = "trackID=1"
	controlQueryTrackID1   = "?trackID=1"
	resolvedStreamTrackID1 = "rtsp://user:pass@cam:554/stream/trackID=1"
	onvifTrackWithSSRC     = "rtsp://cam:554/onvif1/track1?ssrc=1"
	liveURLCamExample      = "rtsp://cam.example:554/live"
	liveURLCamExampleSlash = "rtsp://cam.example:554/live/"
	forcedOnvifBase        = "rtsp://user:pass@cam.example:554/onvif/"
)

func TestResolveBaseURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		requestURL      string
		contentBase     string
		contentLocation string
		want            string
	}{
		{
			name:       "B1 no content headers keeps request URL",
			requestURL: requestURLCamExample,
			want:       requestURLCamExample,
		},
		{
			name:        "B2 content-base absolute without credentials reattaches userinfo",
			requestURL:  requestURLCamExample,
			contentBase: "rtsp://cam.example:554/stream/",
			want:        "rtsp://user:pass@cam.example:554/stream/",
		},
		{
			name:        "B3 content-base absolute path resolves against request host",
			requestURL:  liveURLCamExample,
			contentBase: "/media/",
			want:        "rtsp://cam.example:554/media/",
		},
		{
			name:            "B4 content-location used when content-base absent",
			requestURL:      liveURLCamExample,
			contentLocation: liveURLCamExampleSlash,
			want:            liveURLCamExampleSlash,
		},
		{
			// Firmware bakes a wrong LAN or placeholder authority into the
			// Content-Base. The socket is already connected to the request
			// host, and every later aggregate request travels it, so the
			// header may contribute only its path and query; scheme and host
			// come from the request URL, matching resolveAbsoluteControl.
			name:        "B5 content-base foreign authority forced to request host",
			requestURL:  requestURLCamExample,
			contentBase: "rtsp://10.0.0.1:8554/onvif/",
			want:        forcedOnvifBase,
		},
		{
			name:        "B6 content-base non-rtsp scheme forced to request scheme and host",
			requestURL:  requestURLCamExample,
			contentBase: "http://10.0.0.1:8554/onvif/",
			want:        forcedOnvifBase,
		},
		{
			name:        "B7 content-base foreign authority keeps its path and query",
			requestURL:  requestURLCamExample,
			contentBase: "rtsp://10.0.0.1/onvif/?token=xyz",
			want:        "rtsp://user:pass@cam.example:554/onvif/?token=xyz",
		},
		{
			name:        "B8 rtsps request keeps its scheme against an rtsp content-base",
			requestURL:  "rtsps://user:pass@cam.example:322/stream",
			contentBase: "rtsp://10.0.0.1:554/onvif/",
			want:        "rtsps://user:pass@cam.example:322/onvif/",
		},
		{
			// Absolute but opaque ("rtsp:stream" parses with the segment in
			// Opaque, not Path). It has no authority to distrust and is
			// malformed as a base, so it is ignored for the request URL rather
			// than collapsing to a hostless or path-stripped base.
			name:        "B9 opaque absolute content-base is ignored for the request URL",
			requestURL:  requestURLCamExample,
			contentBase: "rtsp:stream",
			want:        requestURLCamExample,
		},
		{
			name:        "B10 content-base authority without a path yields a pathless base",
			requestURL:  requestURLCamExample,
			contentBase: "rtsp://10.0.0.1:8554",
			want:        "rtsp://user:pass@cam.example:554",
		},
		{
			name:        "B11 IPv6 request host is preserved with brackets",
			requestURL:  "rtsp://[2001:db8::1]:554/stream",
			contentBase: "rtsp://10.0.0.1/onvif/",
			want:        "rtsp://[2001:db8::1]:554/onvif/",
		},
		{
			name:        "B12 header userinfo dropped, request userinfo re-attached",
			requestURL:  requestURLCamExample,
			contentBase: "rtsp://bad:bad@10.0.0.1/onvif/",
			want:        forcedOnvifBase,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rtsp.ResolveBaseURL(tt.requestURL, tt.contentBase, tt.contentLocation)
			if err != nil {
				t.Fatalf("ResolveBaseURL(%q, %q, %q) error = %v", tt.requestURL, tt.contentBase, tt.contentLocation, err)
			}
			if got != tt.want {
				t.Errorf("ResolveBaseURL(%q, %q, %q) = %q, want %q", tt.requestURL, tt.contentBase, tt.contentLocation, got, tt.want)
			}
		})
	}
}

func TestResolveBaseURLInvalidRequestURL(t *testing.T) {
	t.Parallel()
	_, err := rtsp.ResolveBaseURL("://bad", "", "")
	if !errors.Is(err, rtsp.ErrInvalidURL) {
		t.Fatalf("ResolveBaseURL error = %v, want ErrInvalidURL", err)
	}
}

func TestResolveControlURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		base    string
		control string
		want    string
	}{
		{
			name:    "C1 generic bare token",
			base:    streamURLWithAuth,
			control: controlTrackID1,
			want:    resolvedStreamTrackID1,
		},
		{
			name:    "C2 base with trailing slash",
			base:    "rtsp://user:pass@cam:554/stream/",
			control: controlTrackID1,
			want:    resolvedStreamTrackID1,
		},
		{
			name:    "C3 leading-slash control",
			base:    streamURLNoAuth,
			control: "/trackID=1",
			want:    "rtsp://cam:554/stream/trackID=1",
		},
		{
			name:    "C4 query-only control",
			base:    streamURLNoAuth,
			control: controlQueryTrackID1,
			want:    "rtsp://cam:554/stream?trackID=1",
		},
		// The base-carrying-a-query cases. Appending is textual, so a
		// non-"?" control lands inside the existing query rather than after
		// it. Live testing confirmed this is the right form: MediaMTX
		// advertises it in its own Content-Base and recovers the query token
		// from the resulting SETUP URI, so re-attaching the query after the
		// control path is unnecessary and would needlessly rewrite every
		// request URL sent to a token-authenticated camera.
		{
			name:    "C4a relative control appends inside an existing query",
			base:    "rtsp://cam:554/stream?token=abc",
			control: controlTrackID1,
			want:    "rtsp://cam:554/stream?token=abc/trackID=1",
		},
		{
			name:    "C4b query-only control extends an existing query",
			base:    "rtsp://cam:554/stream?token=abc",
			control: controlQueryTrackID1,
			want:    "rtsp://cam:554/stream?token=abc?trackID=1",
		},
		{
			name:    "C4c absolute control keeps its own query, not the base's",
			base:    "rtsp://user:pass@cam:554/stream?token=abc",
			control: "rtsp://0.0.0.0/stream/trackID=1?sub=2",
			want:    "rtsp://user:pass@cam:554/stream/trackID=1?sub=2",
		},
		{
			name:    "C5 absolute with wrong LAN host",
			base:    streamURLWithAuth,
			control: "rtsp://192.168.1.10:554/stream/trackID=1",
			want:    resolvedStreamTrackID1,
		},
		{
			name:    "C6 placeholder host 0.0.0.0",
			base:    streamURLWithAuth,
			control: "rtsp://0.0.0.0/audio",
			want:    "rtsp://user:pass@cam:554/audio",
		},
		{
			name:    "C7 aggregate control asterisk",
			base:    streamURLNoAuth,
			control: "*",
			want:    streamURLNoAuth,
		},
		{
			name:    "C8 empty control",
			base:    streamURLNoAuth,
			control: "",
			want:    streamURLNoAuth,
		},
		{
			name:    "C9 Wowza-style streamid",
			base:    "rtsp://cam:554/media.sdp",
			control: "streamid=0",
			want:    "rtsp://cam:554/media.sdp/streamid=0",
		},
		{
			name:    "C10 Hikvision-style path",
			base:    "rtsp://cam:554/h264/ch1/main/av_stream",
			control: "track1",
			want:    "rtsp://cam:554/h264/ch1/main/av_stream/track1",
		},
		{
			name:    "C11 absolute same-host with query",
			base:    "rtsp://cam:554/onvif1",
			control: onvifTrackWithSSRC,
			want:    onvifTrackWithSSRC,
		},
		{
			name:    "C12 media-type-named control",
			base:    "rtsp://cam:8554/cam",
			control: "video",
			want:    "rtsp://cam:8554/cam/video",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rtsp.ResolveControlURL(tt.base, tt.control)
			if err != nil {
				t.Fatalf("ResolveControlURL(%q, %q) error = %v", tt.base, tt.control, err)
			}
			if got != tt.want {
				t.Errorf("ResolveControlURL(%q, %q) = %q, want %q", tt.base, tt.control, got, tt.want)
			}
		})
	}
}

func TestResolveControlURLInvalidBase(t *testing.T) {
	t.Parallel()
	_, err := rtsp.ResolveControlURL("://bad", "trackID=1")
	if !errors.Is(err, rtsp.ErrInvalidURL) {
		t.Fatalf("ResolveControlURL error = %v, want ErrInvalidURL", err)
	}
}

// The a=control value is remote input. A control carrying CR or LF must not
// come back as a "resolved" URL, or it becomes a request-line injection the
// moment a caller puts it on the wire.
func TestResolveControlURLRejectsControlCharacters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		control string
	}{
		{"crlf injecting a header", "trackID=1\r\nX-Injected: evil"},
		{"bare cr", "trackID=1\rEvil"},
		{"bare lf", "trackID=1\nEvil"},
		{"nul byte", "trackID=1\x00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rtsp.ResolveControlURL(streamURLNoAuth, tt.control)
			if !errors.Is(err, rtsp.ErrInvalidURL) {
				t.Fatalf("ResolveControlURL(%q) error = %v, want ErrInvalidURL", tt.control, err)
			}
			if got != "" {
				t.Fatalf("ResolveControlURL(%q) = %q, want empty on error", tt.control, got)
			}
		})
	}
}

// The absolute-control branch rebuilds the URL from base's scheme and host.
// When base carries neither, that assembled a degenerate string like "://"
// and returned it as a success. Found by the fuzz round-trip invariant.
func TestResolveControlURLRejectsDegenerateAbsoluteResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		base    string
		control string
	}{
		{"schemeless base with bare scheme control", "0", "rtsp:"},
		{"schemeless base with absolute control", "0", "rtsp://"},
		{"empty base with bare scheme control", "", "rtsps:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rtsp.ResolveControlURL(tt.base, tt.control)
			if err == nil {
				if _, perr := url.Parse(got); perr != nil {
					t.Fatalf("ResolveControlURL(%q, %q) = %q which does not parse: %v", tt.base, tt.control, got, perr)
				}
			}
		})
	}
}

// A URL with no "//" authority still parses, but into Opaque with User nil,
// so the ordinary userinfo redaction never fires on it.
func TestRedactURLOpaqueFormDoesNotLeakPassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
	}{
		{"opaque with credentials", "rtsp:user:secret@cam:554/stream"},
		{"opaque without path", "rtsp:user:secret@cam:554"},
		{"opaque password containing at sign", "rtsp:user:sec@ret@cam:554/stream"},
		{"unparseable opaque form", "rtsp:user:secret@ho st/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := rtsp.RedactURL(tt.in)
			if strings.Contains(got, "secret") {
				t.Fatalf("RedactURL(%q) leaked password: %q", tt.in, got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Fatalf("RedactURL(%q) did not mask userinfo: %q", tt.in, got)
			}
		})
	}
}

func TestRedactURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "credentials redacted",
			in:   "rtsp://user:secret@cam:554/stream",
			want: "rtsp://REDACTED@cam:554/stream",
		},
		{
			name: "no userinfo unchanged",
			in:   "rtsp://cam:554/stream",
			want: "rtsp://cam:554/stream",
		},
		{
			name: "unparseable without scheme separator returned unchanged",
			in:   "not a url with user:pw@host",
			want: "not a url with user:pw@host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := rtsp.RedactURL(tt.in)
			if got != tt.want {
				t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRedactURLDoesNotLeakPassword(t *testing.T) {
	t.Parallel()
	got := rtsp.RedactURL("rtsp://user:secret@cam:554/stream")
	if strings.Contains(got, "secret") {
		t.Fatalf("RedactURL leaked password: %q", got)
	}
}

func TestRedactURLFallbackOnParseFailure(t *testing.T) {
	t.Parallel()
	// This value has a space in the host, so it fails strict url.Parse and
	// exercises the best-effort scheme://userinfo@host fallback scan.
	got := rtsp.RedactURL("rtsp://user:pw@ho st/x")
	if strings.Contains(got, "pw") {
		t.Fatalf("RedactURL leaked password in fallback: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Fatalf("RedactURL fallback did not mask userinfo: %q", got)
	}
}
