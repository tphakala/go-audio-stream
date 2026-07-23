package doctor

import (
	"fmt"
	"strconv"
	"strings"
)

// renderReport returns the paste-ready markdown report for r. All values
// come from r and env (injected), so the output is deterministic. The URL
// is already redacted in r.RedactedURL; no credentials and no local file
// paths appear (ListenResult carries no path field, so a written WAV can
// never surface one here).
//
//nolint:gocritic // hugeParam: Report/Env match renderWalkthrough; called once per run.
func renderReport(r Report, env Env) string {
	blocks := []string{reportHeader(&r, env)}
	if b := reportHandshake(&r); b != "" {
		blocks = append(blocks, b)
	}
	if b := reportSessionDetails(&r); b != "" {
		blocks = append(blocks, b)
	}
	if b := reportTracks(&r); b != "" {
		blocks = append(blocks, b)
	}
	if b := reportCapture(&r); b != "" {
		blocks = append(blocks, b)
	}
	if b := reportListen(&r); b != "" {
		blocks = append(blocks, b)
	}
	return strings.Join(blocks, "\n\n") + "\n"
}

// reportHeader renders the title and the Target/Result/Tool summary lines.
func reportHeader(r *Report, env Env) string {
	var b strings.Builder
	fmt.Fprintln(&b, "### stream-doctor report")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "**Target:** `%s`\n", r.RedactedURL)
	fmt.Fprintf(&b, "**Result:** %s\n", r.Result)
	fmt.Fprintf(&b, "**Tool:** go-audio-stream/stream-doctor %s (%s/%s)", env.Version, env.OS, env.Arch)
	return b.String()
}

// reportHandshake renders the handshake step table, or "" when there is
// nothing to show. A successful CAPTURE step is omitted, matching
// renderWalkthrough: its detail is the dedicated Capture block.
func reportHandshake(r *Report) string {
	rows := make([]HandshakeStep, 0, len(r.Steps))
	for i := range r.Steps {
		s := r.Steps[i]
		if s.Name == stepCapture && s.OK {
			continue
		}
		rows = append(rows, s)
	}
	if len(rows) == 0 {
		return ""
	}

	var b strings.Builder
	fmt.Fprintln(&b, "**Handshake**")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Step | Status | Time |")
	fmt.Fprintln(&b, "| --- | --- | --- |")
	for i, s := range rows {
		if i == len(rows)-1 {
			fmt.Fprintf(&b, "| %s | %s | %s |", s.Name, stepStatus(s.OK), formatElapsed(s.Elapsed))
		} else {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", s.Name, stepStatus(s.OK), formatElapsed(s.Elapsed))
		}
	}
	return b.String()
}

// reportSessionDetails renders the auth/session/keepalive/transport bullet
// list, shown once DIAL has negotiated a session; "" before that.
func reportSessionDetails(r *Report) string {
	if !hasStepOK(r.Steps, stepDial) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "- Auth: %s\n", r.Session.AuthScheme)
	fmt.Fprintf(&b, "- Session timeout: %ds\n", int(r.Session.SessionTimeout.Seconds()))
	fmt.Fprintf(&b, "- Keepalive: %s\n", r.Session.KeepaliveMethod)
	fmt.Fprintf(&b, "- Transport: TCP interleaved, %s", channelStr(&r.Session, r.AudioTrack.ID))
	return b.String()
}

// reportTracks renders the SDP track summary table, or "" when no tracks
// were discovered.
func reportTracks(r *Report) string {
	if len(r.Tracks) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintln(&b, "**Tracks**")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| # | Kind | Codec | Clock | Ch | Depacketize |")
	fmt.Fprintln(&b, "| --- | --- | --- | --- | --- | --- |")
	for i := range r.Tracks {
		t := r.Tracks[i]
		row := fmt.Sprintf("| %s | %s | %s | %s | %s | %s |",
			strconv.Itoa(t.ID), t.Media.String(), codecName(t.Codec), strconv.Itoa(t.ClockRate),
			channelsCell(t.Channels), depacketizeCell(decodable(t)))
		if i == len(r.Tracks)-1 {
			fmt.Fprint(&b, row)
		} else {
			fmt.Fprintln(&b, row)
		}
	}
	return b.String()
}

// reportCapture renders the capture statistics block, or "" when capture
// never ran.
func reportCapture(r *Report) string {
	if !r.CaptureShown {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Capture (%s, track %d, ended: %s)**\n", r.Window, r.AudioTrack.ID, endReasonPhrase(r.Reason))
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "| Metric | Value |")
	fmt.Fprintln(&b, "| --- | --- |")
	fmt.Fprintf(&b, "| Packets | %d |\n", r.Capture.Packets)
	fmt.Fprintf(&b, "| Bytes | %d |\n", r.Capture.Bytes)
	fmt.Fprintf(&b, "| Lost | %d (%.2f%%) |\n", r.Capture.Lost, r.Capture.LossRatio*100)
	fmt.Fprintf(&b, "| Max gap | %d |\n", r.Capture.MaxGap)
	fmt.Fprintf(&b, "| Bitrate | %.1f kbit/s |\n", r.Capture.Bitrate/1000)
	fmt.Fprintf(&b, "| Jitter | %.2f ms |", r.Capture.JitterMS)
	return b.String()
}

// reportListen renders the Listen line, or "" when the check never ran
// (Report.Listen's zero value). It never mentions the --wav output path:
// ListenResult carries no path field.
func reportListen(r *Report) string {
	switch {
	case r.Listen.Written:
		seconds := 0.0
		if r.Listen.SampleRate > 0 {
			seconds = float64(r.Listen.Frames) / float64(r.Listen.SampleRate)
		}
		return fmt.Sprintf("**Listen:** wrote %.1fs of %d Hz %s s16 PCM",
			seconds, r.Listen.SampleRate, channelsLabel(r.Listen.Channels))
	case r.Listen.Skipped:
		return fmt.Sprintf("**Listen:** skipped: %s", r.Listen.SkipReason)
	default:
		return ""
	}
}

// endReasonPhrase maps an EndReason to the report's longer prose phrase.
// It is distinct from EndReason.String, which renders the walkthrough's
// short label.
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

// channelsLabel renders a channel count as "mono", "stereo", or "Nch".
func channelsLabel(n int) string {
	switch n {
	case 1:
		return "mono"
	case 2:
		return "stereo"
	default:
		return fmt.Sprintf("%dch", n)
	}
}
