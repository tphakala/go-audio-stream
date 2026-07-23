package rtsp_test

import (
	"errors"
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
	resolvedStreamTrackID1 = "rtsp://user:pass@cam:554/stream/trackID=1"
	onvifTrackWithSSRC     = "rtsp://cam:554/onvif1/track1?ssrc=1"
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
			requestURL:  "rtsp://cam.example:554/live",
			contentBase: "/media/",
			want:        "rtsp://cam.example:554/media/",
		},
		{
			name:            "B4 content-location used when content-base absent",
			requestURL:      "rtsp://cam.example:554/live",
			contentLocation: "rtsp://cam.example:554/live/",
			want:            "rtsp://cam.example:554/live/",
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
			control: "?trackID=1",
			want:    "rtsp://cam:554/stream?trackID=1",
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
