package doctor

import (
	"crypto/x509"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// timeoutError is a minimal net.Error that reports a timeout, standing in for a
// dial or request deadline without a real network.
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return false }

// testUser is a shared stand-in username for the classifier tests.
const testUser = "admin"

// wrappedConnRefused returns a connection-refused error wrapped the way the Go
// net stack wraps it (an *net.OpError over an *os.SyscallError), so tests
// exercise the same errors.Is path the classifier sees in production.
func wrappedConnRefused() error {
	return &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

func TestClassifyFailure(t *testing.T) {
	withCreds := Options{Username: testUser, Password: "x"}
	noCreds := Options{URL: "rtsp://cam.example/stream"}

	tests := []struct {
		name       string
		step       string
		err        error
		opts       Options
		wantResult string
		wantReason string
		wantHint   bool
	}{
		{
			name:       "401 with credentials is a rejection",
			step:       stepDescribe,
			err:        rtsp.ErrAuthFailed,
			opts:       withCreds,
			wantResult: resultAuthFailed,
			wantReason: "the server rejected the username or password",
			wantHint:   true,
		},
		{
			name:       "401 without credentials is a missing login",
			step:       stepDescribe,
			err:        &rtsp.UnauthorizedError{},
			opts:       noCreds,
			wantResult: "authentication required",
			wantReason: "the stream requires a login but no credentials were given",
			wantHint:   true,
		},
		{
			name:       "unsupported auth scheme",
			step:       stepDescribe,
			err:        rtsp.ErrUnsupportedAuth,
			opts:       withCreds,
			wantResult: resultAuthFailed,
			wantReason: "the server's authentication scheme is not supported",
		},
		{
			name:       "TLS unknown authority",
			step:       stepDial,
			err:        x509.UnknownAuthorityError{},
			opts:       noCreds,
			wantResult: "TLS certificate not trusted",
			wantReason: "the server's TLS certificate is signed by an unknown authority",
			wantHint:   true,
		},
		{
			name:       "DNS not found",
			step:       stepDial,
			err:        &net.DNSError{IsNotFound: true},
			opts:       noCreds,
			wantResult: "host not found",
			wantReason: "the hostname could not be resolved (no such host)",
			wantHint:   true,
		},
		{
			name:       "DNS timeout",
			step:       stepDial,
			err:        &net.DNSError{IsTimeout: true},
			opts:       noCreds,
			wantResult: "DNS lookup failed",
			wantReason: "the hostname could not be resolved (lookup failed or timed out)",
			wantHint:   true,
		},
		{
			name:       "connection refused (wrapped like the net stack)",
			step:       stepDial,
			err:        wrappedConnRefused(),
			opts:       noCreds,
			wantResult: resultConnRefused,
			wantReason: "nothing is listening on that port",
			wantHint:   true,
		},
		{
			name:       "host unreachable",
			step:       stepDial,
			err:        syscall.EHOSTUNREACH,
			opts:       noCreds,
			wantResult: resultHostUnreachable,
			wantReason: "no route to the host",
			wantHint:   true,
		},
		{
			name:       "dial timeout names the connect",
			step:       stepDial,
			err:        timeoutError{},
			opts:       noCreds,
			wantResult: "connection timed out",
			wantReason: "timed out connecting: the host is down, unreachable, or firewalled",
			wantHint:   true,
		},
		{
			name:       "post-dial timeout is generic",
			step:       stepDescribe,
			err:        timeoutError{},
			opts:       noCreds,
			wantResult: "timed out",
			wantReason: "the server did not respond in time",
			wantHint:   true,
		},
		{
			name:       "UDP transport rejected",
			step:       stepSetup,
			err:        rtsp.ErrUDPSetupRejected,
			opts:       noCreds,
			wantResult: resultTransportRejected,
			wantReason: "the server rejected the requested media transport",
			wantHint:   true,
		},
		{
			name:       "404 not found",
			step:       stepDescribe,
			err:        &rtsp.ResponseError{Code: 404, Reason: "Not Found"},
			opts:       noCreds,
			wantResult: "stream not found",
			wantReason: "the stream path was not found (404)",
			wantHint:   true,
		},
		{
			name:       "403 forbidden",
			step:       stepDescribe,
			err:        &rtsp.ResponseError{Code: 403},
			opts:       noCreds,
			wantResult: "access forbidden",
			wantReason: "the server refused access to the stream (403)",
		},
		{
			name:       "461 unsupported transport",
			step:       stepSetup,
			err:        &rtsp.ResponseError{Code: 461},
			opts:       noCreds,
			wantResult: resultTransportRejected,
			wantReason: "the server rejected the media transport (461)",
			wantHint:   true,
		},
		{
			name:       "other status falls back to the code",
			step:       stepPlay,
			err:        &rtsp.ResponseError{Code: 500},
			opts:       noCreds,
			wantResult: "server error",
			wantReason: "the server returned status 500",
		},
		{
			name:       "not SDP",
			step:       stepDescribe,
			err:        rtsp.ErrNotSDP,
			opts:       noCreds,
			wantResult: "bad stream description",
			wantReason: "the server's DESCRIBE response was not SDP",
		},
		{
			name:       "connection dropped names the step",
			step:       stepPlay,
			err:        rtsp.ErrConnectionClosed,
			opts:       noCreds,
			wantResult: "connection dropped",
			wantReason: "the connection closed unexpectedly during play",
			wantHint:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classifyFailure(tc.step, tc.err, tc.opts)
			if !ok {
				t.Fatalf("classifyFailure(%q) ok = false, want true", tc.name)
			}
			if got.result != tc.wantResult {
				t.Errorf("result = %q, want %q", got.result, tc.wantResult)
			}
			if got.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.reason, tc.wantReason)
			}
			if (got.hint != "") != tc.wantHint {
				t.Errorf("hint = %q, wantHint = %v", got.hint, tc.wantHint)
			}
		})
	}
}

func TestClassifyFailureUnrecognized(t *testing.T) {
	if _, ok := classifyFailure(stepDial, errors.New("some novel error"), Options{}); ok {
		t.Error("an unrecognized error must return ok=false so the caller keeps its fallback")
	}
	if _, ok := classifyFailure(stepDial, nil, Options{}); ok {
		t.Error("a nil error must return ok=false")
	}
}

func TestCredentialsProvided(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want bool
	}{
		{"flags", Options{Username: testUser, Password: "p"}, true},
		{"username flag only", Options{Username: testUser}, true},
		{"url userinfo", Options{URL: "rtsp://admin:p@cam.example/s"}, true},
		{"url user only", Options{URL: "rtsp://admin@cam.example/s"}, true},
		{"no creds", Options{URL: "rtsp://cam.example/s"}, false},
		{"unparseable url, no flags", Options{URL: "://bad"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := credentialsProvided(tc.opts); got != tc.want {
				t.Errorf("credentialsProvided = %v, want %v", got, tc.want)
			}
		})
	}
}
