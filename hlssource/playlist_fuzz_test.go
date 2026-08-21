package hlssource

import (
	"net/url"
	"testing"
)

// FuzzParsePlaylist drives the m3u8 parser with arbitrary bytes. It must never
// panic, and on success exactly one of the media/master pointers is non-nil. The
// parsing behavior itself has dedicated tests; this is a totality check over
// untrusted playlist bodies.
func FuzzParsePlaylist(f *testing.F) {
	base, _ := url.Parse("https://h/live/index.m3u8")
	f.Add([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\na.ts\n#EXT-X-ENDLIST\n"))
	f.Add([]byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n"))
	f.Add([]byte("#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"x.m3u8\"\n"))
	f.Add([]byte("not a playlist"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, body []byte) {
		media, master, err := parsePlaylist(body, base)
		if err != nil {
			return
		}
		if (media == nil) == (master == nil) {
			t.Fatalf("parsePlaylist returned media=%v master=%v; exactly one must be non-nil", media != nil, master != nil)
		}
		if master != nil {
			// selectMediaURL must not panic on any parsed master.
			_, _ = master.selectMediaURL()
		}
		if media != nil {
			// A well-formed media playlist must satisfy the invariants the parser
			// promises, not merely not panic. seq is mediaSequence + index and the
			// segments run consecutively; the consecutive check is written as
			// seq[i] == seq[i-1]+1 (both modular) so it also holds at the uint64
			// wraparound a hostile EXT-X-MEDIA-SEQUENCE near MaxUint64 can force.
			if media.targetDuration <= 0 {
				t.Fatalf("media playlist has non-positive targetDuration %v", media.targetDuration)
			}
			for i, s := range media.segments {
				if want := media.mediaSequence + uint64(i); s.seq != want {
					t.Fatalf("segment %d seq = %d, want mediaSequence(%d)+%d = %d", i, s.seq, media.mediaSequence, i, want)
				}
				if i > 0 && s.seq != media.segments[i-1].seq+1 {
					t.Fatalf("segment %d seq %d is not consecutive after %d", i, s.seq, media.segments[i-1].seq)
				}
				if s.duration < 0 {
					t.Fatalf("segment %d has negative duration %v", i, s.duration)
				}
			}
		}
	})
}
