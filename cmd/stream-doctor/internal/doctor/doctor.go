package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// Handshake step names, shared by the orchestration and the renderer.
const (
	stepDial     = "DIAL"
	stepDescribe = "DESCRIBE"
	stepSetup    = "SETUP"
	stepPlay     = "PLAY"
	stepCapture  = "CAPTURE"
)

// Run executes one diagnostic session: it drives prober through Dial, Describe,
// Setup (the target audio track, plus every other track discarded when
// opts.FullStream is set), Play, and Collect, timing each step with now,
// builds a Report, renders it to out (walkthrough) per opts, and returns the
// run Result and the terminal error (nil on a clean capture). Run never calls
// os.Exit; main maps the Result and error to a code via mapExit.
//
//nolint:gocritic // hugeParam: Options/Env are the documented Run signature, evaluated once per run.
func Run(ctx context.Context, opts Options, prober Prober, out io.Writer, errOut io.Writer, env Env, now func() time.Time) (Result, error) {
	r := &runner{
		ctx:    ctx,
		opts:   opts,
		prober: prober,
		out:    out,
		errOut: errOut,
		env:    env,
		now:    now,
		report: Report{RedactedURL: rtsp.RedactURL(opts.URL), Window: opts.Duration},
	}
	return r.run()
}

// runner carries the mutable state of one Run so the per-step methods stay
// small and share the growing Report and Result.
type runner struct {
	ctx    context.Context
	opts   Options
	prober Prober
	out    io.Writer
	errOut io.Writer
	env    Env
	now    func() time.Time

	report    Report
	res       Result
	termErr   error
	audio     rtsp.Track
	discarded int
	frames    []CapturedFrame
}

// run drives the pre-capture steps in order, stopping at the first that fails,
// then captures, renders once, and returns.
func (r *runner) run() (Result, error) {
	for _, step := range []func() bool{r.dial, r.describe, r.selectAudio, r.setup, r.play} {
		if !step() {
			r.render()
			return r.res, r.termErr
		}
	}
	r.capture()
	r.finish()
	r.listen()
	r.render()
	return r.res, r.termErr
}

// dial times the connect and OPTIONS exchange.
func (r *runner) dial() bool {
	elapsed, err := timed(r.now, func() error { return r.prober.Dial(r.ctx) })
	if err != nil {
		r.failStep(stepDial, elapsed, PhaseDial, "connection failed", err)
		return false
	}
	r.report.Session = r.prober.SessionInfo()
	r.okStep(stepDial, elapsed, dialDetail(&r.report.Session))
	return true
}

// describe times the SDP fetch and stores the discovered tracks.
func (r *runner) describe() bool {
	var tracks []rtsp.Track
	elapsed, err := timed(r.now, func() error {
		var e error
		tracks, e = r.prober.Describe(r.ctx)
		return e
	})
	if err != nil {
		r.failStep(stepDescribe, elapsed, PhaseDescribe, "describe failed", err)
		return false
	}
	r.report.Tracks = tracks
	r.okStep(stepDescribe, elapsed, describeDetail(tracks))
	return true
}

// selectAudio picks the first audio track; absence is a terminal, non-error
// outcome (mapExit maps it to ExitNoAudioTrack).
func (r *runner) selectAudio() bool {
	for i := range r.report.Tracks {
		if r.report.Tracks[i].Media == audiostream.MediaAudio {
			r.audio = r.report.Tracks[i]
			r.report.AudioTrack = r.audio
			r.report.HaveAudio = true
			return true
		}
	}
	r.res.Phase = PhaseDescribe
	r.res.AudioTrackFound = false
	r.report.Result = "no audio track"
	return false
}

// setup times the SETUP group: the target audio track first (Discard false),
// then, under --full-stream, every other track discarded. The whole group is
// one timed step.
func (r *runner) setup() bool {
	audio := r.audio
	elapsed, err := timed(r.now, func() error {
		if e := r.prober.Setup(r.ctx, audio, rtsp.SetupOptions{}); e != nil {
			return e
		}
		if !r.opts.FullStream {
			return nil
		}
		for i := range r.report.Tracks {
			t := r.report.Tracks[i]
			if t.ID == audio.ID {
				continue
			}
			if e := r.prober.Setup(r.ctx, t, rtsp.SetupOptions{Discard: true}); e != nil {
				return e
			}
			r.discarded++
		}
		return nil
	})
	if err != nil {
		r.failStep(stepSetup, elapsed, PhaseSetup, "setup failed", err)
		return false
	}
	r.report.Session = r.prober.SessionInfo()
	r.okStep(stepSetup, elapsed, setupDetail(&r.report.Session, audio, r.discarded))
	return true
}

