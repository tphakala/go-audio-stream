package doctor

import (
	"strings"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

func TestCodecName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		codec audiostream.Codec
		want  string
	}{
		{"aac", audiostream.CodecAAC{}, "AAC"},
		{"mp4a-latm", audiostream.CodecMP4ALATM{}, "AAC (MP4A-LATM)"},
		{"opus", audiostream.CodecOpus{}, "Opus"},
		{"mp3", audiostream.CodecMP3{}, "MP3"},
		{"g711 mu-law", audiostream.CodecG711{Law: audiostream.MuLaw}, "PCMU (G.711 mu-law)"},
		{"g711 a-law", audiostream.CodecG711{Law: audiostream.ALaw}, "PCMA (G.711 A-law)"},
		{"l16", audiostream.CodecL16{ClockRate: 8000, Channels: 1}, "L16"},
		{"unknown with rtpmap", audiostream.CodecUnknown{RTPMap: testH264}, testH264},
		{"unknown empty", audiostream.CodecUnknown{}, "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := codecName(tc.codec); got != tc.want {
				t.Errorf("codecName(%T) = %q, want %q", tc.codec, got, tc.want)
			}
		})
	}
}

func TestOpenDetail(t *testing.T) {
	t.Parallel()
	// A PCM source renders a concrete s16le shape.
	pcm := openDetail(rtsp.Track{Codec: audiostream.CodecL16{ClockRate: 8000, Channels: 1}, ClockRate: 8000, Channels: 1}, "")
	if !strings.Contains(pcm, "s16le") || !strings.Contains(pcm, "8000 Hz") {
		t.Errorf("PCM openDetail = %q, want an s16le 8000 Hz shape", pcm)
	}
	// A compressed source names the codec and does not assert a PCM shape (the
	// synthesized MP3 track has ClockRate 0, which must not surface as "0 Hz").
	mp3 := openDetail(rtsp.Track{Codec: audiostream.CodecMP3{}, ClockRate: 0, Channels: 0}, "")
	if strings.Contains(mp3, "s16le") || strings.Contains(mp3, "0 Hz") {
		t.Errorf("compressed openDetail = %q, must not claim a PCM shape", mp3)
	}
	if !strings.Contains(mp3, "MP3") {
		t.Errorf("compressed openDetail = %q, want the codec name", mp3)
	}
}

