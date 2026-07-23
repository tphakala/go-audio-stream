package rtsp

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// parseRTPInfoEntry reads a header a remote server controls, so it is fuzzed
// like every other wire parser in this package: it must be total, and it must
// never report ok for a value that later arithmetic would trust.
//
// The invariants are re-derived from the input rather than restated from the
// implementation. Asserting only that rtptime fits 32 bits would pin the
// ParseUint bitSize argument and nothing else, and a parser mutated to return
// ok unconditionally would still satisfy it.
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
		if rtptime > math.MaxUint32 {
			t.Fatalf("accepted rtptime %d, above the 32-bit RTP timestamp range", rtptime)
		}
		if seq > math.MaxUint32 {
			t.Fatalf("accepted seq %d, above the 32-bit range", seq)
		}
		// The url reaches URL resolution and string comparison against this
		// client's own control URLs, so a control character must never survive.
		if strings.ContainsAny(url, "\r\n\x00") {
			t.Fatalf("accepted a url carrying a control character: %q", url)
		}
		// ok means BOTH fields were present and parsed. Re-derive that from the
		// entry rather than trusting the parser that just reported it: a parser
		// that returned ok unconditionally would pass every check above.
		gotSeq, gotTime := false, false
		var wantSeq, wantTime uint64
		for part := range strings.SplitSeq(entry, ";") {
			name, val, has := strings.Cut(strings.TrimSpace(part), "=")
			if !has {
				continue
			}
			n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 32)
			if err != nil {
				continue
			}
			// A repeated parameter takes its last value, which is what the
			// parser's plain assignment does; nothing in RFC 2326 defines the
			// case, so the invariant records the choice rather than asserting a
			// rule the format does not have.
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "seq":
				gotSeq, wantSeq = true, n
			case "rtptime":
				gotTime, wantTime = true, n
			}
		}
		if !gotSeq || !gotTime {
			t.Fatalf("reported ok for %q, which carries seq=%v rtptime=%v", entry, gotSeq, gotTime)
		}
		if seq != wantSeq {
			t.Fatalf("seq = %d, want the last seq= value %d in %q", seq, wantSeq, entry)
		}
		if rtptime != wantTime {
			t.Fatalf("rtptime = %d, want the last rtptime= value %d in %q", rtptime, wantTime, entry)
		}
	})
}
