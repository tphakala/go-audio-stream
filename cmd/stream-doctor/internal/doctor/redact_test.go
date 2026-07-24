package doctor

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

func TestRedactTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"credentials and host stripped", "rtsp://admin:hunter2@cam.example:554/stream", "rtsp://[redacted]/stream"},
		{"ip host stripped", "rtsp://user:pass@192.168.1.50:554/live", "rtsp://[redacted]/live"},
		{"no userinfo, host still stripped", "rtsp://cam.local/stream", "rtsp://[redacted]/stream"},
		{"query dropped", "rtsps://u:p@host:322/s?token=secret", "rtsps://[redacted]/s"},
		{"no path keeps only scheme", "rtsp://u:p@host", "rtsp://[redacted]"},
		{"unparseable collapses to token", "://not a url", "[redacted]"},
		{"empty collapses to token", "", "[redacted]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redactTarget(tt.in); got != tt.want {
				t.Errorf("redactTarget(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestPIIScrubberScrubError(t *testing.T) {
	t.Parallel()
	const rawURL = "rtsp://admin:hunter2@cam.example:554/stream"
	s := newPIIScrubber(rawURL)

	// A malformed-URL error echoes the raw URL (credentials and the offending
	// fragment); it must be reduced to a category, never scrubbed token by
	// token, so no credential fragment can survive.
	invalid := fmt.Errorf("%w: parse %q: invalid URL escape", rtsp.ErrInvalidURL, "rtsp://admin:S3cr3t%ZZ@cam.example/stream")
	if got := s.scrubError(invalid); strings.Contains(got, "admin") ||
		strings.Contains(got, "S3cr3t") || strings.Contains(got, "cam.example") {
		t.Errorf("scrubError leaked PII from an invalid-URL error: %q", got)
	}

	// A dial error carries the host:port; the host must be scrubbed while the
	// diagnostic text is kept.
	got := s.scrubError(errors.New("dial tcp cam.example:554: connect: connection refused"))
	if strings.Contains(got, "cam.example") {
		t.Errorf("scrubError leaked the host: %q", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("scrubError dropped the diagnostic text: %q", got)
	}

	// A resolved IP that a dial error can report even for a hostname target is
	// masked by the IP backstop, IPv4 and bracketed IPv6 alike.
	if got := s.scrubError(errors.New("dial tcp 192.168.1.50:554: i/o timeout")); strings.Contains(got, "192.168.1.50") {
		t.Errorf("scrubError leaked a resolved IPv4 address: %q", got)
	}
	if got := s.scrubError(errors.New("dial tcp [2001:db8::5]:554: connect: connection refused")); strings.Contains(got, "2001:db8") {
		t.Errorf("scrubError leaked a resolved IPv6 address: %q", got)
	}

	if s.scrubError(nil) != "" {
		t.Error("scrubError(nil) = non-empty, want empty")
	}
}
