package doctor

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// Fixed column format strings for the walkthrough. Widths are constants so the
// golden output is byte-stable.
const (
	stepRowFmt  = "  %-11s%s%8s   %s\n"            // name, status, elapsed, detail
	trackRowFmt = "  %-3s%-7s%-21s%5s   %2s  %s\n" // id, kind, codec, clock, ch, depacketize
	captureInt  = "  %-10s%d\n"                    // label, integer value
)

// unknownLabel is the codec/reason fallback label.
const unknownLabel = "unknown"

// End-reason labels shared verbatim between the walkthrough's short String
// and the report's longer endReasonPhrase: these three reasons need no
// longer prose form, so both renderers use the same word.
const (
	endReasonCompletedLabel  = "completed"
	endReasonDisconnectLabel = "disconnect"
	endReasonCancelledLabel  = "cancelled"
)

// String returns a short lowercase name for the end reason.
func (r EndReason) String() string {
	switch r {
	case EndCompleted:
		return endReasonCompletedLabel
	case EndWatchdog:
		return "watchdog"
	case EndTeardown:
		return "teardown"
	case EndDisconnect:
		return endReasonDisconnectLabel
	case EndCancelled:
		return endReasonCancelledLabel
	case EndTruncated:
		return "truncated"
	default:
		return unknownLabel
	}
}

// codecName returns a short human label for a codec.
func codecName(c audiostream.Codec) string {
	switch v := c.(type) {
	case audiostream.CodecAAC:
		return "AAC"
	case audiostream.CodecOpus:
		return "Opus"
	case audiostream.CodecG711:
		if v.Law == audiostream.ALaw {
			return "PCMA (G.711 A-law)"
		}
		return "PCMU (G.711 mu-law)"
	case audiostream.CodecUnknown:
		if v.RTPMap == "" {
			return unknownLabel
		}
		return v.RTPMap
	default:
		return unknownLabel
	}
}

// decodable reports whether the doctor can turn a track's audio into a WAV:
// true for CodecAAC, CodecOpus, and CodecG711; false for CodecUnknown and any
// non-audio track.
func decodable(t rtsp.Track) bool {
	if t.Media != audiostream.MediaAudio {
		return false
	}
	switch t.Codec.(type) {
	case audiostream.CodecAAC, audiostream.CodecOpus, audiostream.CodecG711:
		return true
	default:
		return false
	}
}

// renderWalkthrough writes the human-readable handshake walkthrough, SDP
// summary, and capture summary for r to w. Plain ASCII, no color.
//
//nolint:gocritic // hugeParam: Report/Env are the documented rendering signature, called once per run.
func renderWalkthrough(w io.Writer, r Report, env Env) {
	var b strings.Builder
	fmt.Fprintf(&b, "stream-doctor %s (%s/%s)\n", env.Version, env.OS, env.Arch)
	fmt.Fprintf(&b, "target: %s\n", r.RedactedURL)
	fmt.Fprintln(&b)
	renderHandshake(&b, &r)
	renderTracks(&b, &r)
	renderNoAudio(&b, &r)
	renderCapture(&b, &r)
	_, _ = io.WriteString(w, b.String())
}

// renderHandshake writes the "handshake" block. A successful CAPTURE step is
// omitted here because its detail is the dedicated capture block; a failed one
// is surfaced as a step line.
func renderHandshake(b *strings.Builder, r *Report) {
	fmt.Fprintln(b, "handshake")
	for i := range r.Steps {
		s := r.Steps[i]
		if s.Name == stepCapture && s.OK {
			continue
		}
		fmt.Fprintf(b, stepRowFmt, s.Name, stepStatus(s.OK), formatElapsed(s.Elapsed), s.Detail)
	}
}

// renderTracks writes the SDP track summary table, or nothing when no tracks
// were discovered.
func renderTracks(b *strings.Builder, r *Report) {
	if len(r.Tracks) == 0 {
		return
	}
	fmt.Fprintln(b)
	fmt.Fprintln(b, "tracks")
	fmt.Fprintf(b, trackRowFmt, "#", "kind", "codec", "clock", "ch", "depacketize")
	for i := range r.Tracks {
		t := r.Tracks[i]
		fmt.Fprintf(b, trackRowFmt,
			strconv.Itoa(t.ID),
			t.Media.String(),
			codecName(t.Codec),
			strconv.Itoa(t.ClockRate),
			channelsCell(t.Channels),
			depacketizeCell(decodable(t)),
		)
	}
}

