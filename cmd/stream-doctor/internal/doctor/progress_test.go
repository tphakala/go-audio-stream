package doctor

import (
	"bytes"
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// progressFake is a minimal Prober that also reports progress, used to exercise
// the meter's terminal gate without a network or a camera.
type progressFake struct{}

func (progressFake) Collect(context.Context, rtsp.Track, time.Duration) (CaptureResult, error) {
	return CaptureResult{}, nil
}
func (progressFake) Close() error              { return nil }
func (progressFake) Progress() CaptureProgress { return CaptureProgress{} }

func TestDrawProgressLine(t *testing.T) {
	var buf bytes.Buffer
	base := time.Unix(1000, 0)
	r := &runner{
		errOut: &buf,
		opts:   Options{Duration: 10 * time.Second},
		now:    func() time.Time { return base.Add(4 * time.Second) },
	}
	r.drawProgress(base, CaptureProgress{Packets: 82, Lost: 1, Malformed: 2})
	const want = "\r\033[K  capturing  4s / 10s   82 packets   1 lost   2 malformed"
	if got := buf.String(); got != want {
		t.Errorf("drawProgress = %q, want %q", got, want)
	}
}

func TestClearProgress(t *testing.T) {
	var buf bytes.Buffer
	r := &runner{errOut: &buf}
	r.clearProgress()
	if got := buf.String(); got != "\r\033[K" {
		t.Errorf("clearProgress = %q, want %q", got, "\r\033[K")
	}
}

// TestStartProgressMeterSilentOnNonTerminal is the guard that keeps piped
// output and golden tests byte-stable: a non-terminal writer must draw nothing,
// even when the prober can report progress.
func TestStartProgressMeterSilentOnNonTerminal(t *testing.T) {
	var buf bytes.Buffer
	r := &runner{
		prober: progressFake{},
		errOut: &buf, // a *bytes.Buffer is not a terminal
		opts:   Options{Duration: 10 * time.Second},
		now:    time.Now,
	}
	stop := r.startProgressMeter()
	stop()
	if buf.Len() != 0 {
		t.Errorf("meter wrote %q to a non-terminal writer, want nothing", buf.String())
	}
}

func TestIsTerminal(t *testing.T) {
	if isTerminal(&bytes.Buffer{}) {
		t.Error("a *bytes.Buffer must not be reported as a terminal")
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer func() { _ = pr.Close() }()
	defer func() { _ = pw.Close() }()
	if isTerminal(pw) {
		t.Error("a pipe write end must not be reported as a terminal")
	}

	// /dev/null is a character device on Unix, the same device class as a tty,
	// so it exercises the positive branch. Windows NUL is not reliably a
	// character device through os.Stat, so skip the positive assertion there.
	if runtime.GOOS != "windows" {
		devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", os.DevNull, err)
		}
		defer func() { _ = devnull.Close() }()
		if !isTerminal(devnull) {
			t.Errorf("%s must be reported as a character device", os.DevNull)
		}
	}
}

func TestFrameSinkCount(t *testing.T) {
	s := &frameSink{maxFrames: 10, maxBytes: 1000}
	if got := s.count(); got != 0 {
		t.Fatalf("empty sink count = %d, want 0", got)
	}
	s.onFrame(audiostream.Frame{Data: []byte{1, 2, 3}})
	s.onFrame(audiostream.Frame{Data: []byte{4, 5}})
	if got := s.count(); got != 2 {
		t.Errorf("sink count = %d, want 2", got)
	}
}
