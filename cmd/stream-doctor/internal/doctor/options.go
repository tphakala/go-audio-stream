package doctor

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"time"
)

// Options is the fully-parsed CLI configuration for one run.
type Options struct {
	URL          string
	Duration     time.Duration
	Timeout      time.Duration
	ReadIdle     time.Duration
	WAVPath      string // "" disables the listen check
	Report       bool
	InsecureTLS  bool
	InsecureAuth bool   // permit HTTP Basic credentials over a plaintext http connection (mirrors httpsource.Config.AllowInsecureAuth)
	FullStream   bool   // set up all tracks (discarding non-target ones) for cameras that reject audio-only SETUP
	Transport    string // media transport: "tcp" (default), "udp", or "udp-then-tcp" (RTSP only, ignored for http)
	Username     string
	Password     string
}

// Defaults.
const (
	DefaultDuration = 10 * time.Second
	DefaultTimeout  = 10 * time.Second
	DefaultReadIdle = 15 * time.Second
)

// Accepted -transport flag values, mapped to a library TransportPreference by
// transportPreference.
const (
	transportTCP        = "tcp"
	transportUDP        = "udp"
	transportUDPThenTCP = "udp-then-tcp"
)

// Version is the stream-doctor version string, printed by --version and in the
// report. It defaults to "dev" for local and unversioned builds, and is
// overridden at release time via the linker:
//
//	-ldflags "-X github.com/tphakala/go-audio-stream/cmd/stream-doctor/internal/doctor.Version=<version>"
//
// It must stay a var (not a const) for -X to patch it.
var Version = "dev"

// ErrUsage marks a command-line usage error; mapExit turns it into
// ExitUsage.
var ErrUsage = errors.New("stream-doctor: usage error")

// errVersionRequested is returned by parseArgs when --version was given.
// Execute checks for it with errors.Is to print the version and exit 0
// instead of running.
var errVersionRequested = errors.New("stream-doctor: version requested")

// usageText is printed to stderr by Execute on a usage error.
const usageText = `Usage: stream-doctor [flags] <rtsp-or-http-url>

Probes an rtsp/rtsps camera stream or an http/https progressive source. An
http(s) target must serve WAV or raw PCM/L16.

Flags:
  -duration duration    capture window (default 10s)
  -timeout duration     dial and request timeout (default 10s)
  -read-idle duration   watchdog: no frames within this window ends capture (default 15s)
  -wav path             write captured audio to a WAV file
  -report               print a full diagnostic report
  -insecure-tls         skip certificate verification for rtsps and https
  -insecure-auth        permit HTTP Basic credentials over a plaintext http connection
  -full-stream          set up all tracks, not just audio, for cameras that
                        reject audio-only SETUP (RTSP only; ignored for http)
  -transport mode       media transport: tcp, udp, or udp-then-tcp
                        (default tcp; RTSP only, ignored for http)
  -user username        stream username (overridden by URL userinfo)
  -password password    stream password (overridden by URL userinfo)
  -version              print the version and exit
`

// parseArgs parses argv (excluding the program name) into Options. It
// returns a wrapped ErrUsage on a missing URL, an unknown flag, an
// unparseable duration, or extra positional arguments. When --version is
// present it returns the sentinel errVersionRequested so Execute can print
// the version and exit 0.
func parseArgs(args []string) (Options, error) {
	var opts Options
	var version bool

	fs := flag.NewFlagSet("stream-doctor", flag.ContinueOnError)
	var out bytes.Buffer
	fs.SetOutput(&out)

	fs.DurationVar(&opts.Duration, "duration", DefaultDuration, "capture window")
	fs.DurationVar(&opts.Timeout, "timeout", DefaultTimeout, "dial and request timeout")
	fs.DurationVar(&opts.ReadIdle, "read-idle", DefaultReadIdle, "watchdog idle window")
	fs.StringVar(&opts.WAVPath, "wav", "", "write captured audio to a WAV file")
	fs.BoolVar(&opts.Report, "report", false, "print a full diagnostic report")
	fs.BoolVar(&opts.InsecureTLS, "insecure-tls", false, "skip certificate verification for rtsps and https")
	fs.BoolVar(&opts.InsecureAuth, "insecure-auth", false, "permit HTTP Basic credentials over a plaintext http connection")
	fs.BoolVar(&opts.FullStream, "full-stream", false, "set up all tracks, not just audio")
	fs.StringVar(&opts.Transport, "transport", transportTCP, "media transport: tcp, udp, or udp-then-tcp")
	fs.StringVar(&opts.Username, "user", "", "stream username")
	fs.StringVar(&opts.Password, "password", "", "stream password")
	fs.BoolVar(&version, "version", false, "print the version and exit")

	if err := fs.Parse(args); err != nil {
		return Options{}, fmt.Errorf("%w: %w", ErrUsage, err)
	}

	if version {
		return Options{}, errVersionRequested
	}

	rest := fs.Args()
	switch len(rest) {
	case 0:
		return Options{}, fmt.Errorf("%w: missing stream URL", ErrUsage)
	case 1:
		opts.URL = rest[0]
	default:
		return Options{}, fmt.Errorf("%w: unexpected extra arguments: %v", ErrUsage, rest[1:])
	}

	// The capture window drives Collect's context timeout; a non-positive value
	// would expire the window immediately and capture nothing, which would
	// misreport as a capture failure rather than the usage error it is.
	if opts.Duration <= 0 {
		return Options{}, fmt.Errorf("%w: -duration must be positive, got %s", ErrUsage, opts.Duration)
	}
	if opts.Timeout <= 0 {
		return Options{}, fmt.Errorf("%w: -timeout must be positive, got %s", ErrUsage, opts.Timeout)
	}
	// A zero read-idle disables the watchdog, so only a negative value is a
	// usage error here.
	if opts.ReadIdle < 0 {
		return Options{}, fmt.Errorf("%w: -read-idle must not be negative, got %s", ErrUsage, opts.ReadIdle)
	}
	// Validate the transport selector against the same mapping the RTSP adapter
	// uses, so an unknown value fails as a usage error here rather than silently
	// falling back to TCP at Dial time.
	if _, ok := transportPreference(opts.Transport); !ok {
		return Options{}, fmt.Errorf("%w: -transport must be tcp, udp, or udp-then-tcp, got %q", ErrUsage, opts.Transport)
	}

	return opts, nil
}
