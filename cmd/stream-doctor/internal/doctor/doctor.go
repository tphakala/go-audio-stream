package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	// stepOpen is the single pre-capture step for an HTTP progressive source,
	// replacing the RTSP DIAL/DESCRIBE/SETUP/PLAY group.
	stepOpen = "OPEN"
)

// authFailedPhrase is the result phrase for an authentication failure, shared
// by failStep (which overrides any per-step phrase with it) so the report's
// result line always matches mapExit's ExitAuth classification.
const authFailedPhrase = "authentication failed"

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
		ctx:      ctx,
		opts:     opts,
		prober:   prober,
		out:      out,
		errOut:   errOut,
		env:      env,
		now:      now,
		scrubber: newPIIScrubber(opts.URL),
		report:   Report{RedactedURL: redactTarget(opts.URL), Window: opts.Duration},
	}
	return r.run()
}

// runner carries the mutable state of one Run so the per-step methods stay
// small and share the growing Report and Result.
type runner struct {
	ctx    context.Context
	opts   Options
	prober Prober // source-agnostic capture surface (Collect/Close)
	// rtsp and http are the negotiation surfaces, set by run() from prober's
	// dynamic type; exactly one is non-nil per run.
	rtsp   RTSPProber
	http   HTTPProber
	out    io.Writer
	errOut io.Writer
	env    Env
	now    func() time.Time

	scrubber  piiScrubber
	report    Report
	res       Result
	termErr   error
	audio     rtsp.Track
	discarded int
	frames    []CapturedFrame
}

