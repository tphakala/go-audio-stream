package doctor

import (
	"fmt"
	"strconv"
	"strings"
)

// reportFence wraps the whole report in one code block. A fenced block survives
// copy-paste verbatim (GitHub renders it as a single code block, preserving
// alignment) and an LLM parses the key:value text unambiguously, which is the
// point: reports are pasted into issues for a maintainer or an LLM to debug
// from. Untrusted stream-derived fields are sanitized (see sanitizeLine) so a
// hostile camera cannot break out of the fence.
const reportFence = "```"

// renderReport returns the paste-ready report for r: one fenced code block of
// plain, sectioned key:value text, no markdown tables. Every PII-bearing field
// (the target, the failure reason, the Server header, the raw fmtp) is already
// redacted in r, and no local file path appears (ListenResult carries none).
//
//nolint:gocritic // hugeParam: Report/Env match renderWalkthrough; called once per run.
func renderReport(r Report, env Env) string {
	var b strings.Builder
	fmt.Fprintln(&b, reportFence)
	fmt.Fprintf(&b, "stream-doctor %s (%s/%s)\n", env.Version, env.OS, env.Arch)
	fmt.Fprintf(&b, "target: %s\n", r.RedactedURL)
	fmt.Fprintf(&b, "result: %s\n", r.Result)
	reportHandshake(&b, &r)
	reportSession(&b, &r)
	writeTracksSection(&b, r.Tracks)
	reportNoAudio(&b, &r)
	reportCapture(&b, &r)
	reportListen(&b, &r)
	fmt.Fprintln(&b, reportFence)
	return b.String()
}

// reportStepFmt aligns the handshake step lines: name, status, elapsed.
const reportStepFmt = "  %-10s%-6s%s\n"

// reportHandshake writes the handshake step list and, when a step failed, a
// dedicated failure line naming the step and its (already scrubbed) reason. A
// successful CAPTURE step is omitted; its detail is the capture block.
func reportHandshake(b *strings.Builder, r *Report) {
	fmt.Fprintln(b)
	fmt.Fprintln(b, "handshake")
	var failed *HandshakeStep
	for i := range r.Steps {
		s := r.Steps[i]
		if s.Name == stepCapture && s.OK {
			continue
		}
		fmt.Fprintf(b, reportStepFmt, s.Name, stepStatus(s.OK), formatElapsed(s.Elapsed))
		if !s.OK && failed == nil {
			failed = &r.Steps[i]
		}
	}
	if failed != nil {
		// The reason is on its own line, not a table cell, so a long error
		// string stays readable. Detail was scrubbed at failStep.
		fmt.Fprintf(b, "failure: %s - %s\n", failed.Name, failed.Detail)
	}
}

// reportSession writes the negotiated session block once DIAL has succeeded.
// The session timeout and transport are populated by SETUP, so those two lines
// appear only once SETUP has succeeded, never as misleading zero values.
func reportSession(b *strings.Builder, r *Report) {
	if !hasStepOK(r.Steps, stepDial) {
		return
	}
	fmt.Fprintln(b)
	fmt.Fprintln(b, "session")
	if r.Session.Server != "" {
		fmt.Fprintf(b, "  server: %s\n", r.Session.Server)
	}
	fmt.Fprintf(b, "  auth: %s\n", r.Session.AuthScheme)
	setupOK := hasStepOK(r.Steps, stepSetup)
	if setupOK {
		fmt.Fprintf(b, "  session-timeout: %ds\n", int(r.Session.SessionTimeout.Seconds()))
	}
	fmt.Fprintf(b, "  keepalive: %s\n", r.Session.KeepaliveMethod)
	if setupOK {
		fmt.Fprintf(b, "  transport: TCP interleaved, %s\n", channelStr(&r.Session, r.AudioTrack.ID))
	}
}

// reportNoAudio writes the no-audio-track notice when Describe succeeded but no
// audio track was present.
func reportNoAudio(b *strings.Builder, r *Report) {
	if r.HaveAudio || !hasStepOK(r.Steps, stepDescribe) {
		return
	}
	fmt.Fprintln(b)
	fmt.Fprintln(b, "no audio track found")
}

// reportCapture writes the capture statistics block, or nothing when capture
// never ran. Malformed and ssrc-resets are library-health counters: a climbing
// malformed count points at a codec or framing mismatch.
func reportCapture(b *strings.Builder, r *Report) {
	if !r.CaptureShown {
		return
	}
	c := r.Capture
	fmt.Fprintln(b)
	fmt.Fprintf(b, "capture: track %d, window %s, ended %s\n",
		r.AudioTrack.ID, r.Window, endReasonPhrase(r.Reason))
	fmt.Fprintf(b, "  packets: %d\n", c.Packets)
	fmt.Fprintf(b, "  bytes: %d\n", c.Bytes)
	fmt.Fprintf(b, "  lost: %d (%.2f%%)\n", c.Lost, c.LossRatio*100)
	fmt.Fprintf(b, "  malformed: %d\n", c.Malformed)
	fmt.Fprintf(b, "  ssrc-resets: %d\n", c.SSRCResets)
	fmt.Fprintf(b, "  max-gap: %d\n", c.MaxGap)
	fmt.Fprintf(b, "  bitrate: %.1f kbit/s\n", c.Bitrate/1000)
	fmt.Fprintf(b, "  jitter: %.2f ms\n", c.JitterMS)
}

// reportListen writes the listen line, or nothing when the check never ran
// (Report.Listen's zero value). It never mentions the --wav output path:
// ListenResult carries no path field.
func reportListen(b *strings.Builder, r *Report) {
	switch {
	case r.Listen.Written:
		seconds := listenSeconds(r.Listen)
		fmt.Fprintln(b)
		fmt.Fprintf(b, "listen: wrote %.1fs of %d Hz %s s16 PCM\n",
			seconds, r.Listen.SampleRate, channelsLabel(r.Listen.Channels))
	case r.Listen.Skipped:
		fmt.Fprintln(b)
		fmt.Fprintf(b, "listen: skipped: %s\n", r.Listen.SkipReason)
	}
}

// endReasonPhrase maps an EndReason to the report's longer prose phrase. It is
// distinct from EndReason.String, which renders the walkthrough's short label.
func endReasonPhrase(r EndReason) string {
	switch r {
	case EndCompleted:
		return endReasonCompletedLabel
	case EndWatchdog:
		return "silence (read timeout)"
	case EndTeardown:
		return "server teardown"
	case EndDisconnect:
		return endReasonDisconnectLabel
	case EndCancelled:
		return endReasonCancelledLabel
	case EndTruncated:
		return "truncated (capture cap)"
	default:
		return unknownLabel
	}
}

// channelsLabel renders a channel count as "mono", "stereo", or "Nch". Shared
// by reportListen and renderListen.
func channelsLabel(n int) string {
	switch n {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	default:
		return strconv.Itoa(n) + "ch"
	}
}