// renderNoAudio writes the no-audio-track notice when Describe succeeded but no
// audio track was present.
func renderNoAudio(b *strings.Builder, r *Report) {
	if r.HaveAudio || !hasStepOK(r.Steps, stepDescribe) {
		return
	}
	fmt.Fprintln(b)
	fmt.Fprintln(b, "no audio track found")
}

// renderCapture writes the capture statistics block, or nothing when capture
// never ran.
func renderCapture(b *strings.Builder, r *Report) {
	if !r.CaptureShown {
		return
	}
	fmt.Fprintln(b)
	fmt.Fprintf(b, "capture (%s, track %d, ended: %s)\n", r.Window, r.AudioTrack.ID, r.Reason)
	fmt.Fprintf(b, captureInt, "packets", r.Capture.Packets)
	fmt.Fprintf(b, captureInt, "bytes", r.Capture.Bytes)
	fmt.Fprintf(b, "  %-10s%d (%.2f%%)\n", "lost", r.Capture.Lost, r.Capture.LossRatio*100)
	fmt.Fprintf(b, captureInt, "max gap", r.Capture.MaxGap)
	fmt.Fprintf(b, "  %-10s%.1f kbit/s\n", "bitrate", r.Capture.Bitrate/1000)
	fmt.Fprintf(b, "  %-10s%.2f ms\n", "jitter", r.Capture.JitterMS)
}

// stepStatus renders the ok/FAIL column for a handshake step.
func stepStatus(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAIL"
}

// formatElapsed renders a step duration: whole milliseconds under a second,
// otherwise the standard duration string.
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return d.String()
}

// channelsCell renders a track's channel count, "-" when zero (absent).
func channelsCell(n int) string {
	if n == 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

// depacketizeCell renders the depacketize column from decodability.
func depacketizeCell(ok bool) string {
	if ok {
		return "yes"
	}
	return "no"
}

// hasStepOK reports whether a step with the given name completed successfully.
func hasStepOK(steps []HandshakeStep, name string) bool {
	for i := range steps {
		if steps[i].Name == name && steps[i].OK {
			return true
		}
	}
	return false
}

// dialDetail summarizes the negotiated auth scheme and keepalive method.
func dialDetail(si *rtsp.SessionInfo) string {
	return fmt.Sprintf("auth %s, keepalive %s", si.AuthScheme, si.KeepaliveMethod)
}

// describeDetail counts the discovered tracks by media kind.
func describeDetail(tracks []rtsp.Track) string {
	var audio, video, other int
	for i := range tracks {
		switch tracks[i].Media {
		case audiostream.MediaAudio:
			audio++
		case audiostream.MediaVideo:
			video++
		default:
			other++
		}
	}
	parts := make([]string, 0, 3)
	if audio > 0 {
		parts = append(parts, fmt.Sprintf("%d audio track%s", audio, pluralSuffix(audio)))
	}
	if video > 0 {
		parts = append(parts, fmt.Sprintf("%d video track%s", video, pluralSuffix(video)))
	}
	if other > 0 {
		parts = append(parts, fmt.Sprintf("%d other track%s", other, pluralSuffix(other)))
	}
	if len(parts) == 0 {
		return "no tracks"
	}
	return strings.Join(parts, ", ")
}

// setupDetail summarizes the target track, its negotiated channels, and any
// discarded tracks.
func setupDetail(si *rtsp.SessionInfo, audio rtsp.Track, discarded int) string {
	detail := fmt.Sprintf("track %d, %s", audio.ID, channelStr(si, audio.ID))
	if discarded > 0 {
		detail += fmt.Sprintf(", %d track%s discarded", discarded, pluralSuffix(discarded))
	}
	return detail
}

// playDetail renders the negotiated session timeout in whole seconds.
func playDetail(si *rtsp.SessionInfo) string {
	return fmt.Sprintf("session timeout %ds", int(si.SessionTimeout.Seconds()))
}

// channelStr renders the interleaved channel pair assigned to trackID.
func channelStr(si *rtsp.SessionInfo, trackID int) string {
	for _, c := range si.Channels {
		if c.TrackID == trackID {
			return fmt.Sprintf("channels %d-%d", c.RTP, c.RTCP)
		}
	}
	return "channels n/a"
}

// pluralSuffix returns "" for a count of one, "s" otherwise.
func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// failReason renders a step's failure detail from its terminal error.
func failReason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
