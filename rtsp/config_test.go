package rtsp

import (
	"testing"
	"time"
)

func TestApplyDefaults(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	cfg.applyDefaults()
	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", cfg.Timeout, DefaultTimeout)
	}
	if cfg.UserAgent != DefaultUserAgent {
		t.Errorf("UserAgent = %q, want %q", cfg.UserAgent, DefaultUserAgent)
	}

	custom := Config{Timeout: 3 * time.Second, UserAgent: "custom/1"}
	custom.applyDefaults()
	if custom.Timeout != 3*time.Second {
		t.Errorf("custom Timeout = %v, want 3s", custom.Timeout)
	}
	if custom.UserAgent != "custom/1" {
		t.Errorf("custom UserAgent = %q, want custom/1", custom.UserAgent)
	}
}

func TestParseTargetSchemes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		in       string
		wantTLS  bool
		wantAddr string
	}{
		{"rtsp default port", "rtsp://host/path", false, "host:554"},
		{"rtsps default port", "rtsps://host/path", true, "host:322"},
		{"rtsp explicit port", "rtsp://host:8554/path", false, "host:8554"},
		{"rtsps explicit port", "rtsps://host:8555/path", true, "host:8555"},
		{"uppercase scheme", "RTSP://host/path", false, "host:554"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tg, err := parseTarget(&Config{URL: tc.in})
			if err != nil {
				t.Fatalf("parseTarget(%q): %v", tc.in, err)
			}
			if tg.tls != tc.wantTLS {
				t.Errorf("tls = %v, want %v", tg.tls, tc.wantTLS)
			}
			if tg.address != tc.wantAddr {
				t.Errorf("address = %q, want %q", tg.address, tc.wantAddr)
			}
		})
	}
}

func TestParseTargetCredentials(t *testing.T) {
	t.Parallel()

	// Userinfo in the URL wins and is stripped from the request URL.
	tg, err := parseTarget(&Config{URL: "rtsp://user:pass@host:554/cam"})
	if err != nil {
		t.Fatalf("parseTarget with userinfo: %v", err)
	}
	if tg.username != "user" || tg.password != "pass" {
		t.Errorf("creds = %q/%q, want user/pass", tg.username, tg.password)
	}
	if tg.requestURL != "rtsp://host:554/cam" {
		t.Errorf("requestURL = %q, want rtsp://host:554/cam (no userinfo)", tg.requestURL)
	}

	// Config credentials are used when the URL has no userinfo.
	tg2, err := parseTarget(&Config{URL: "rtsp://host/cam", Username: "u2", Password: "p2"})
	if err != nil {
		t.Fatalf("parseTarget with config creds: %v", err)
	}
	if tg2.username != "u2" || tg2.password != "p2" {
		t.Errorf("creds = %q/%q, want u2/p2", tg2.username, tg2.password)
	}

	// Userinfo overrides Config credentials.
	tg3, err := parseTarget(&Config{URL: "rtsp://ui:up@host/cam", Username: "u2", Password: "p2"})
	if err != nil {
		t.Fatalf("parseTarget override: %v", err)
	}
	if tg3.username != "ui" || tg3.password != "up" {
		t.Errorf("creds = %q/%q, want ui/up", tg3.username, tg3.password)
	}
}

func TestParseTargetInvalid(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "   ", "http://host/x", "://bad", "rtsp://", "rtsp:///nohost"} {
		if _, err := parseTarget(&Config{URL: in}); err == nil {
			t.Errorf("parseTarget(%q) = nil error, want error", in)
		}
	}
}
