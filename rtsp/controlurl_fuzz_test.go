package rtsp_test

import (
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
			_ = rtsp.RedactURL(result)
		}
		_ = rtsp.RedactURL(base)
		_ = rtsp.RedactURL(control)
	})
}
