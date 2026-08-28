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
	case EndStreamEnded:
		return "stream-end"
	default:
		return unknownLabel
	}
}

// codecName returns a short human label for a codec.
func codecName(c audiostream.Codec) string {
	switch v := c.(type) {
	case audiostream.CodecAAC:
		return "AAC"
	case audiostream.CodecMP4ALATM:
		return "AAC (MP4A-LATM)"
	case audiostream.CodecOpus:
		return "Opus"
	case audiostream.CodecMP3:
		return "MP3"
	case audiostream.CodecG711:
		if v.Law == audiostream.ALaw {
			return "PCMA (G.711 A-law)"
		}
		return "PCMU (G.711 mu-law)"
	case audiostream.CodecG726:
		// Name the AAL2 packing explicitly: the two forms are the same codec at
		// the same bit rate and differ only in codeword bit order, so a run that
		// sounds wrong is most often a packing mismatch, and the report should
		// say which one was used.
		if v.Packing == audiostream.G726PackingAAL2 {
			return "AAL2-G.726 " + v.BitRate.String() + " (ADPCM)"
		}
		return "G.726 " + v.BitRate.String() + " (ADPCM)"
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
// true for CodecAAC, CodecMP4ALATM (AAC access units), CodecOpus, CodecG711,
// CodecG726, and CodecL16; false for CodecUnknown and any non-audio track.
func decodable(t rtsp.Track) bool {
	if t.Media != audiostream.MediaAudio {
		return false
	}
	switch t.Codec.(type) {
	case audiostream.CodecAAC, audiostream.CodecMP4ALATM, audiostream.CodecOpus, audiostream.CodecG711, audiostream.CodecG726, audiostream.CodecL16:
		return true
	default:
		return false
	}
}

// ascHex returns the AAC AudioSpecificConfig as lowercase hex, or "" for a
// codec that carries no ASC or a track whose ASC was absent. Both CodecAAC and
// CodecMP4ALATM (AAC in MP4A-LATM) carry one; an in-band LATM track has none
// until its config is learned from a packet, so it renders "" at describe time.
// The ASC encodes the object type, sample rate, and channel configuration a
// maintainer needs to reproduce an AAC decode issue. Hex only, so it carries no
// PII.
func ascHex(c audiostream.Codec) string {
	var asc []byte
	switch v := c.(type) {
	case audiostream.CodecAAC:
		asc = v.AudioSpecificConfig
	case audiostream.CodecMP4ALATM:
		asc = v.AudioSpecificConfig
	}
	if len(asc) == 0 {
		return ""
	}
	return hex.EncodeToString(asc)
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
	renderHeaderTo(&b, &r, env)
	renderHandshake(&b, &r)
	renderTrailingTo(&b, &r)
	_, _ = io.WriteString(w, b.String())
}

// renderHeaderTo writes the two-line header (version/platform and target)
// followed by a blank line. Split out so the live path can stream it up front,
// before any step has run, while the batch path composes it inline.
func renderHeaderTo(b *strings.Builder, r *Report, env Env) {
	fmt.Fprintf(b, "stream-doctor %s (%s/%s)\n", env.Version, env.OS, env.Arch)
	fmt.Fprintf(b, "target: %s\n", r.RedactedURL)
	fmt.Fprintln(b)
}

// renderTrailingTo writes everything after the handshake block: the tracks
// summary, the no-audio notice, the capture statistics, and the listen result.
// Split out so the live path can emit it once at the end, after the step rows
// have already streamed.
func renderTrailingTo(b *strings.Builder, r *Report) {
	writeTracksSection(b, r.Tracks)
	renderIdentity(b, &r.Session)
	writeNoAudioSection(b, r)
	renderCapture(b, r)
	writeListenSection(b, r)
}

// renderIdentity writes a best-effort camera-identity block from the SDP the
// DESCRIBE returned: the session name (s=) and the tool (a=tool:), the latter
// often naming the streaming stack and version. Each line appears only when the
// stream advertised it, and the whole block is omitted when it advertised
// neither, so a terse camera adds no noise. Both values are server-controlled
// and were scrubbed at the orchestration boundary. The RTSP Server header is
// not repeated here; it already shows in the DIAL detail.
func renderIdentity(b *strings.Builder, si *rtsp.SessionInfo) {
	if si.SDPSessionName == "" && si.SDPTool == "" {
		return
	}
	fmt.Fprintln(b)
	fmt.Fprintln(b, "identity")
	if si.SDPSessionName != "" {
		fmt.Fprintf(b, "  %-10s%s\n", "sdp name", si.SDPSessionName)
	}
	if si.SDPTool != "" {
		fmt.Fprintf(b, "  %-10s%s\n", "sdp tool", si.SDPTool)
	}
}

// renderHandshake writes the "handshake" block. A successful CAPTURE step is
// omitted here because its detail is the dedicated capture block; a failed one
// is surfaced as a step line.
func renderHandshake(b *strings.Builder, r *Report) {
	fmt.Fprintln(b, "handshake")
	for i := range r.Steps {
		s := r.Steps[i]
		if stepHidden(&s) {
			continue
		}
		renderStepRow(b, &s)
	}
}

// stepHidden reports whether a step is omitted from the handshake block: a
// successful CAPTURE step, whose detail is the dedicated capture block. It is
// the single source of truth for the skip rule, shared by the batch renderer
// and the live per-step streamer so they agree on which rows appear.
func stepHidden(s *HandshakeStep) bool {
	return s.Name == stepCapture && s.OK
}

// renderStepRow writes one handshake step row and, when present, its hint
// continuation line. Shared by the batch renderer and the live streamer so a
// row looks identical whichever path emits it.
func renderStepRow(b *strings.Builder, s *HandshakeStep) {
	fmt.Fprintf(b, stepRowFmt, s.Name, stepStatus(s.OK), formatElapsed(s.Elapsed), s.Detail)
	if s.Hint != "" {
		fmt.Fprintf(b, "%shint: %s\n", hintIndent, s.Hint)
	}
}

// hintIndent aligns a hint continuation line under the detail column of a
// failed handshake row: 2 leading spaces + the 11-wide name + "FAIL" (4) +
// the 8-wide elapsed + 3 spaces = 28 columns.
const hintIndent = "                            " // 28 spaces

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
	// The sender clock is an RTCP construct; an HTTP progressive source has no
	// sender report, so the line is omitted rather than always reading "none".
	if r.Kind != SourceHTTP {
		fmt.Fprintf(b, captureStr, "sender clock", senderClockCell(&c))
	}
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

// openDetail summarizes the HTTP OPEN step and, when the server sent one, the
// (already scrubbed) Server header. A PCM source (WAV or raw/L16) resolves to a
// concrete s16le shape, so the detail reads "s16le <rate> Hz <layout>". A
// compressed source (MP3) is framed but not decoded, so its true rate and
// channel count come from the consumer's decoder, not the transport: the detail
// names the codec instead of asserting a PCM shape it does not have.
func openDetail(track rtsp.Track, server string) string {
	var d string
	if audiostream.PayloadKindFor(track.Codec) == audiostream.KindPCMS16LE {
		d = fmt.Sprintf("s16le %d Hz %s", track.ClockRate, channelsLabel(track.Channels))
	} else {
		d = codecName(track.Codec) + " (compressed, geometry from decoder)"
	}
	if server != "" {
		d += ", server " + server
	}
	return d
}

// describeDetail counts the discovered tracks by media kind and reports the
// login outcome. The 401 challenge is answered inside DESCRIBE, so this is the
// step that makes authentication visible: a scheme was negotiated ("Digest auth
// OK") or the stream was open ("no auth required"). auth is the scheme snapshot
// taken right after Describe returned.
func describeDetail(tracks []rtsp.Track, auth rtsp.AuthScheme) string {
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
	base := "no tracks"
	if len(parts) > 0 {
		base = strings.Join(parts, ", ")
	}
	return base + ", " + authOutcome(auth)
}

// authOutcome renders the login result for the describe detail: the negotiated
// scheme when the stream required authentication, or a plain note that it did
// not. AuthNone is the zero value, so an anonymous stream reads "no auth
// required".
func authOutcome(auth rtsp.AuthScheme) string {
	if auth == rtsp.AuthNone {
		return "no auth required"
	}
	return fmt.Sprintf("%s auth OK", auth)
}

// setupDetail summarizes the target track, its negotiated channels, and any
// discarded tracks.
func setupDetail(si *rtsp.SessionInfo, audio rtsp.Track, discarded int) string {
	detail := fmt.Sprintf("track %d, %s", audio.ID, transportStr(si, audio.ID))
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

// transportStr renders the negotiated media endpoints for trackID in the
// walkthrough's setup line: the UDP port pairs over a UDP session, otherwise
// the interleaved channel pair.
func transportStr(si *rtsp.SessionInfo, trackID int) string {
	if si.IsUDP() {
		return "udp " + udpEndpointStr(si, trackID)
	}
	return channelStr(si, trackID)
}

// reportTransport renders the report session block's transport line: the UDP
// label and port pairs over a UDP session, otherwise the interleaved label and
// channel pair. The TCP branch reproduces the previous hard-coded line exactly.
func reportTransport(si *rtsp.SessionInfo, trackID int) string {
	if si.IsUDP() {
		return "UDP unicast, " + udpEndpointStr(si, trackID)
	}
	return "TCP interleaved, " + channelStr(si, trackID)
}

// udpEndpointStr renders the negotiated UDP client and server port pairs for
// trackID, without a transport label; "ports n/a" when no endpoint matches.
func udpEndpointStr(si *rtsp.SessionInfo, trackID int) string {
	for _, e := range si.UDPEndpoints {
		if e.TrackID == trackID {
			return fmt.Sprintf("client_port %d-%d, server_port %d-%d",
				e.ClientRTPPort, e.ClientRTCPPort, e.ServerRTPPort, e.ServerRTCPPort)
		}
	}
	return "ports n/a"
}

// pluralSuffix returns "" for a count of one, "s" otherwise.
func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
