package hlssource

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse base URL %q: %v", s, err)
	}
	return u
}

func parseMedia(t *testing.T, body, base string) *mediaPlaylist {
	t.Helper()
	media, master, err := parsePlaylist([]byte(body), mustURL(t, base))
	if err != nil {
		t.Fatalf("parsePlaylist: %v", err)
	}
	if master != nil {
		t.Fatal("expected a media playlist, got a master")
	}
	if media == nil {
		t.Fatal("expected a media playlist, got nil")
	}
	return media
}

func parseMaster(t *testing.T, body, base string) *masterPlaylist {
	t.Helper()
	media, master, err := parsePlaylist([]byte(body), mustURL(t, base))
	if err != nil {
		t.Fatalf("parsePlaylist: %v", err)
	}
	if media != nil {
		t.Fatal("expected a master playlist, got a media")
	}
	if master == nil {
		t.Fatal("expected a master playlist, got nil")
	}
	return master
}

func TestParseMediaPlaylistHappyPath(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXT-X-VERSION:3\n" +
		"#EXT-X-TARGETDURATION:10\n" +
		"#EXTINF:9.9,\n" +
		"seg0.ts\n" +
		"#EXTINF:10.0,\n" +
		"seg1.ts\n" +
		"#EXTINF:8.0,\n" +
		"sub/seg2.ts\n" +
		"#EXT-X-ENDLIST\n"
	m := parseMedia(t, body, "https://h/live/index.m3u8")
	if m.targetDuration != 10*time.Second {
		t.Errorf("targetDuration = %v, want 10s", m.targetDuration)
	}
	if !m.endList {
		t.Error("endList = false, want true")
	}
	if len(m.segments) != 3 {
		t.Fatalf("segments = %d, want 3", len(m.segments))
	}
	wantURIs := []string{
		"https://h/live/seg0.ts",
		"https://h/live/seg1.ts",
		"https://h/live/sub/seg2.ts",
	}
	for i, seg := range m.segments {
		if seg.seq != uint64(i) {
			t.Errorf("segment %d seq = %d, want %d", i, seg.seq, i)
		}
		if seg.uri != wantURIs[i] {
			t.Errorf("segment %d uri = %q, want %q", i, seg.uri, wantURIs[i])
		}
	}
	if m.segments[0].duration != 9900*time.Millisecond {
		t.Errorf("segment 0 duration = %v, want 9.9s", m.segments[0].duration)
	}
}

func TestParseMediaSequenceOffsetsSeq(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:42\n" +
		"#EXTINF:6,\na.ts\n#EXTINF:6,\nb.ts\n"
	m := parseMedia(t, body, "https://h/x.m3u8")
	if m.mediaSequence != 42 {
		t.Errorf("mediaSequence = %d, want 42", m.mediaSequence)
	}
	if m.segments[0].seq != 42 || m.segments[1].seq != 43 {
		t.Errorf("seqs = %d,%d, want 42,43", m.segments[0].seq, m.segments[1].seq)
	}
}

func TestParseDiscontinuityMarksNextSegmentOnly(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"#EXTINF:6,\na.ts\n" +
		"#EXT-X-DISCONTINUITY\n#EXTINF:6,\nb.ts\n" +
		"#EXTINF:6,\nc.ts\n"
	m := parseMedia(t, body, "https://h/x.m3u8")
	if m.segments[0].discontinuity {
		t.Error("segment 0 should not be flagged discontinuous")
	}
	if !m.segments[1].discontinuity {
		t.Error("segment 1 should be flagged discontinuous")
	}
	if m.segments[2].discontinuity {
		t.Error("segment 2 should not be flagged discontinuous")
	}
}

func TestParseExtXGapMarksSegment(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"#EXTINF:6,\na.ts\n" +
		"#EXT-X-GAP\n#EXTINF:6,\ngap.ts\n" +
		"#EXTINF:6,\nc.ts\n"
	m := parseMedia(t, body, "https://h/x.m3u8")
	if m.segments[0].gap || m.segments[2].gap {
		t.Error("only the middle segment should be a gap")
	}
	if !m.segments[1].gap {
		t.Error("segment 1 should be flagged as a gap")
	}
}

func TestParseAbsoluteSegmentURI(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:6,\nhttps://cdn.example/x/a.ts\n"
	m := parseMedia(t, body, "https://h/live/x.m3u8")
	if m.segments[0].uri != "https://cdn.example/x/a.ts" {
		t.Errorf("absolute uri = %q, want it used verbatim", m.segments[0].uri)
	}
}

