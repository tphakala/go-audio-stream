package rtsp

import (
	"strings"
	"testing"
)

// parseRTPInfoEntry reads a header a remote server controls, so it is fuzzed
// like every other wire parser in this package: it must be total, and it must
// never report ok for a value that later arithmetic would trust. The invariant
// checked here is that a successful parse yields an rtptime that fits the
// 32-bit RTP timestamp field, since the caller stores it as a timestamp origin
// and subtracts it from packet timestamps.
func FuzzParseRTPInfoEntry(f *testing.F) {
	f.Add("url=rtsp://cam/stream/audio;seq=1000;rtptime=123456")
	f.Add("url=audio;seq=0;rtptime=0")
	f.Add("url=audio;rtptime=5")
	f.Add("seq=1;rtptime=4294967295")
	f.Add("seq=1;rtptime=4294967296")
	f.Add(";;;=;=x;url=")
	f.Add("url=a b;seq=-1;rtptime=+7")

	f.Fuzz(func(t *testing.T, entry string) {
		url, seq, rtptime, ok := parseRTPInfoEntry(entry)
		if !ok {
			return
		}
		if rtptime > 0xFFFFFFFF {
			t.Fatalf("accepted rtptime %d, above the 32-bit RTP timestamp range", rtptime)
		}
		if seq > 0xFFFFFFFF {
			t.Fatalf("accepted seq %d, above the 32-bit range", seq)
		}
		if strings.ContainsAny(url, "\r\n") {
			t.Fatalf("accepted a url carrying a CR or LF: %q", url)
		}
	})
}