// run drives the pre-capture steps in order, stopping at the first that fails,
// then captures, renders once, and returns. The pre-capture step list is chosen
// by the prober's negotiation surface: the RTSP handshake group, or a single
// HTTP OPEN. The shared capture, finish, listen, and render steps run for both.
func (r *runner) run() (Result, error) {
	var pre []func() bool
	switch p := r.prober.(type) {
	case HTTPProber:
		r.http = p
		pre = []func() bool{r.open}
	case RTSPProber:
		r.rtsp = p
		pre = []func() bool{r.dial, r.describe, r.selectAudio, r.setup, r.play}
	default:
		// proberFor only builds an RTSP or HTTP prober, so this is unreachable
		// in production. A bare Prober (neither negotiation surface) has nothing
		// to probe: record a terminal usage error and PhaseStart so mapExit
		// yields ExitUsage, rather than letting a nil termErr render as a clean,
		// misleading success.
		r.res.Phase = PhaseStart
		r.termErr = fmt.Errorf("%w: unsupported source", ErrUsage)
		r.report.Result = "unsupported source"
		r.render()
		return r.res, r.termErr
	}

	for _, step := range pre {
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

// open times the HTTP handshake as a single OPEN step. On success it populates
// the synthesized single track and the source identity, mirroring what the RTSP
// DESCRIBE and SETUP steps populate for the shared capture, render, and listen
// paths. On failure it classifies the phrase by error class (openPhrase), with
// an HTTP 401 folded into the auth phrase by failStep.
func (r *runner) open() bool {
	hp := r.http
	elapsed, err := timed(r.now, func() error { return hp.Open(r.ctx) })
	if err != nil {
		r.failStep(stepOpen, elapsed, PhaseDial, openPhrase(err), err)
		return false
	}

	track := hp.Track()
	r.audio = track
	r.report.Kind = SourceHTTP
	r.report.AudioTrack = track
	r.report.Tracks = []rtsp.Track{track}
	r.report.HaveAudio = true

	// The Server header is peer-controlled text both the report and the OPEN
	// detail display, so scrub it once here at the boundary. The identity URL
	// is never rendered (the target line uses the redacted URL), so it is
	// retained raw for completeness.
	info := hp.Info()
	info.Server = r.scrubber.scrubString(info.Server)
	r.report.Source = info
	r.report.SourceAuth = httpAuthScheme(r.opts)

	r.okStep(stepOpen, elapsed, openDetail(track, info.Server))
	return true
}

// dial times the connect and OPTIONS exchange.
func (r *runner) dial() bool {
	elapsed, err := timed(r.now, func() error { return r.rtsp.Dial(r.ctx) })
	if err != nil {
		r.failStep(stepDial, elapsed, PhaseDial, "connection failed", err)
		return false
	}
	r.refreshSession()
	r.okStep(stepDial, elapsed, dialDetail(&r.report.Session))
	return true
}

// describe times the SDP fetch and stores the discovered tracks.
func (r *runner) describe() bool {
	var tracks []rtsp.Track
	elapsed, err := timed(r.now, func() error {
		var e error
		tracks, e = r.rtsp.Describe(r.ctx)
		return e
	})
	if err != nil {
		r.failStep(stepDescribe, elapsed, PhaseDescribe, "describe failed", err)
		return false
	}
	// Copy before scrubbing: the prober owns the slice it returned (a fake or a
	// future prober may share or reuse it), so mutate an owned copy, never the
	// caller's backing array. The Track fields scrubbed below are value types
	// (a string and an interface holding a value codec), so a shallow copy fully
	// isolates the writes.
	scrubbed := make([]rtsp.Track, len(tracks))
	copy(scrubbed, tracks)
	// Scrub every camera-controlled SDP string once, at the boundary: the raw
	// fmtp and an unknown codec's rtpmap are both rendered by both renderers, so
	// a hostile stream must not leak PII or break the report's code fence
	// through either. Scrubbing here means the renderers can display them raw.
	for i := range scrubbed {
		scrubbed[i].FMTP = r.scrubber.scrubString(scrubbed[i].FMTP)
		if cu, ok := scrubbed[i].Codec.(audiostream.CodecUnknown); ok {
			cu.RTPMap = r.scrubber.scrubString(cu.RTPMap)
			scrubbed[i].Codec = cu
		}
	}
	r.report.Tracks = scrubbed
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
		if e := r.rtsp.Setup(r.ctx, audio, rtsp.SetupOptions{}); e != nil {
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
			if e := r.rtsp.Setup(r.ctx, t, rtsp.SetupOptions{Discard: true}); e != nil {
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
	r.refreshSession()
	r.okStep(stepSetup, elapsed, setupDetail(&r.report.Session, audio, r.discarded))
	return true
}

// play times PLAY and records the negotiated session timeout.
func (r *runner) play() bool {
	elapsed, err := timed(r.now, func() error { return r.rtsp.Play(r.ctx) })
	if err != nil {
		r.failStep(stepPlay, elapsed, PhasePlay, "play failed", err)
		return false
	}
	r.refreshSession()
	r.okStep(stepPlay, elapsed, playDetail(&r.report.Session))
	return true
}

// capture collects audio for the window, computes statistics, and records the
// capture outcome. Collect owns its own timeout and never surfaces a terminal
// error, so the captured frame count and end reason drive the exit code.
func (r *runner) capture() {
	audio := r.audio
	cr, _ := r.prober.Collect(r.ctx, audio, r.opts.Duration)
	stats := computeStats(cr.Frames, &cr.Stats, audio.ClockRate, cr.Elapsed, cr.CapturedAt)

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

// finish sets the Report result phrase from the capture outcome. The ordering
// matches mapExitClean's precedence (unsupported codec before zero frames) so
// the human phrase and the exit code always agree.
func (r *runner) finish() {
	switch {
	case !r.res.CodecSupported:
		r.report.Result = "unsupported codec"
	case r.res.FramesCaptured == 0:
		r.report.Result = "no audio captured"
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

	// Write to a temp file in the destination directory and rename it into
	// place only on success, so a decode or write failure, or an interrupted
	// run, never truncates the destination or leaves a partial WAV, and an
	// unsupported codec (Skipped, nothing written) never clobbers an existing
	// file at the --wav path.
	dir := filepath.Dir(r.opts.WAVPath)
	tmp, err := os.CreateTemp(dir, ".stream-doctor-*.wav")
	if err != nil {
		// The raw OS error typically embeds the file path; the report never
		// shows the --wav path (privacy), so a generic reason is used here
		// instead of err.Error().
		r.report.Listen = ListenResult{Skipped: true, SkipReason: "could not create the WAV output file"}
		return
	}
	tmpName := tmp.Name()

	res, werr := writeWAV(tmp, r.audio, r.frames)
	// A flush failure surfaces only at Close, so fold it into the write error
	// rather than deferring and discarding it.
	if closeErr := tmp.Close(); werr == nil {
		werr = closeErr
	}

	switch {
	case werr != nil:
		_ = os.Remove(tmpName)
		r.report.Listen = ListenResult{Skipped: true, SkipReason: sanitizeWriteErr(werr)}
	case !res.Written:
		// A Skipped result (unsupported codec, a quirky stream) wrote nothing;
		// discard the empty temp and leave the destination untouched.
		_ = os.Remove(tmpName)
		r.report.Listen = res
	default:
		// Anchor the written WAV to absolute time when the sender clock is
		// valid: the first captured frame's RTP timestamp extrapolates to the
		// sender's wall clock. Computed before the rename below, since a
		// valid, non-zero senderStart also drives the bext splice that
		// follows; HTTP sources and cameras that send no Sender Reports
		// leave senderStart at its zero value and the WAV stays plain
		// RIFF/WAVE, unchanged from before this chunk existed.
		var senderStart time.Time
		if sc := r.report.Capture.SenderClock; sc.Valid && len(r.frames) > 0 {
			senderStart = sc.WallClock(r.frames[0].RTPTime)
		}

		// finalName is the file renamed into place: the plain WAV go-wav
		// wrote at tmpName, or, when senderStart is known, a second temp
		// file holding tmpName's bytes with a BWF bext chunk spliced in
		// (go-wav's encoder cannot write bext itself; see bext.go). A
		// splice failure is a graceful fallback, never a lost capture:
		// finalName simply stays tmpName and the WAV is written without the
		// chunk, exactly as if the sender clock had been invalid.
		finalName := tmpName
		if !senderStart.IsZero() {
			if splicedName, spliceErr := spliceBextFile(tmpName, dir, senderStart, res.SampleRate); spliceErr == nil {
				finalName = splicedName
			}
		}

		if renameErr := os.Rename(finalName, r.opts.WAVPath); renameErr != nil {
			// os.Rename returns an *os.LinkError carrying both the temp and the
			// destination paths, which sanitizeWriteErr (a *os.PathError
			// stripper) would not scrub; the report never shows a path, so use
			// a generic reason as the CreateTemp branch above does.
			_ = os.Remove(finalName)
			if finalName != tmpName {
				_ = os.Remove(tmpName)
			}
			r.report.Listen = ListenResult{Skipped: true, SkipReason: "could not finalize the WAV output file"}
			return
		}
		if finalName != tmpName {
			// The spliced copy was renamed into place; the plain WAV it was
			// built from is now redundant.
			_ = os.Remove(tmpName)
		}
		res.SenderStart = senderStart
		r.report.Listen = res
	}
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

// refreshSession snapshots the negotiated session into the report and scrubs
// the Server header once at the boundary: it is camera-controlled text that
// both renderers display, so it must not leak PII or break the report's code
// fence. Called after each step that can advance the negotiated details.
func (r *runner) refreshSession() {
	r.report.Session = r.rtsp.SessionInfo()
	r.report.Session.Server = r.scrubber.scrubString(r.report.Session.Server)
}

// okStep appends a successful handshake step.
func (r *runner) okStep(name string, elapsed time.Duration, detail string) {
	r.report.Steps = append(r.report.Steps, HandshakeStep{Name: name, OK: true, Elapsed: elapsed, Detail: detail})
}

// failStep appends a failed handshake step and records the phase, result
// phrase, and terminal error for mapExit. An authentication failure
// overrides the caller's per-step phrase so the report's Result always
// agrees with mapExit's classification (ExitAuth), whichever step the 401
// surfaced on.
func (r *runner) failStep(name string, elapsed time.Duration, phase Phase, phrase string, err error) {
	if isAuthErr(err) {
		phrase = authFailedPhrase
	}
	r.report.Steps = append(r.report.Steps, HandshakeStep{Name: name, Elapsed: elapsed, Detail: r.scrubber.scrubError(err)})
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
