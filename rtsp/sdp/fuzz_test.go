package sdp_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/sdp"
)

func TestAllFixturesRoundTrip(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("..", "..", "testdata", "fixtures", "sdp")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixtures dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no SDP fixtures found")
	}
	for _, e := range entries {
		t.Run(e.Name(), func(t *testing.T) {
			t.Parallel()
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			s, err := sdp.Parse(b)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := s.Codecs(); len(got) != len(s.Media) {
				t.Errorf("Codecs len = %d, want %d", len(got), len(s.Media))
			}
		})
	}
}

func FuzzParse(f *testing.F) {
	dir := filepath.Join("..", "..", "testdata", "fixtures", "sdp")
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				f.Add(b)
			}
		}
	}
	f.Add([]byte(""))
	f.Add([]byte("v=0\r\nm=audio 0 RTP/AVP 97\r\na=rtpmap:97 MPEG4-GENERIC/16000/1\r\na=fmtp:97 config=1408\r\n"))
	f.Add([]byte("a=control:")) // attribute at end of input
	f.Fuzz(func(t *testing.T, body []byte) {
		s, err := sdp.Parse(body)
		if err != nil {
			return // typed error is fine; the contract is no panic
		}
		_ = s.Codecs() // must also not panic on any parseable input
	})
}
