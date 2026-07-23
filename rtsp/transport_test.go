package rtsp_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// Fixture strings reused across the transport tests and fuzz seeds,
// factored out to keep the linter's duplicate-string check quiet.
const (
	transportProtocol      = "RTP/AVP/TCP"
	interleavedTransport01 = "RTP/AVP/TCP;unicast;interleaved=0-1"
	methodOptions          = "OPTIONS"
	methodGetParameter     = "GET_PARAMETER"
)

func TestParseTransportInterleaved(t *testing.T) {
	t.Parallel()
	th, err := rtsp.ParseTransport(interleavedTransport01)
	if err != nil {
		t.Fatalf("ParseTransport error = %v", err)
	}
	if th.Protocol != transportProtocol {
		t.Errorf("Protocol = %q, want %q", th.Protocol, transportProtocol)
	}
	if !th.Unicast {
		t.Errorf("Unicast = false, want true")
	}
	if !th.Interleaved {
		t.Errorf("Interleaved = false, want true")
	}
	if th.RTPChannel != 0 || th.RTCPChannel != 1 {
		t.Errorf("channels = %d/%d, want 0/1", th.RTPChannel, th.RTCPChannel)
	}

	rtp, rtcp, err := th.InterleavedChannels(nil)
	if err != nil {
		t.Fatalf("InterleavedChannels error = %v", err)
	}
	if rtp != 0 || rtcp != 1 {
		t.Errorf("InterleavedChannels = %d/%d, want 0/1", rtp, rtcp)
	}
}

func TestParseTransportModeAndSSRC(t *testing.T) {
	t.Parallel()
	th, err := rtsp.ParseTransport(`RTP/AVP/TCP;unicast;interleaved=2-3;ssrc=1234ABCD;mode="PLAY"`)
	if err != nil {
		t.Fatalf("ParseTransport error = %v", err)
	}
	if th.RTPChannel != 2 || th.RTCPChannel != 3 {
		t.Errorf("channels = %d/%d, want 2/3", th.RTPChannel, th.RTCPChannel)
	}
	if th.SSRC != "1234ABCD" {
		t.Errorf("SSRC = %q, want 1234ABCD", th.SSRC)
	}
	if th.Mode != "PLAY" {
		t.Errorf("Mode = %q, want PLAY", th.Mode)
	}
}

func TestInterleavedChannelsServerRenumbered(t *testing.T) {
	t.Parallel()
	// Client proposed 0-1; server SETUP response renumbers to 4-5.
	th, err := rtsp.ParseTransport("RTP/AVP/TCP;unicast;interleaved=4-5")
	if err != nil {
		t.Fatalf("ParseTransport error = %v", err)
	}
	rtp, rtcp, err := th.InterleavedChannels(nil)
	if err != nil {
		t.Fatalf("InterleavedChannels error = %v", err)
	}
	if rtp != 4 || rtcp != 5 {
		t.Errorf("InterleavedChannels = %d/%d, want 4/5 (renumber accepted)", rtp, rtcp)
	}
}

func TestInterleavedChannelsBadPair(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value string
	}{
		{"non-consecutive descending", "RTP/AVP/TCP;unicast;interleaved=1-0"},
		{"non-consecutive gap", "RTP/AVP/TCP;unicast;interleaved=0-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			th, err := rtsp.ParseTransport(tt.value)
			if err != nil {
				t.Fatalf("ParseTransport(%q) error = %v", tt.value, err)
			}
			_, _, err = th.InterleavedChannels(nil)
			if !errors.Is(err, rtsp.ErrBadChannelPair) {
				t.Errorf("InterleavedChannels error = %v, want ErrBadChannelPair", err)
			}
		})
	}
}

func TestParseTransportUDPResponse(t *testing.T) {
	t.Parallel()
	th, err := rtsp.ParseTransport("RTP/AVP;unicast;client_port=5000-5001;server_port=6000-6001")
	if err != nil {
		t.Fatalf("ParseTransport error = %v", err)
	}
	if th.Interleaved {
		t.Errorf("Interleaved = true, want false for a UDP response")
	}
	_, _, err = th.InterleavedChannels(nil)
	if !errors.Is(err, rtsp.ErrNoInterleaved) {
		t.Errorf("InterleavedChannels error = %v, want ErrNoInterleaved", err)
	}
}

func TestInterleavedChannelsConflict(t *testing.T) {
	t.Parallel()
	th, err := rtsp.ParseTransport("RTP/AVP/TCP;unicast;interleaved=2-3")
	if err != nil {
		t.Fatalf("ParseTransport error = %v", err)
	}
	claimed := map[int]bool{0: true, 1: true, 2: true}
	_, _, err = th.InterleavedChannels(claimed)
	if !errors.Is(err, rtsp.ErrChannelConflict) {
		t.Errorf("InterleavedChannels error = %v, want ErrChannelConflict", err)
	}
}

