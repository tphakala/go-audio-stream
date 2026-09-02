package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const testStreamURL = "rtsp://cam/stream"

const (
	durationFlag    = "--duration"
	timeoutFlag     = "--timeout"
	readIdleFlag    = "--read-idle"
	transportFlag   = "--transport"
	g726PackingFlag = "--g726-packing"
)

func TestParseArgsDefaults(t *testing.T) {
	t.Parallel()
	opts, err := parseArgs([]string{testStreamURL})
	if err != nil {
		t.Fatalf("parseArgs() error = %v, want nil", err)
	}
	if opts.URL != testStreamURL {
		t.Errorf("URL = %q, want %q", opts.URL, testStreamURL)
	}
	if opts.Duration != DefaultDuration {
		t.Errorf("Duration = %v, want %v", opts.Duration, DefaultDuration)
	}
	if opts.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", opts.Timeout, DefaultTimeout)
	}
	if opts.ReadIdle != DefaultReadIdle {
		t.Errorf("ReadIdle = %v, want %v", opts.ReadIdle, DefaultReadIdle)
	}
	if opts.Report {
		t.Error("Report = true, want false")
	}
	if opts.WAVPath != "" {
		t.Errorf("WAVPath = %q, want empty", opts.WAVPath)
	}
	if opts.Transport != transportTCP {
		t.Errorf("Transport = %q, want tcp (default)", opts.Transport)
	}
}

func TestParseArgsAllFlags(t *testing.T) {
	t.Parallel()
	args := []string{
		durationFlag, "5s",
		timeoutFlag, "3s",
		readIdleFlag, "7s",
		"--wav", testWAVName,
		"--report",
		"--insecure-tls",
		"--insecure-auth",
		"--full-stream",
		transportFlag, transportUDP,
		g726PackingFlag, g726PackingAAL2,
		"--user", "u",
		"--password", "p",
		testStreamURL,
	}
	opts, err := parseArgs(args)
	if err != nil {
		t.Fatalf("parseArgs() error = %v, want nil", err)
	}
	want := Options{
		URL:          testStreamURL,
		Duration:     5 * time.Second,
		Timeout:      3 * time.Second,
		ReadIdle:     7 * time.Second,
		WAVPath:      testWAVName,
		Report:       true,
		InsecureTLS:  true,
		InsecureAuth: true,
		FullStream:   true,
		Transport:    transportUDP,
		G726Packing:  g726PackingAAL2,
		Username:     "u",
		Password:     "p",
	}
	if opts != want {
		t.Errorf("parseArgs() = %+v, want %+v", opts, want)
	}
}

func TestParseArgsInsecureAuthDefaultsOff(t *testing.T) {
	t.Parallel()
	opts, err := parseArgs([]string{testStreamURL})
	if err != nil {
		t.Fatalf("parseArgs() error = %v, want nil", err)
	}
	if opts.InsecureAuth {
		t.Error("InsecureAuth = true by default, want false (plaintext credentials must be opt-in)")
	}
}

func TestUsageTextCoversHTTP(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"rtsp-or-http-url", "-insecure-auth", "WAV or raw PCM/L16", "-transport"} {
		if !strings.Contains(usageText, want) {
			t.Errorf("usageText missing %q:\n%s", want, usageText)
		}
	}
}

func TestParseArgsMissingURL(t *testing.T) {
	t.Parallel()
	_, err := parseArgs(nil)
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("parseArgs() error = %v, want ErrUsage", err)
	}
}