func TestParseCRLFLineEndings(t *testing.T) {
	body := "#EXTM3U\r\n#EXT-X-TARGETDURATION:6\r\n#EXTINF:6,\r\na.ts\r\n#EXT-X-ENDLIST\r\n"
	m := parseMedia(t, body, "https://h/x.m3u8")
	if len(m.segments) != 1 || m.segments[0].uri != "https://h/a.ts" {
		t.Fatalf("CRLF parse failed: %+v", m.segments)
	}
}

func TestParseMasterSelectsLowestBandwidth(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=800000\nhi.m3u8\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=128000\nlo.m3u8\n"
	master := parseMaster(t, body, "https://h/master.m3u8")
	got, err := master.selectMediaURL()
	if err != nil {
		t.Fatalf("selectMediaURL: %v", err)
	}
	if got != "https://h/lo.m3u8" {
		t.Errorf("selected %q, want the lowest-bandwidth lo.m3u8", got)
	}
}

func TestParseMasterPrefersAudioRendition(t *testing.T) {
	body := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aac\",NAME=\"English\",DEFAULT=YES,URI=\"audio/en.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=128000,AUDIO=\"aac\"\nvideo.m3u8\n"
	master := parseMaster(t, body, "https://h/master.m3u8")
	got, err := master.selectMediaURL()
	if err != nil {
		t.Fatalf("selectMediaURL: %v", err)
	}
	if got != "https://h/audio/en.m3u8" {
		t.Errorf("selected %q, want the audio rendition audio/en.m3u8", got)
	}
}

func TestParseMasterMuxedVariantWhenNoAudioURI(t *testing.T) {
	// An audio group whose renditions carry no URI means the audio is muxed into
	// the variant stream, so the variant URI is what we fetch.
	body := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aac\",NAME=\"English\",DEFAULT=YES\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=128000,AUDIO=\"aac\"\nmuxed.m3u8\n"
	master := parseMaster(t, body, "https://h/master.m3u8")
	got, err := master.selectMediaURL()
	if err != nil {
		t.Fatalf("selectMediaURL: %v", err)
	}
	if got != "https://h/muxed.m3u8" {
		t.Errorf("selected %q, want the muxed variant muxed.m3u8", got)
	}
}

func TestParseMasterNoAudioIsUnsupported(t *testing.T) {
	// A master carrying only a subtitles rendition and no variants: it is a valid
	// master (EXT-X-MEDIA makes it one) but has nothing this source can play.
	body := "#EXTM3U\n" +
		"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",NAME=\"English\",URI=\"subs/en.m3u8\"\n"
	master := parseMaster(t, body, "https://h/master.m3u8")
	if _, err := master.selectMediaURL(); !errors.Is(err, ErrUnsupportedPlaylist) {
		t.Errorf("selectMediaURL with no variants = %v, want ErrUnsupportedPlaylist", err)
	}
}

func TestParseRejectsMissingHeader(t *testing.T) {
	body := "#EXT-X-TARGETDURATION:6\n#EXTINF:6,\na.ts\n"
	if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrMalformedPlaylist) {
		t.Errorf("missing #EXTM3U = %v, want ErrMalformedPlaylist", err)
	}
}

func TestParseRejectsMediaWithoutTargetDuration(t *testing.T) {
	body := "#EXTM3U\n#EXTINF:6,\na.ts\n"
	if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrMalformedPlaylist) {
		t.Errorf("missing target duration = %v, want ErrMalformedPlaylist", err)
	}
}

func TestParseRejectsSegmentWithoutExtinf(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\na.ts\n"
	if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrMalformedPlaylist) {
		t.Errorf("segment without EXTINF = %v, want ErrMalformedPlaylist", err)
	}
}

func TestParseRejectsBadExtinf(t *testing.T) {
	for _, field := range []string{"notanumber", "NaN", "Inf", "-1", "999999999"} {
		body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXTINF:" + field + ",\na.ts\n"
		if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrMalformedPlaylist) {
			t.Errorf("EXTINF %q = %v, want ErrMalformedPlaylist", field, err)
		}
	}
}

func TestParseRejectsOutOfRangeSequence(t *testing.T) {
	for _, tag := range []string{"EXT-X-MEDIA-SEQUENCE", "EXT-X-DISCONTINUITY-SEQUENCE"} {
		body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n#" + tag + ":99999999999999999999\n#EXTINF:6,\na.ts\n"
		if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrMalformedPlaylist) {
			t.Errorf("out-of-range %s = %v, want ErrMalformedPlaylist", tag, err)
		}
	}
}

