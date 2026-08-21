package doctor

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// newLiveRunner builds a runner wired for live streaming to buf, bypassing the
// terminal check (a bytes.Buffer is never a terminal) so the live path is
// testable without a pty.
func newLiveRunner(buf *bytes.Buffer) *runner {
	return &runner{
		out:      buf,
		errOut:   buf,
		env:      testEnv(),
		scrubber: newPIIScrubber("rtsp://cam.example/stream"),
		report:   Report{RedactedURL: redactedStreamURL},
		live:     true,
	}
}

func TestLiveBannerStreamsHeaderAndConnecting(t *testing.T) {
	var buf bytes.Buffer
	r := newLiveRunner(&buf)
	r.emitLiveBanner()

	got := buf.String()
	for _, want := range []string{"stream-doctor", "target: " + redactedStreamURL, "handshake", "connecting..."} {
		if !strings.Contains(got, want) {
			t.Errorf("banner missing %q:\n%q", want, got)
		}
	}
	if !r.connectingShown {
		t.Error("connectingShown = false after the banner, want true")
	}
}

func TestLiveStepsStreamAndClearConnecting(t *testing.T) {
	var buf bytes.Buffer
	r := newLiveRunner(&buf)
	r.emitLiveBanner()
	r.okStep("DIAL", 2*time.Millisecond, "auth none")
	r.okStep("DESCRIBE", 40*time.Millisecond, "1 audio track")

	got := buf.String()
	// The first streamed row clears the transient connecting line exactly once.
	if n := strings.Count(got, "\r\033[K"); n != 1 {
		t.Errorf("clear sequence appears %d times, want exactly 1:\n%q", n, got)
	}
	if r.connectingShown {
		t.Error("connectingShown = true after the first row, want false")
	}
	if !strings.Contains(got, "DIAL") || !strings.Contains(got, "auth none") {
		t.Errorf("DIAL row was not streamed:\n%q", got)
	}
	if !strings.Contains(got, "DESCRIBE") {
		t.Errorf("DESCRIBE row was not streamed:\n%q", got)
	}
}

func TestLiveFailStepStreamsHint(t *testing.T) {
	var buf bytes.Buffer
	r := newLiveRunner(&buf)
	r.emitLiveBanner()
	r.failStep(stepDial, 3*time.Millisecond, PhaseDial, "connection failed", wrappedConnRefused())

	got := buf.String()
	if !strings.Contains(got, "nothing is listening on that port") {
		t.Errorf("failed step reason not streamed:\n%q", got)
	}
	if !strings.Contains(got, "hint:") {
		t.Errorf("failed step hint not streamed:\n%q", got)
	}
}

// TestLiveHiddenCaptureStepNotStreamed proves a successful CAPTURE step is not
// streamed as a row (its detail is the capture block), matching the batch
// renderer's skip rule.
func TestLiveHiddenCaptureStepNotStreamed(t *testing.T) {
	var buf bytes.Buffer
	r := newLiveRunner(&buf)
	r.addStep(HandshakeStep{Name: stepCapture, OK: true, Detail: "500 frames, completed"})
	if got := buf.String(); got != "" {
		t.Errorf("a hidden successful CAPTURE step was streamed:\n%q", got)
	}
}

// TestLiveNoOpWhenNotLive proves the live emitters write nothing when the run is
// not live: the batch renderer owns the output in that case.
func TestLiveNoOpWhenNotLive(t *testing.T) {
	var buf bytes.Buffer
	r := newLiveRunner(&buf)
	r.live = false
	r.emitLiveBanner()
	r.okStep("DIAL", time.Millisecond, "auth none")
	if got := buf.String(); got != "" {
		t.Errorf("live emitters wrote %q when not live, want nothing", got)
	}
	// The step is still recorded for the batch renderer.
	if len(r.report.Steps) != 1 {
		t.Errorf("okStep recorded %d steps, want 1", len(r.report.Steps))
	}
}