func TestParseArgsExtraPositional(t *testing.T) {
	t.Parallel()
	_, err := parseArgs([]string{"a", "b"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("parseArgs() error = %v, want ErrUsage", err)
	}
}

func TestParseArgsBadDuration(t *testing.T) {
	t.Parallel()
	_, err := parseArgs([]string{durationFlag, "later", "rtsp://x"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("parseArgs() error = %v, want ErrUsage", err)
	}
}

func TestParseArgsUnknownFlag(t *testing.T) {
	t.Parallel()
	_, err := parseArgs([]string{"--frobnicate", "rtsp://x"})
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("parseArgs() error = %v, want ErrUsage", err)
	}
}

func TestParseArgsVersion(t *testing.T) {
	t.Parallel()
	opts, err := parseArgs([]string{"--version"})
	if !errors.Is(err, errVersionRequested) {
		t.Fatalf("parseArgs() error = %v, want errVersionRequested", err)
	}
	if opts.URL != "" {
		t.Errorf("URL = %q, want empty", opts.URL)
	}
}

func TestParseArgsNonPositiveDuration(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"0s", "-5s"} {
		if _, err := parseArgs([]string{durationFlag, arg, testStreamURL}); !errors.Is(err, ErrUsage) {
			t.Errorf("parseArgs(%s %s) error = %v, want ErrUsage", durationFlag, arg, err)
		}
	}
}

func TestParseArgsNonPositiveTimeout(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"0s", "-5s"} {
		if _, err := parseArgs([]string{timeoutFlag, arg, testStreamURL}); !errors.Is(err, ErrUsage) {
			t.Errorf("parseArgs(%s %s) error = %v, want ErrUsage", timeoutFlag, arg, err)
		}
	}
}

func TestParseArgsReadIdle(t *testing.T) {
	t.Parallel()
	// A negative read-idle is a usage error.
	if _, err := parseArgs([]string{readIdleFlag, "-1s", testStreamURL}); !errors.Is(err, ErrUsage) {
		t.Errorf("parseArgs(%s -1s) error = %v, want ErrUsage", readIdleFlag, err)
	}
	// A zero read-idle is allowed: it disables the watchdog.
	if _, err := parseArgs([]string{readIdleFlag, "0s", testStreamURL}); err != nil {
		t.Errorf("parseArgs(%s 0s) error = %v, want nil (0 disables the watchdog)", readIdleFlag, err)
	}
}

func TestParseArgsTransport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		arg  string
		want string
	}{
		{transportTCP, transportTCP},
		{transportUDP, transportUDP},
		{transportUDPThenTCP, transportUDPThenTCP},
	} {
		opts, err := parseArgs([]string{transportFlag, tc.arg, testStreamURL})
		if err != nil {
			t.Errorf("parseArgs(--transport %s) error = %v, want nil", tc.arg, err)
			continue
		}
		if opts.Transport != tc.want {
			t.Errorf("Transport = %q, want %q", opts.Transport, tc.want)
		}
	}
	// An unrecognized transport is a usage error, not a silent fallback to TCP.
	if _, err := parseArgs([]string{transportFlag, "tls", testStreamURL}); !errors.Is(err, ErrUsage) {
		t.Errorf("parseArgs(--transport tls) error = %v, want ErrUsage", err)
	}
}

// TestParseArgsG726Packing covers the -g726-packing flag: the accepted values
// round-trip into Options, and an unrecognized value is a usage error rather than
// a silent fallback to the SDP packing.
func TestParseArgsG726Packing(t *testing.T) {
	t.Parallel()
	for _, want := range []string{g726PackingSDP, g726PackingRFC3551, g726PackingAAL2} {
		opts, err := parseArgs([]string{g726PackingFlag, want, testStreamURL})
		if err != nil {
			t.Errorf("parseArgs(--g726-packing %s) error = %v, want nil", want, err)
			continue
		}
		if opts.G726Packing != want {
			t.Errorf("G726Packing = %q, want %q", opts.G726Packing, want)
		}
	}
	// An unrecognized packing is a usage error, not a silent fallback to the SDP.
	if _, err := parseArgs([]string{g726PackingFlag, "msb", testStreamURL}); !errors.Is(err, ErrUsage) {
		t.Errorf("parseArgs(--g726-packing msb) error = %v, want ErrUsage", err)
	}
}
