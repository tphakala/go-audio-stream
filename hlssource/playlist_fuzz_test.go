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
	})
}
