package doctor

import (
	"encoding/hex"
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
	stepRowFmt = "  %-11s%s%8s   %s\n" // name, status, elapsed, detail
	// captureInt's label column is wide enough for the longest capture label
	// ("wire bitrate", "sender clock") plus a separating space.
	captureInt = "  %-14s%d\n" // label, integer value
	// captureStr renders a capture row whose value is a preformatted string
	// (the last-frame age and sender-clock lines), sharing captureInt's label
	// column width.
	captureStr = "  %-14s%s\n" // label, string value
)

// senderClockTimeFormat renders a sender wall-clock time as a UTC ISO 8601
// timestamp with millisecond precision and a literal Z suffix.
const senderClockTimeFormat = "2006-01-02T15:04:05.000Z"

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
	case audiostream.CodecL16:
		return "L16"
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
// true for CodecAAC, CodecOpus, CodecG711, and CodecL16; false for
// CodecUnknown and any non-audio track.
func decodable(t rtsp.Track) bool {
	if t.Media != audiostream.MediaAudio {
		return false
	}
	switch t.Codec.(type) {
	case audiostream.CodecAAC, audiostream.CodecOpus, audiostream.CodecG711, audiostream.CodecL16:
		return true
	default:
		return false
	}
}

// ascHex returns the AAC AudioSpecificConfig as lowercase hex, or "" for a
// non-AAC codec or an AAC track whose ASC was absent. The ASC encodes the
// object type, sample rate, and channel configuration a maintainer needs to
// reproduce an AAC decode issue. Hex only, so it carries no PII.
func ascHex(c audiostream.Codec) string {
	aac, ok := c.(audiostream.CodecAAC)
	if !ok || len(aac.AudioSpecificConfig) == 0 {
		return ""
	}
	return hex.EncodeToString(aac.AudioSpecificConfig)
}

