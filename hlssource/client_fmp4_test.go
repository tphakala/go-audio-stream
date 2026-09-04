package hlssource

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

const (
	initURL  = "/init.mp4"
	fragURL0 = "/f0.m4s"
	fragURL1 = "/f1.m4s"
	fragRel0 = "f0.m4s"
	fragRel1 = "f1.m4s"
	initRel  = "init.mp4"
)

func TestFMP4VODDeliversAllSamplesThenEnds(t *testing.T) {
	init := buildInitSegment(wantASC, 44100, 1)
	s0 := fmp4Samples(2, 40)
	s1 := fmp4Samples(3, 55)
	wantAUs := make([][]byte, 0, len(s0)+len(s1))
	wantAUs = append(wantAUs, s0...)
	wantAUs = append(wantAUs, s1...)
	segs := map[string][]byte{
		initURL:  init,
		fragURL0: buildFragment(1, s0, 1024),
		fragURL1: buildFragment(1, s1, 1024),
	}
	body := buildFMP4MediaPlaylist(1, 0, true, initRel, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
	})
	h := &hlsServer{segments: segs, playlist: func(int) (string, int) { return body, http.StatusOK }}
	srv := h.start(t)
	col := &collector{}
	c, err := Open(t.Context(), Config{URL: srv.URL + "/vod.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Codec parity: the fMP4 track resolves the byte-identical AAC config.
	aac, ok := c.Format().Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("codec = %T, want CodecAAC", c.Format().Codec)
	}
	if !bytes.Equal(aac.AudioSpecificConfig, wantASC) {
		t.Errorf("ASC = %x, want %x", aac.AudioSpecificConfig, wantASC)
	}
	if err := c.Wait(t.Context()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(wantAUs) {
		t.Fatalf("delivered %d AUs, want %d", col.count(), len(wantAUs))
	}
	for i := range wantAUs {
		if !bytes.Equal(col.datas[i], wantAUs[i]) {
			t.Errorf("AU %d bytes mismatch end to end", i)
		}
	}
	// PTS strictly increasing from 0.
	if col.frames[0].PTS != 0 {
		t.Errorf("first PTS = %v, want 0", col.frames[0].PTS)
	}
	var prev time.Duration = -1
	for i, f := range col.frames {
		if i > 0 && f.PTS <= prev {
			t.Errorf("frame %d PTS %v not strictly increasing (prev %v)", i, f.PTS, prev)
		}
		prev = f.PTS
	}
}

func TestFMP4LiveReloadDeliversNewSegments(t *testing.T) {
	fastReload(t)
	init := buildInitSegment(wantASC, 44100, 1)
	s0 := fmp4Samples(2, 40)
	s1 := fmp4Samples(2, 44)
	segs := map[string][]byte{
		initURL:  init,
		fragURL0: buildFragment(1, s0, 1024),
		fragURL1: buildFragment(1, s1, 1024),
	}
	v1 := buildFMP4MediaPlaylist(1, 0, false, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
	v2 := buildFMP4MediaPlaylist(1, 0, true, initRel, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
	})
	h := &hlsServer{
		segments: segs,
		playlist: func(n int) (string, int) {
			if n == 1 {
				return v1, http.StatusOK
			}
			return v2, http.StatusOK
		},
	}
	srv := h.start(t)
	col := &collector{}
	c, err := Open(t.Context(), Config{URL: srv.URL + "/live.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(t.Context()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if want := len(s0) + len(s1); col.count() != want {
		t.Fatalf("delivered %d AUs across reload, want %d", col.count(), want)
	}
}

func TestFMP4MasterSelectsMediaPlaylist(t *testing.T) {
	init := buildInitSegment(wantASC, 44100, 1)
	s0 := fmp4Samples(2, 40)
	segs := map[string][]byte{initURL: init, fragURL0: buildFragment(1, s0, 1024)}
	mediaBody := buildFMP4MediaPlaylist(1, 0, true, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=64000\nmedia.m3u8\n"
	h := &hlsServer{
		segments: segs,
		playlist: func(n int) (string, int) {
			if n == 1 {
				return master, http.StatusOK
			}
			return mediaBody, http.StatusOK
		},
	}
	srv := h.start(t)
	col := &collector{}
	c, err := Open(t.Context(), Config{URL: srv.URL + "/master.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(t.Context()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(s0) {
		t.Errorf("delivered %d AUs via master, want %d", col.count(), len(s0))
	}
}

func TestFMP4MultiplexedFragmentPicksAudio(t *testing.T) {
	// The media fragments carry both a video traf (bytes first in mdat) and the
	// audio traf. Only the audio samples must be delivered, byte-identical.
	init := buildInitSegment(wantASC, 44100, 2)
	audio := fmp4Samples(3, 40)
	video := bytes.Repeat([]byte{0xFF}, 120)
	segs := map[string][]byte{
		initURL:  init,
		fragURL0: buildMultiplexedFragment(1, video, 2, audio, 1024),
	}
	body := buildFMP4MediaPlaylist(1, 0, true, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
	h := &hlsServer{segments: segs, playlist: func(int) (string, int) { return body, http.StatusOK }}
	srv := h.start(t)
	col := &collector{}
	c, err := Open(t.Context(), Config{URL: srv.URL + "/vod.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(t.Context()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(audio) {
		t.Fatalf("delivered %d AUs, want %d (audio only)", col.count(), len(audio))
	}
	for i := range audio {
		if !bytes.Equal(col.datas[i], audio[i]) {
			t.Errorf("AU %d is not the audio sample (video leaked into the AAC path?)", i)
		}
	}
}