func TestDecodable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		track rtsp.Track
		want  bool
	}{
		{"aac audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecAAC{}}, true},
		{"mp4a-latm audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecMP4ALATM{}}, true},
		{"opus audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecOpus{}}, true},
		{"g711 audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecG711{}}, true},
		{"l16 audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecL16{}}, true},
		{"unknown audio", rtsp.Track{Media: audiostream.MediaAudio, Codec: audiostream.CodecUnknown{RTPMap: testUnknownRTPMap}}, false},
		{"video track", rtsp.Track{Media: audiostream.MediaVideo, Codec: audiostream.CodecUnknown{RTPMap: testH264}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decodable(tc.track); got != tc.want {
				t.Errorf("decodable(%+v) = %v, want %v", tc.track, got, tc.want)
			}
		})
	}
}

func TestASCHex(t *testing.T) {
	t.Parallel()
	asc := []byte{0x12, 0x10}
	cases := []struct {
		name  string
		codec audiostream.Codec
		want  string
	}{
		{"aac with asc", audiostream.CodecAAC{AudioSpecificConfig: asc}, "1210"},
		{"latm with asc", audiostream.CodecMP4ALATM{AudioSpecificConfig: asc}, "1210"},
		{"aac without asc", audiostream.CodecAAC{}, ""},
		{"in-band latm without asc", audiostream.CodecMP4ALATM{MuxConfigPresent: true}, ""},
		{"non-aac codec carries no asc", audiostream.CodecOpus{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ascHex(tc.codec); got != tc.want {
				t.Errorf("ascHex(%T) = %q, want %q", tc.codec, got, tc.want)
			}
		})
	}
}

// udpTransportValue is the library's documented SessionInfo.Transport value
// for a UDP-pinned session, shared by the fixtures below (goconst wants a
// literal repeated this often pulled into a constant).
const udpTransportValue = "UDP"

// TestTransportStr covers the walkthrough setup-line detail: a TCP session
// renders the interleaved channel pair (unchanged from before UDP), a UDP
// session renders its negotiated port pairs, and a UDP session missing an
// endpoint for the track renders the n/a marker.
func TestTransportStr(t *testing.T) {
	t.Parallel()
	tcp := &rtsp.SessionInfo{
		Transport: "TCP",
		Channels:  []rtsp.ChannelPair{{TrackID: 0, RTP: 0, RTCP: 1}},
	}
	udp := &rtsp.SessionInfo{
		Transport: udpTransportValue,
		UDPEndpoints: []rtsp.UDPEndpoint{
			{TrackID: 0, ClientRTPPort: 40000, ClientRTCPPort: 40001, ServerRTPPort: 6970, ServerRTCPPort: 6971},
		},
	}
	if got, want := transportStr(tcp, 0), "channels 0-1"; got != want {
		t.Errorf("transportStr(tcp) = %q, want %q", got, want)
	}
	if got, want := transportStr(udp, 0), "udp client_port 40000-40001, server_port 6970-6971"; got != want {
		t.Errorf("transportStr(udp) = %q, want %q", got, want)
	}
	if got, want := transportStr(udp, 7), "udp ports n/a"; got != want {
		t.Errorf("transportStr(udp, missing track) = %q, want %q", got, want)
	}
}

// TestReportTransport covers the report session block's transport line. The TCP
// branch reproduces the previous hard-coded line byte for byte, so existing
// golden output is unchanged.
func TestReportTransport(t *testing.T) {
	t.Parallel()
	tcp := &rtsp.SessionInfo{
		Transport: "TCP",
		Channels:  []rtsp.ChannelPair{{TrackID: 0, RTP: 0, RTCP: 1}},
	}
	udp := &rtsp.SessionInfo{
		Transport: udpTransportValue,
		UDPEndpoints: []rtsp.UDPEndpoint{
			{TrackID: 0, ClientRTPPort: 40000, ClientRTCPPort: 40001, ServerRTPPort: 6970, ServerRTCPPort: 6971},
		},
	}
	if got, want := reportTransport(tcp, 0), "TCP interleaved, channels 0-1"; got != want {
		t.Errorf("reportTransport(tcp) = %q, want %q", got, want)
	}
	if got, want := reportTransport(udp, 0), "UDP unicast, client_port 40000-40001, server_port 6970-6971"; got != want {
		t.Errorf("reportTransport(udp) = %q, want %q", got, want)
	}
}

// TestRenderWalkthroughColumns covers the two column-rendering edge cases: a
// track with zero channels renders "-", and a failed early step omits every
// later step.
func TestRenderWalkthroughColumns(t *testing.T) {
	t.Parallel()
	env := Env{OS: "linux", Arch: "amd64", Version: "0.1.0"}

	t.Run("zero channels render dash", func(t *testing.T) {
		t.Parallel()
		r := Report{
			RedactedURL: redactedStreamURL,
			HaveAudio:   true,
			Tracks: []rtsp.Track{
				{ID: 2, Media: audiostream.MediaAudio, Codec: audiostream.CodecOpus{}, ClockRate: 48000, Channels: 0},
			},
		}
		var b strings.Builder
		renderWalkthrough(&b, r, env)
		got := b.String()
		if !strings.Contains(got, "clock 48000, ch -, depacketize yes") {
			t.Errorf("zero-channel row missing dash cell:\n%s", got)
		}
	})

	t.Run("unknown payload type renders dash", func(t *testing.T) {
		t.Parallel()
		r := Report{
			RedactedURL: redactedStreamURL,
			HaveAudio:   true,
			Tracks: []rtsp.Track{
				{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecOpus{}, PayloadType: -1, ClockRate: 48000, Channels: 2},
			},
		}
		var b strings.Builder
		renderWalkthrough(&b, r, env)
		if got := b.String(); !strings.Contains(got, "Opus, PT -, clock") {
			t.Errorf("unknown payload type (-1) should render as a dash:\n%s", got)
		}
	})

	t.Run("failed step omits later steps", func(t *testing.T) {
		t.Parallel()
		r := Report{
			RedactedURL: redactedStreamURL,
			Steps: []HandshakeStep{
				{Name: "DIAL", OK: false, Detail: "connection refused"},
			},
		}
		var b strings.Builder
		renderWalkthrough(&b, r, env)
		got := b.String()
		if !strings.Contains(got, "DIAL") || !strings.Contains(got, "FAIL") {
			t.Errorf("expected DIAL FAIL line, got:\n%s", got)
		}
		for _, later := range []string{"DESCRIBE", "SETUP", "PLAY", "tracks", "capture"} {
			if strings.Contains(got, later) {
				t.Errorf("expected %q to be omitted after a failed DIAL, got:\n%s", later, got)
			}
		}
	})
}

// telemetryReport returns a Report with a shown capture block and the given
// CaptureStats, used to exercise the telemetry lines through the walkthrough.
func telemetryReport(c *CaptureStats) Report {
	return Report{
		RedactedURL:  redactedStreamURL,
		HaveAudio:    true,
		CaptureShown: true,
		Window:       10 * time.Second,
		Reason:       EndCompleted,
		AudioTrack:   rtsp.Track{ID: 0, Media: audiostream.MediaAudio, Codec: audiostream.CodecOpus{}},
		Capture:      *c,
	}
}

// TestRenderCaptureTelemetryPresent asserts the walkthrough renders the wire,
// last-frame, and sender-clock lines with their widened %-14s label column
// when the values are present.
func TestRenderCaptureTelemetryPresent(t *testing.T) {
	t.Parallel()
	r := telemetryReport(&CaptureStats{
		WireBytes: 70000, WireBitrate: 56000,
		HaveLastFrame: true, LastFrameAge: 400 * time.Millisecond,
		SenderClock:  audiostream.SenderClock{Valid: true},
		SenderWall:   time.Date(2026, 8, 4, 9, 12, 33, 512_000_000, time.UTC),
		SenderOffset: 120 * time.Millisecond,
	})
	var b strings.Builder
	renderWalkthrough(&b, r, testEnv())
	got := b.String()
	for _, want := range []string{
		"  wire bytes    70000\n",
		"  wire bitrate  56.0 kbit/s\n",
		"  last frame    0.4s ago\n",
		"  sender clock  2026-08-04T09:12:33.512Z (offset +0.12s)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("walkthrough missing %q:\n%s", want, got)
		}
	}
}

// TestRenderCaptureTelemetryAbsent asserts the wire lines are omitted when
// WireBytes is zero and that the last-frame and sender-clock lines render
// their "none" forms.
func TestRenderCaptureTelemetryAbsent(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	renderWalkthrough(&b, telemetryReport(&CaptureStats{}), testEnv())
	got := b.String()
	if strings.Contains(got, "wire bytes") || strings.Contains(got, "wire bitrate") {
		t.Errorf("wire lines must be omitted when WireBytes is zero:\n%s", got)
	}
	if !strings.Contains(got, "  last frame    none\n") {
		t.Errorf("walkthrough missing the last-frame none form:\n%s", got)
	}
	if !strings.Contains(got, "  sender clock  none (no RTCP sender report)\n") {
		t.Errorf("walkthrough missing the sender-clock none form:\n%s", got)
	}
}

// TestRenderCaptureSenderClockNoRate covers the degenerate case of a valid
// sender report on a track that declared no clock rate: WallClock cannot
// extrapolate, so SenderWall is the zero Time and the line must render the
// distinct "no clock rate" none form rather than a year-one timestamp.
func TestRenderCaptureSenderClockNoRate(t *testing.T) {
	t.Parallel()
	r := telemetryReport(&CaptureStats{SenderClock: audiostream.SenderClock{Valid: true}})
	var b strings.Builder
	renderWalkthrough(&b, r, testEnv())
	got := b.String()
	if !strings.Contains(got, "  sender clock  none (sender report, no clock rate)\n") {
		t.Errorf("a valid clock with a zero wall time must render the no-clock-rate none form:\n%s", got)
	}
	if strings.Contains(got, "0001-01-01") {
		t.Errorf("walkthrough rendered a year-one sender-clock timestamp:\n%s", got)
	}
}