func TestParseRejectsEncryption(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:6,\na.ts\n"
	if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrUnsupportedPlaylist) {
		t.Errorf("AES-128 key = %v, want ErrUnsupportedPlaylist", err)
	}
}

func TestParseAllowsKeyMethodNone(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-KEY:METHOD=NONE\n#EXTINF:6,\na.ts\n"
	m := parseMedia(t, body, "https://h/x.m3u8")
	if len(m.segments) != 1 {
		t.Errorf("METHOD=NONE should be allowed, got %d segments", len(m.segments))
	}
}

func TestParseAcceptsFmp4Map(t *testing.T) {
	// EXT-X-MAP (fMP4/CMAF) is now supported: the init URI is resolved against the
	// playlist base and threaded onto every following segment, marking it fMP4.
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:6,\na.m4s\n#EXTINF:6,\nb.m4s\n"
	m := parseMedia(t, body, "https://h/p/x.m3u8")
	if len(m.segments) != 2 {
		t.Fatalf("parsed %d segments, want 2", len(m.segments))
	}
	for i, s := range m.segments {
		if s.initURI != "https://h/p/init.mp4" {
			t.Errorf("segment %d initURI = %q, want the resolved init.mp4", i, s.initURI)
		}
	}
}

func TestParseRejectsFmp4MapNoURI(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-MAP:BYTERANGE=\"0@0\"\n#EXTINF:6,\na.m4s\n"
	if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrUnsupportedPlaylist) {
		t.Errorf("EXT-X-MAP BYTERANGE = %v, want ErrUnsupportedPlaylist", err)
	}
	body = "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-MAP:FOO=\"bar\"\n#EXTINF:6,\na.m4s\n"
	if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrMalformedPlaylist) {
		t.Errorf("EXT-X-MAP without URI = %v, want ErrMalformedPlaylist", err)
	}
}

func TestParseRejectsByteRange(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"#EXT-X-BYTERANGE:75232@0\n#EXTINF:6,\na.ts\n"
	if _, _, err := parsePlaylist([]byte(body), mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrUnsupportedPlaylist) {
		t.Errorf("EXT-X-BYTERANGE = %v, want ErrUnsupportedPlaylist", err)
	}
}

func TestParseIgnoresUnknownTagsAndComments(t *testing.T) {
	body := "#EXTM3U\n#EXT-X-TARGETDURATION:6\n" +
		"# a plain comment\n#EXT-X-PROGRAM-DATE-TIME:2026-08-21T00:00:00Z\n" +
		"#EXT-X-INDEPENDENT-SEGMENTS\n#EXTINF:6,\na.ts\n"
	m := parseMedia(t, body, "https://h/x.m3u8")
	if len(m.segments) != 1 {
		t.Errorf("unknown tags should be ignored, got %d segments", len(m.segments))
	}
}

// TestSegmentCountCap covers MaxSegmentsPerPlaylist: a playlist at the cap
// parses, one segment past it is rejected as unsupported (well-formed HLS this
// source refuses, not a parse failure). The over-cap body is the at-cap body
// with one extra entry appended, so the larger cap does not pay to regenerate a
// whole second playlist.
func TestSegmentCountCap(t *testing.T) {
	build := func(n int) []byte {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
		for i := 0; i < n; i++ {
			b.WriteString("#EXTINF:4.0,\n")
			fmt.Fprintf(&b, "s%d.ts\n", i)
		}
		return []byte(b.String())
	}

	atCap := build(MaxSegmentsPerPlaylist)
	media, _, err := parsePlaylist(atCap, mustURL(t, "https://h/x.m3u8"))
	if err != nil {
		t.Fatalf("playlist at the cap: unexpected %v", err)
	}
	if got := len(media.segments); got != MaxSegmentsPerPlaylist {
		t.Fatalf("playlist at the cap: parsed %d segments, want %d", got, MaxSegmentsPerPlaylist)
	}

	overCap := append(append([]byte(nil), atCap...), "#EXTINF:4.0,\nsX.ts\n"...)
	if _, _, err := parsePlaylist(overCap, mustURL(t, "https://h/x.m3u8")); !errors.Is(err, ErrUnsupportedPlaylist) {
		t.Fatalf("playlist one past the cap: got %v, want ErrUnsupportedPlaylist", err)
	}
}
