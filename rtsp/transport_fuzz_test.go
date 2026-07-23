package rtsp_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// transportSeeds are the fixtures exercising ParseTransport's branches:
// interleaved pairs (valid, renumbered, non-consecutive), mode/ssrc, a UDP
// response with no interleaved pair, and the malformed-empty case.
var transportSeeds = []string{
	interleavedTransport01,
	`RTP/AVP/TCP;unicast;interleaved=2-3;ssrc=1234ABCD;mode="PLAY"`,
	"RTP/AVP/TCP;unicast;interleaved=4-5",
	"RTP/AVP/TCP;unicast;interleaved=1-0",
	"RTP/AVP/TCP;unicast;interleaved=0-2",
	"RTP/AVP;unicast;client_port=5000-5001;server_port=6000-6001",
	"",
}

func FuzzParseTransport(f *testing.F) {
	for _, s := range transportSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		// The contract is total: ParseTransport never panics, and when it
		// succeeds InterleavedChannels (the only other panic surface here)
		// must not panic either, regardless of channel values or claimed set.
		th, err := rtsp.ParseTransport(value)
		if err == nil {
			_, _, _ = th.InterleavedChannels(map[int]bool{0: true, 255: true})
		}
	})
}

// sessionSeeds are the fixtures exercising ParseSession's branches: bare ID,
// explicit timeout, extra ignored parameters, surrounding whitespace, and
// the empty value.
var sessionSeeds = []string{
	"12345678",
	"12345678;timeout=60",
	"A9B8C7D6;timeout=30",
	"66334873;timeout=90;foo=bar",
	"  12345678  ",
	"",
}

func FuzzParseSession(f *testing.F) {
	for _, s := range sessionSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_ = rtsp.ParseSession(value)
	})
}
