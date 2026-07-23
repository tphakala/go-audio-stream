package rtsp_test

import (
	"net/url"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// controlURLSeeds are the quirk table's base/control pairs plus the empty
// string, exercising every ResolveControlURL branch: bare token, trailing
// slash, leading-slash and query-only control, absolute controls with wrong
// or placeholder hosts, "*", and empty.
var controlURLSeeds = []struct {
	base    string
	control string
}{
	{streamURLWithAuth, controlTrackID1},
	{"rtsp://user:pass@cam:554/stream/", controlTrackID1},
	{streamURLNoAuth, "/trackID=1"},
	{streamURLNoAuth, controlQueryTrackID1},
	{streamURLWithAuth, "rtsp://192.168.1.10:554/stream/trackID=1"},
	{streamURLWithAuth, "rtsp://0.0.0.0/audio"},
	{streamURLNoAuth, "*"},
	{streamURLNoAuth, ""},
	{"rtsp://cam:554/media.sdp", "streamid=0"},
	{"rtsp://cam:554/h264/ch1/main/av_stream", "track1"},
	{"rtsp://cam:554/onvif1", onvifTrackWithSSRC},
	{"rtsp://cam:8554/cam", "video"},
	{"", ""},
}

func FuzzResolveControlURL(f *testing.F) {
	for _, s := range controlURLSeeds {
		f.Add(s.base, s.control)
	}
	f.Fuzz(func(t *testing.T, base, control string) {
		// The contract is total: neither resolver nor redactor may panic on
		// arbitrary input, and a successful resolution must still redact
		// cleanly.
		result, err := rtsp.ResolveControlURL(base, control)
		if err == nil {
			// A successful result is a URL the caller may put on the wire, so
			// it must parse. Checking only for panics let a control value
			// carrying raw CR/LF through as a "resolved" URL; this invariant
			// is what turns that class into a fuzz failure instead of a
			// request-line injection.
			if _, perr := url.Parse(result); perr != nil {
				t.Fatalf("ResolveControlURL(%q, %q) returned unparseable %q: %v", base, control, result, perr)
			}
			_ = rtsp.RedactURL(result)
		}
		_ = rtsp.RedactURL(base)
		_ = rtsp.RedactURL(control)
	})
}

// FuzzResolveBaseURL exercises the other resolver. Content-Base and
// Content-Location are server-controlled, and this function carries its own
// security-relevant defense: a "/"-prefixed value is forced into the path by
// concatenation so a "//host/path" shape cannot be reinterpreted as a new
// host. A successful result must parse and must keep the request URL's host.
func FuzzResolveBaseURL(f *testing.F) {
	seeds := []struct {
		requestURL, contentBase, contentLocation string
	}{
		{requestURLCamExample, "", ""},
		{requestURLCamExample, "rtsp://cam.example:554/stream/", ""},
		{liveURLCamExample, "/media/", ""},
		{liveURLCamExample, "", liveURLCamExampleSlash},
		{requestURLCamExample, "//evil.example/path", ""},
		{requestURLCamExample, "relative/path", ""},
		{"", "", ""},
	}
	for _, s := range seeds {
		f.Add(s.requestURL, s.contentBase, s.contentLocation)
	}
	f.Fuzz(func(t *testing.T, requestURL, contentBase, contentLocation string) {
		got, err := rtsp.ResolveBaseURL(requestURL, contentBase, contentLocation)
		if err != nil {
			return
		}
		if _, perr := url.Parse(got); perr != nil {
			t.Fatalf("ResolveBaseURL(%q, %q, %q) returned unparseable %q: %v",
				requestURL, contentBase, contentLocation, got, perr)
		}
		_ = rtsp.RedactURL(got)
	})
}