// writeTracksSection writes the "tracks" block: one labeled line per track and,
// under each audio track, the raw fmtp and the AAC ASC hex when present. Shared
// by the walkthrough and the report so both stay table-free and identical; both
// receive tracks whose camera-controlled strings (the raw fmtp and a
// CodecUnknown's rtpmap) were already scrubbed at the orchestration boundary.
func writeTracksSection(b *strings.Builder, tracks []rtsp.Track) {
	if len(tracks) == 0 {
		return
	}
	fmt.Fprintln(b)
	fmt.Fprintln(b, "tracks")
	for i := range tracks {
		t := tracks[i]
		// codecName can echo a CodecUnknown's raw rtpmap; like FMTP it is
		// scrubbed and made fence-safe at the describe boundary (doctor.go), so
		// it is rendered raw here.
		fmt.Fprintf(b, "  track %d: %s, %s, PT %s, clock %d, ch %s, depacketize %s\n",
			t.ID, t.Media.String(), codecName(t.Codec), payloadTypeCell(t.PayloadType), t.ClockRate,
			channelsCell(t.Channels), depacketizeCell(decodable(t)))
		if t.Media != audiostream.MediaAudio {
			continue
		}
		if t.FMTP != "" {
			fmt.Fprintf(b, "    fmtp: %s\n", t.FMTP)
		}
		if asc := ascHex(t.Codec); asc != "" {
			fmt.Fprintf(b, "    asc: %s\n", asc)
		}
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
	writeTracksSection(&b, r.Tracks)
	writeNoAudioSection(&b, &r)
	renderCapture(&b, &r)
	writeListenSection(&b, &r)
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

// writeNoAudioSection writes the no-audio-track notice when Describe succeeded
// but no audio track was present. Shared by the report and the walkthrough.
func writeNoAudioSection(b *strings.Builder, r *Report) {
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
	c := r.Capture
	fmt.Fprintf(b, "capture (%s, track %d, ended: %s)\n", r.Window, r.AudioTrack.ID, r.Reason)
	fmt.Fprintf(b, captureInt, "packets", c.Packets)
	fmt.Fprintf(b, captureInt, "bytes", c.Bytes)
	if c.WireBytes > 0 {
		fmt.Fprintf(b, captureInt, "wire bytes", c.WireBytes)
	}
	fmt.Fprintf(b, "  %-14s%d (%.2f%%)\n", "lost", c.Lost, c.LossRatio*100)
	fmt.Fprintf(b, captureInt, "duplicates", c.Duplicates)
	fmt.Fprintf(b, captureInt, "malformed", c.Malformed)
	fmt.Fprintf(b, captureInt, "ssrc-resets", c.SSRCResets)
	fmt.Fprintf(b, captureInt, "max gap", c.MaxGap)
	fmt.Fprintf(b, "  %-14s%.1f kbit/s\n", "bitrate", c.Bitrate/1000)
	if c.WireBytes > 0 {
		fmt.Fprintf(b, "  %-14s%.1f kbit/s\n", "wire bitrate", c.WireBitrate/1000)
	}
	fmt.Fprintf(b, "  %-14s%.2f ms\n", "jitter", c.JitterMS)
	fmt.Fprintf(b, captureStr, "last frame", lastFrameCell(&c))
	fmt.Fprintf(b, captureStr, "sender clock", senderClockCell(&c))
}

// lastFrameCell renders the last-frame value shared by both renderers:
// "0.4s ago" when a frame has arrived, "none" otherwise.
func lastFrameCell(c *CaptureStats) string {
	if !c.HaveLastFrame {
		return "none"
	}
	return fmt.Sprintf("%.1fs ago", c.LastFrameAge.Seconds())
}

// senderClockCell renders the sender-clock value shared by both renderers: the
// last frame's extrapolated sender wall clock and its offset from local time,
// or a "none" form when the sender clock is unusable. The two none forms are
// distinct: no RTCP sender report arrived at all, versus a report that arrived
// but whose track declared no clock rate, so WallClock cannot extrapolate a
// wall time (a zero SenderWall) and a year-one timestamp is suppressed.
func senderClockCell(c *CaptureStats) string {
	switch {
	case !c.SenderClock.Valid:
		return "none (no RTCP sender report)"
	case c.SenderWall.IsZero():
		return "none (sender report, no clock rate)"
	}
	return fmt.Sprintf("%s (offset %+.2fs)", c.SenderWall.UTC().Format(senderClockTimeFormat), c.SenderOffset.Seconds())
}

// listenSeconds returns the decoded audio duration for l in seconds, or 0
// when the sample rate is unknown. Used by writeListenSection so the
// walkthrough and the report always agree on a run's playback duration.
func listenSeconds(l ListenResult) float64 {
	if l.SampleRate <= 0 {
		return 0
	}
	return float64(l.Frames) / float64(l.SampleRate)
}

// writeListenSection writes the listen-check outcome line, or nothing when the
// check never ran (Report.Listen's zero value). Shared by the report and the
// walkthrough. The skip reason can carry stream-derived decoder error text (a
// go-aac/go-opus error over a camera-controlled ASC or channel count), so it is
// made fence-safe here rather than trusted.
func writeListenSection(b *strings.Builder, r *Report) {
	switch {
	case r.Listen.Written:
		seconds := listenSeconds(r.Listen)
		fmt.Fprintln(b)
		line := fmt.Sprintf("listen: wrote %.1fs of %d Hz %s s16 PCM", seconds, r.Listen.SampleRate, channelsLabel(r.Listen.Channels))
		if !r.Listen.SenderStart.IsZero() {
			line += ", sender clock start " + r.Listen.SenderStart.UTC().Format(senderClockTimeFormat)
		}
		fmt.Fprintln(b, line)
	case r.Listen.Skipped:
		fmt.Fprintln(b)
		fmt.Fprintf(b, "listen: skipped: %s\n", sanitizeLine(r.Listen.SkipReason))
	}
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

// payloadTypeCell renders the payload-type column: the numeric RTP payload
// type, or "-" for the -1 sentinel a media section that lists no format
// carries, matching how channelsCell renders an unknown channel count.
func payloadTypeCell(pt int) string {
	if pt < 0 {
		return "-"
	}
	return strconv.Itoa(pt)
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

// dialDetail summarizes the negotiated auth scheme, keepalive method, and the
// Server header when the camera reported one (already scrubbed at the
// orchestration boundary).
func dialDetail(si *rtsp.SessionInfo) string {
	d := fmt.Sprintf("auth %s, keepalive %s", si.AuthScheme, si.KeepaliveMethod)
	if si.Server != "" {
		d += ", server " + si.Server
	}
	return d
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
