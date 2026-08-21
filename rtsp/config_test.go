package rtsp

import (
	"crypto/tls"
	"errors"
	"strings"
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
	// Every case asserts ErrInvalidURL, not merely that some error occurred:
	// the wrapping IS the documented contract, and a regression that returned
	// a bare error would break errors.Is for every caller while still passing
	// a non-nil check.
	cases := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"wrong scheme", "http://host/x"},
		{"unparseable", "://bad"},
		{"no host", "rtsp://"},
		{"empty host with path", "rtsp:///nohost"},
		{"port above range", "rtsp://host:65536/cam"},
		{"port zero", "rtsp://host:0/cam"},
		{"credentials carrying CRLF", "rtsp://a%0D%0AX-Injected:%20y@host/cam"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseTarget(&Config{URL: tt.url})
			if !errors.Is(err, ErrInvalidURL) {
				t.Errorf("parseTarget(%q) error = %v, want ErrInvalidURL", tt.url, err)
			}
		})
	}
}

func TestParseTargetErrorDoesNotLeakCredentials(t *testing.T) {
	t.Parallel()
	// url.Parse returns a *url.Error whose Error() embeds the whole input URL,
	// userinfo included. A malformed URL carrying credentials must not leak them
	// through the returned error, or the wrapped error reaches caller logs and
	// exposes the password. The DEL control byte makes url.Parse fail while the
	// userinfo is present.
	const secret = "s3cretp4ss"
	_, err := parseTarget(&Config{URL: "rtsp://user:" + secret + "@cam.local/\x7f"})
	if err == nil {
		t.Fatal("parseTarget of a control-character URL = nil error, want non-nil")
	}
	if !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("error = %v, want ErrInvalidURL", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error text leaked the credential: %q", err.Error())
	}
	if strings.Contains(err.Error(), "cam.local") {
		t.Fatalf("error text leaked the URL host: %q", err.Error())
	}
}

// An empty userinfo is what a URL template produces when its substitution
// variables are unset. Treating it as an override would silently discard the
// credentials the caller supplied in Config and surface as a 401.
func TestParseTargetEmptyUserinfoKeepsConfigCredentials(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"rtsp://@host/cam", "rtsp://:@host/cam"} {
		tgt, err := parseTarget(&Config{URL: raw, Username: "u", Password: "p"})
		if err != nil {
			t.Fatalf("parseTarget(%q): %v", raw, err)
		}
		if tgt.username != "u" || tgt.password != "p" {
			t.Errorf("parseTarget(%q) credentials = %q/%q, want u/p", raw, tgt.username, tgt.password)
		}
	}
}

// A username-only userinfo deliberately clears the Config password, so a
// URL-supplied identity cannot pick up an unrelated secret.
func TestParseTargetUsernameOnlyClearsPassword(t *testing.T) {
	t.Parallel()
	tgt, err := parseTarget(&Config{URL: "rtsp://user@host/cam", Username: "u2", Password: "p2"})
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	if tgt.username != "user" || tgt.password != "" {
		t.Errorf("credentials = %q/%q, want user/empty", tgt.username, tgt.password)
	}
}

// A fragment is client-side only and must not reach the request line.
func TestParseTargetStripsFragment(t *testing.T) {
	t.Parallel()
	tgt, err := parseTarget(&Config{URL: "rtsp://host/path#frag"})
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	if strings.Contains(tgt.requestURL, "#") {
		t.Errorf("requestURL = %q, want no fragment", tgt.requestURL)
	}
}

// An IPv6 literal must survive host/port splitting and rejoining, and the TLS
// server name must be the bare address without brackets.
func TestParseTargetIPv6(t *testing.T) {
	t.Parallel()
	tgt, err := parseTarget(&Config{URL: "rtsps://[::1]:8554/cam"})
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	if tgt.address != "[::1]:8554" {
		t.Errorf("address = %q, want [::1]:8554", tgt.address)
	}
	if tgt.serverName != "::1" {
		t.Errorf("serverName = %q, want ::1", tgt.serverName)
	}
}

// applyDefaults treats a negative Timeout like a zero one, and normalizes a
// negative ReadIdle to the documented disabled value so it can never reach a
// timer that panics on a non-positive interval.
func TestApplyDefaultsNegative(t *testing.T) {
	t.Parallel()
	cfg := Config{Timeout: -1, ReadIdle: -1}
	cfg.applyDefaults()
	if cfg.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want DefaultTimeout", cfg.Timeout)
	}
	if cfg.ReadIdle != 0 {
		t.Errorf("ReadIdle = %v, want 0", cfg.ReadIdle)
	}
}

// A password-only userinfo is a real credential some cameras accept. The
// empty-userinfo guard must not swallow it.
func TestParseTargetPasswordOnlyUserinfo(t *testing.T) {
	t.Parallel()
	tgt, err := parseTarget(&Config{URL: "rtsp://:secret@host/cam", Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("parseTarget: %v", err)
	}
	if tgt.username != "" || tgt.password != "secret" {
		t.Errorf("credentials = %q/%q, want empty/secret", tgt.username, tgt.password)
	}
}

// tlsConfigFor normalizes a caller-supplied config in two ways, and the
// MinVersion floor is the security-relevant one. A socket test cannot see it,
// because the test server negotiates 1.2 anyway.
func TestTLSConfigForNormalizesCallerConfig(t *testing.T) {
	t.Parallel()
	tgt := target{serverName: "cam.example"}
	got := tlsConfigFor(&Config{TLSConfig: &tls.Config{}}, &tgt) //nolint:gosec // asserting the floor this function applies
	if got.ServerName != "cam.example" {
		t.Errorf("ServerName = %q, want it filled in from the URL host", got.ServerName)
	}
	if got.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", got.MinVersion)
	}

	// An explicit choice is preserved rather than overwritten.
	pinned := tlsConfigFor(&Config{TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "pinned"}}, &tgt)
	if pinned.MinVersion != tls.VersionTLS13 || pinned.ServerName != "pinned" {
		t.Errorf("caller values overwritten: %#x %q", pinned.MinVersion, pinned.ServerName)
	}
}
