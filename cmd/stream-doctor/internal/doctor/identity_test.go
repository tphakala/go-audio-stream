package doctor

import (
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

func TestRenderIdentity(t *testing.T) {
	var b strings.Builder
	renderIdentity(&b, &rtsp.SessionInfo{
		SDPSessionName: `Session streamed by "preview"`,
		SDPTool:        "BC Streaming Media v202210012022.10.01",
	})
	got := b.String()
	for _, want := range []string{"identity", "sdp name", `Session streamed by "preview"`, "sdp tool", "BC Streaming Media"} {
		if !strings.Contains(got, want) {
			t.Errorf("identity block missing %q:\n%q", want, got)
		}
	}
}

// TestRenderIdentityPartial shows only the advertised line: a stream that named
// a tool but no session name emits the tool line and no name line.
func TestRenderIdentityPartial(t *testing.T) {
	var b strings.Builder
	renderIdentity(&b, &rtsp.SessionInfo{SDPTool: "LIVE555 Streaming Media"})
	got := b.String()
	if !strings.Contains(got, "sdp tool") || !strings.Contains(got, "LIVE555") {
		t.Errorf("identity block missing the tool line:\n%q", got)
	}
	if strings.Contains(got, "sdp name") {
		t.Errorf("identity block showed a name line for an absent session name:\n%q", got)
	}
}

// TestRenderIdentityOmittedWhenSilent proves a stream that advertised neither a
// session name nor a tool produces no identity block at all.
func TestRenderIdentityOmittedWhenSilent(t *testing.T) {
	var b strings.Builder
	renderIdentity(&b, &rtsp.SessionInfo{})
	if b.Len() != 0 {
		t.Errorf("identity block rendered for a silent stream:\n%q", b.String())
	}
}
