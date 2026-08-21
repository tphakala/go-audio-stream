package doctor

import (
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// TestDescribeDetailAuthOutcome proves the describe detail makes the login
// visible: the negotiated scheme on an authenticated stream, and a plain note
// when the stream needed no login, without dropping the track counts.
func TestDescribeDetailAuthOutcome(t *testing.T) {
	tracks := []rtsp.Track{aacTrack(), videoTrack()}

	digest := describeDetail(tracks, rtsp.AuthDigest)
	if !strings.Contains(digest, "1 audio track, 1 video track") {
		t.Errorf("describeDetail dropped the track counts: %q", digest)
	}
	if !strings.Contains(digest, "Digest auth OK") {
		t.Errorf("describeDetail with Digest = %q, want the login noted", digest)
	}

	none := describeDetail(tracks, rtsp.AuthNone)
	if !strings.Contains(none, "no auth required") {
		t.Errorf("describeDetail with no auth = %q, want it noted as open", none)
	}
}
