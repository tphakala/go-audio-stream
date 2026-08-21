package urltarget

import (
	"net/url"
	"strings"
	"testing"
)

// Shared literals, extracted so the table rows do not trip goconst.
const (
	host   = "cam.local"
	user   = "bob"
	reqURL = "http://cam.local/s.m3u8"
	keep   = "keep"
)

func TestParseValid(t *testing.T) {
	tests := []struct {
		name     string
		rawURL   string
		cfgUser  string
		cfgPass  string
		wantTLS  bool
		wantReq  string
		wantHost string // host:port
		wantName string // hostname (no port)
		wantUser string
		wantPass string
	}{
		{
			name:     "plain http with config credentials",
			rawURL:   "http://cam.local/stream.m3u8",
			cfgUser:  user,
			cfgPass:  "s3cret",
			wantTLS:  false,
			wantReq:  "http://cam.local/stream.m3u8",
			wantHost: host,
			wantName: host,
			wantUser: user,
			wantPass: "s3cret",
		},
		{
			name:     "https with port",
			rawURL:   "https://cam.local:8443/a.m3u8",
			wantTLS:  true,
			wantReq:  "https://cam.local:8443/a.m3u8",
			wantHost: "cam.local:8443",
			wantName: host,
		},
		{
			name:     "URL userinfo overrides config",
			rawURL:   "http://alice:pw@cam.local/s.m3u8",
			cfgUser:  user,
			cfgPass:  "ignored",
			wantReq:  reqURL,
			wantHost: host,
			wantName: host,
			wantUser: "alice",
			wantPass: "pw",
		},
		{
			name:     "password-only userinfo is a real credential",
			rawURL:   "http://:onlypass@cam.local/s.m3u8",
			cfgUser:  user,
			cfgPass:  "ignored",
			wantReq:  reqURL,
			wantHost: host,
			wantName: host,
			wantUser: "",
			wantPass: "onlypass",
		},
		{
			name:     "wholly empty userinfo keeps config credentials",
			rawURL:   "http://@cam.local/s.m3u8",
			cfgUser:  user,
			cfgPass:  keep,
			wantReq:  reqURL,
			wantHost: host,
			wantName: host,
			wantUser: user,
			wantPass: keep,
		},
		{
			name:     "userinfo and fragment stripped from request URL",
			rawURL:   "https://u:p@cam.local/s.m3u8#frag",
			wantTLS:  true,
			wantReq:  "https://cam.local/s.m3u8",
			wantHost: host,
			wantName: host,
			wantUser: "u",
			wantPass: "p",
		},
		{
			// url.Parse normalizes the scheme to lowercase in String(), so the
			// request URL comes back lowercased even though the input was not.
			name:     "uppercase scheme accepted and normalized",
			rawURL:   "HTTPS://cam.local/s.m3u8",
			wantTLS:  true,
			wantReq:  "https://cam.local/s.m3u8",
			wantHost: host,
			wantName: host,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseHTTP(tt.rawURL, tt.cfgUser, tt.cfgPass)
			if err != nil {
				t.Fatalf("ParseHTTP(%q) unexpected error: %v", tt.rawURL, err)
			}
			if got.TLS != tt.wantTLS {
				t.Errorf("TLS = %v, want %v", got.TLS, tt.wantTLS)
			}
			if got.RequestURL != tt.wantReq {
				t.Errorf("RequestURL = %q, want %q", got.RequestURL, tt.wantReq)
			}
			if got.Host != tt.wantHost {
				t.Errorf("Host = %q, want %q", got.Host, tt.wantHost)
			}
			if got.Hostname != tt.wantName {
				t.Errorf("Hostname = %q, want %q", got.Hostname, tt.wantName)
			}
			if got.Username != tt.wantUser {
				t.Errorf("Username = %q, want %q", got.Username, tt.wantUser)
			}
			if got.Password != tt.wantPass {
				t.Errorf("Password = %q, want %q", got.Password, tt.wantPass)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		cfgUser string
		cfgPass string
	}{
		{name: "empty", rawURL: ""},
		{name: "whitespace only", rawURL: "   "},
		{name: "unsupported scheme", rawURL: "ftp://cam.local/s.m3u8"},
		{name: "missing host", rawURL: "http:///s.m3u8"},
		{name: "port zero", rawURL: "http://cam.local:0/s.m3u8"},
		{name: "port too large", rawURL: "http://cam.local:70000/s.m3u8"},
		{name: "CR in encoded userinfo", rawURL: "http://a%0d:p@cam.local/s.m3u8"},
		{name: "LF in encoded password", rawURL: "http://a:p%0a@cam.local/s.m3u8"},
		{name: "NUL in config username", rawURL: reqURL, cfgUser: "a\x00b"},
		{name: "CR in config password", rawURL: reqURL, cfgPass: "a\rb"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseHTTP(tt.rawURL, tt.cfgUser, tt.cfgPass); err == nil {
				t.Fatalf("ParseHTTP(%q, %q, %q) = nil error, want non-nil", tt.rawURL, tt.cfgUser, tt.cfgPass)
			}
		})
	}
}

func TestParseURL(t *testing.T) {
	if _, err := ParseURL(""); err == nil {
		t.Error("ParseURL(\"\") = nil error, want non-nil")
	}
	if _, err := ParseURL("   "); err == nil {
		t.Error("ParseURL(whitespace) = nil error, want non-nil")
	}
	u, err := ParseURL("  rtsp://cam.local:554/stream  ")
	if err != nil {
		t.Fatalf("ParseURL(valid) unexpected error: %v", err)
	}
	if u.Scheme != "rtsp" || u.Host != "cam.local:554" {
		t.Errorf("ParseURL = scheme %q host %q, want rtsp / cam.local:554", u.Scheme, u.Host)
	}
}

func TestResolveCredentials(t *testing.T) {
	mustParse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", raw, err)
		}
		return u
	}
	tests := []struct {
		name             string
		rawURL           string
		cfgUser, cfgPass string
		wantUser         string
		wantPass         string
		wantErr          bool
	}{
		{name: "config used when no userinfo", rawURL: "rtsp://cam.local/s", cfgUser: user, cfgPass: "pw", wantUser: user, wantPass: "pw"},
		{name: "userinfo overrides config", rawURL: "rtsp://a:b@cam.local/s", cfgUser: user, cfgPass: "pw", wantUser: "a", wantPass: "b"},
		{name: "password-only userinfo is real", rawURL: "rtsp://:only@cam.local/s", cfgUser: user, cfgPass: "pw", wantUser: "", wantPass: "only"},
		{name: "empty userinfo keeps config", rawURL: "rtsp://@cam.local/s", cfgUser: user, cfgPass: keep, wantUser: user, wantPass: keep},
		{name: "CR in encoded userinfo rejected", rawURL: "rtsp://a%0d:b@cam.local/s", wantErr: true},
		{name: "NUL in config password rejected", rawURL: "rtsp://cam.local/s", cfgPass: "a\x00b", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotUser, gotPass, err := ResolveCredentials(mustParse(tt.rawURL), tt.cfgUser, tt.cfgPass)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveCredentials(%q) = nil error, want non-nil", tt.rawURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveCredentials(%q) unexpected error: %v", tt.rawURL, err)
			}
			if gotUser != tt.wantUser || gotPass != tt.wantPass {
				t.Errorf("ResolveCredentials = (%q, %q), want (%q, %q)", gotUser, gotPass, tt.wantUser, tt.wantPass)
			}
		})
	}
}

func TestParseURLErrorDoesNotLeakCredentials(t *testing.T) {
	// url.Parse returns a *url.Error whose Error() embeds the whole input URL,
	// userinfo included. A malformed URL carrying credentials must not leak them
	// through the returned error, or the wrapped error reaches caller logs and
	// defeats the never-logged guarantee on the password. The DEL control byte
	// makes url.Parse fail while the userinfo is present.
	const secret = "s3cretp4ss"
	_, err := ParseHTTP("http://user:"+secret+"@cam.local/\x7f", "", "")
	if err == nil {
		t.Fatal("ParseHTTP of a control-character URL = nil error, want non-nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error text leaked the credential: %q", err.Error())
	}
	if strings.Contains(err.Error(), "cam.local") {
		t.Fatalf("error text leaked the URL host: %q", err.Error())
	}
}