func TestParseTransportEmptyValue(t *testing.T) {
	t.Parallel()
	_, err := rtsp.ParseTransport("")
	if !errors.Is(err, rtsp.ErrMalformedTransport) {
		t.Fatalf("ParseTransport(\"\") error = %v, want ErrMalformedTransport", err)
	}
}

func TestBuildTransportRoundTrip(t *testing.T) {
	t.Parallel()
	got := rtsp.BuildTransport(0, 1)
	want := interleavedTransport01
	if got != want {
		t.Fatalf("BuildTransport(0, 1) = %q, want %q", got, want)
	}

	th, err := rtsp.ParseTransport(got)
	if err != nil {
		t.Fatalf("ParseTransport(BuildTransport(...)) error = %v", err)
	}
	rtp, rtcp, err := th.InterleavedChannels(nil)
	if err != nil {
		t.Fatalf("InterleavedChannels error = %v", err)
	}
	if rtp != 0 || rtcp != 1 {
		t.Errorf("channels = %d/%d, want 0/1", rtp, rtcp)
	}
}

func TestParseSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		value     string
		wantID    string
		wantTOSec int
	}{
		{"bare ID default timeout", "12345678", "12345678", 60},
		{"explicit timeout", "12345678;timeout=60", "12345678", 60},
		{"different ID and timeout", "A9B8C7D6;timeout=30", "A9B8C7D6", 30},
		{"extra param ignored", "66334873;timeout=90;foo=bar", "66334873", 90},
		{"surrounding spaces trimmed", "  12345678  ", "12345678", 60},
		{"empty value", "", "", 60},
		// A non-positive or overflowing timeout must not reach the keepalive
		// timer, which would fire continuously (or panic) on one. Each falls
		// back to the default rather than being taken at face value.
		{"zero timeout falls back to default", "12345678;timeout=0", "12345678", 60},
		{"negative timeout falls back to default", "12345678;timeout=-30", "12345678", 60},
		{"overflowing timeout falls back to default", "12345678;timeout=9223372037", "12345678", 60},
		{"absurd timeout falls back to default", "12345678;timeout=99999999999999999999", "12345678", 60},
		{"non-numeric timeout falls back to default", "12345678;timeout=abc", "12345678", 60},
		{"largest representable timeout is accepted", "12345678;timeout=9223372036", "12345678", 9223372036},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := rtsp.ParseSession(tt.value)
			if got.ID != tt.wantID {
				t.Errorf("ParseSession(%q).ID = %q, want %q", tt.value, got.ID, tt.wantID)
			}
			wantTimeout := time.Duration(tt.wantTOSec) * time.Second
			if got.Timeout != wantTimeout {
				t.Errorf("ParseSession(%q).Timeout = %v, want %v", tt.value, got.Timeout, wantTimeout)
			}
		})
	}
}

func TestDefaultSessionTimeout(t *testing.T) {
	t.Parallel()
	if rtsp.DefaultSessionTimeout != 60*time.Second {
		t.Fatalf("DefaultSessionTimeout = %v, want 60s", rtsp.DefaultSessionTimeout)
	}
}

func TestParsePublicAndKeepaliveMethod(t *testing.T) {
	t.Parallel()

	withGetParam := "OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN, GET_PARAMETER"
	methods := rtsp.ParsePublic(withGetParam)
	want := []string{methodOptions, "DESCRIBE", "SETUP", "PLAY", "TEARDOWN", methodGetParameter}
	if len(methods) != len(want) {
		t.Fatalf("ParsePublic(%q) = %v, want %v", withGetParam, methods, want)
	}
	for i, m := range methods {
		if m != want[i] {
			t.Errorf("ParsePublic(%q)[%d] = %q, want %q", withGetParam, i, m, want[i])
		}
	}
	if got := rtsp.KeepaliveMethod(methods); got != methodGetParameter {
		t.Errorf("KeepaliveMethod(%v) = %q, want GET_PARAMETER", methods, got)
	}

	withoutGetParam := rtsp.ParsePublic("OPTIONS, DESCRIBE, SETUP, PLAY, TEARDOWN")
	if got := rtsp.KeepaliveMethod(withoutGetParam); got != methodOptions {
		t.Errorf("KeepaliveMethod(%v) = %q, want OPTIONS", withoutGetParam, got)
	}
}

func TestKeepaliveMethodLowercaseInput(t *testing.T) {
	t.Parallel()
	if got := rtsp.KeepaliveMethod([]string{"get_parameter"}); got != methodGetParameter {
		t.Fatalf("KeepaliveMethod([\"get_parameter\"]) = %q, want GET_PARAMETER", got)
	}
}
