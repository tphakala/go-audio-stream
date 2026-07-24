package doctor

import (
	"errors"
	"testing"
	"time"
)

const testStreamURL = "rtsp://cam/stream"

const durationFlag = "--duration"

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
}

func TestParseArgsAllFlags(t *testing.T) {
	t.Parallel()
	args := []string{
		durationFlag, "5s",
		"--timeout", "3s",
		"--read-idle", "7s",
		"--wav", "out.wav",
		"--report",
		"--insecure-tls",
		"--full-stream",
		"--user", "u",
		"--password", "p",
		testStreamURL,
	}
	opts, err := parseArgs(args)
	if err != nil {
		t.Fatalf("parseArgs() error = %v, want nil", err)
	}
	want := Options{
		URL:         testStreamURL,
		Duration:    5 * time.Second,
		Timeout:     3 * time.Second,
		ReadIdle:    7 * time.Second,
		WAVPath:     "out.wav",
		Report:      true,
		InsecureTLS: true,
		FullStream:  true,
		Username:    "u",
		Password:    "p",
	}
	if opts != want {
		t.Errorf("parseArgs() = %+v, want %+v", opts, want)
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