// play times PLAY and records the negotiated session timeout.
func (r *runner) play() bool {
	elapsed, err := timed(r.now, func() error { return r.prober.Play(r.ctx) })
	if err != nil {
		r.failStep(stepPlay, elapsed, PhasePlay, "connection failed", err)
		return false
	}
	r.report.Session = r.prober.SessionInfo()
	r.okStep(stepPlay, elapsed, playDetail(&r.report.Session))
	return true
}

// capture collects audio for the window, computes statistics, and records the
// capture outcome. Collect owns its own timeout and never surfaces a terminal
// error, so the captured frame count and end reason drive the exit code.
func (r *runner) capture() {
	audio := r.audio
	cr, _ := r.prober.Collect(r.ctx, audio, r.opts.Duration)
	stats := computeStats(cr.Frames, cr.Stats, audio.ClockRate, cr.Elapsed)

	r.frames = cr.Frames
	r.report.Capture = stats
	r.report.CaptureShown = true
	r.report.Reason = cr.Reason
	r.report.Window = r.opts.Duration

	r.res.Phase = PhaseCapture
	r.res.AudioTrackFound = true
	r.res.CodecSupported = decodable(audio)
	r.res.FramesCaptured = len(cr.Frames)

	ok := len(cr.Frames) > 0
	detail := fmt.Sprintf("no frames, ended: %s", cr.Reason)
	if ok {
		detail = fmt.Sprintf("%d frames, %s", len(cr.Frames), cr.Reason)
	}
	r.report.Steps = append(r.report.Steps, HandshakeStep{Name: stepCapture, OK: ok, Elapsed: cr.Elapsed, Detail: detail})
}

// finish sets the Report result phrase from the capture outcome. The listen
// check and its Report.Listen population arrive in Task 4.
func (r *runner) finish() {
	switch {
	case r.res.FramesCaptured == 0:
		r.report.Result = "no audio captured"
	case !r.res.CodecSupported:
		r.report.Result = "unsupported codec"
	default:
		r.report.Result = "capture OK"
	}
}

// listen runs the WAV listen check when opts.WAVPath is set, populating
// report.Listen. It never affects the run's exit code: mapExit's
// precedence is already fully determined by Phase, AudioTrackFound,
// CodecSupported, and FramesCaptured, so any listen failure (an
// unsupported codec, a quirky stream the decoder refuses, or a write
// failure on the output file) degrades to a Skipped report entry rather
// than surfacing as a terminal error.
func (r *runner) listen() {
	if r.opts.WAVPath == "" {
		return
	}

	f, err := os.Create(r.opts.WAVPath)
	if err != nil {
		// The raw OS error typically embeds the file path; the report never
		// shows the --wav path (privacy), so a generic reason is used here
		// instead of err.Error().
		r.report.Listen = ListenResult{Skipped: true, SkipReason: "could not open the WAV output file"}
		return
	}
	defer func() { _ = f.Close() }()

	res, err := writeWAV(f, r.audio, r.frames)
	if err != nil {
		res = ListenResult{Skipped: true, SkipReason: sanitizeWriteErr(err)}
	}
	r.report.Listen = res
}

// sanitizeWriteErr redacts a writeWAV failure for the report's SkipReason.
// Most such failures are I/O errors on the --wav output file, which the
// standard library wraps in an *os.PathError (a type alias for
// *fs.PathError) carrying the local filesystem path in its Path field;
// unwrapping to the underlying cause strips that path, matching the
// os.Create branch above. No path fragment may reach the report, which
// deliberately never shows the --wav path.
func sanitizeWriteErr(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return "wav write failed: " + pathErr.Err.Error()
	}
	return "wav write failed: " + err.Error()
}

// render writes the walkthrough (and, under --report, the Markdown report)
// to the configured writers.
func (r *runner) render() {
	if r.opts.Report {
		_, _ = io.WriteString(r.out, renderReport(r.report, r.env))
		renderWalkthrough(r.errOut, r.report, r.env)
		return
	}
	renderWalkthrough(r.out, r.report, r.env)
}

// okStep appends a successful handshake step.
func (r *runner) okStep(name string, elapsed time.Duration, detail string) {
	r.report.Steps = append(r.report.Steps, HandshakeStep{Name: name, OK: true, Elapsed: elapsed, Detail: detail})
}

// failStep appends a failed handshake step and records the phase, result
// phrase, and terminal error for mapExit.
func (r *runner) failStep(name string, elapsed time.Duration, phase Phase, phrase string, err error) {
	r.report.Steps = append(r.report.Steps, HandshakeStep{Name: name, Elapsed: elapsed, Detail: failReason(err)})
	r.report.Result = phrase
	r.res.Phase = phase
	r.termErr = err
}

// timed runs fn between two now() reads and returns the elapsed duration and
// fn's error.
func timed(now func() time.Time, fn func() error) (time.Duration, error) {
	t0 := now()
	err := fn()
	return now().Sub(t0), err
}
