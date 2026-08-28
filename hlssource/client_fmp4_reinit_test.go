package hlssource

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
)

// altASC is a second AudioSpecificConfig, distinct from wantASC, standing in for
// a replacement initialization segment that genuinely changes the audio
// configuration (a different sampling-frequency index).
var altASC = []byte{0x11, 0x90}

// thirdASC is a third distinct configuration, for the repeated-change test.
var thirdASC = []byte{0x13, 0x08}

const (
	initURL2 = "/init2.mp4"
	initRel2 = "init2.mp4"
	initURL3 = "/init3.mp4"
	initRel3 = "init3.mp4"
)

// timeline records frames and codec updates in ONE ordered sequence, so a test
// can assert not just that OnCodecUpdate fired but where it fell relative to the
// delivered frames. That relative position is the ordering guarantee
// Config.OnCodecUpdate documents, and a separate frame counter and update
// counter could not express it.
type timeline struct {
	mu     sync.Mutex
	events []string // "frame" or "codec:<hex asc>"
	ascs   [][]byte // the ASC of each codec event, in order
	frames int
}

//nolint:gocritic // OnFrame is func(audiostream.Frame); the value parameter is required by that signature.
func (tl *timeline) onFrame(audiostream.Frame) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.events = append(tl.events, "frame")
	tl.frames++
}

//nolint:gocritic // OnCodecUpdate is func(audiostream.CodecUpdate); the value parameter is required by that signature.
func (tl *timeline) onCodecUpdate(u audiostream.CodecUpdate) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	aac, ok := u.Codec.(audiostream.CodecAAC)
	if !ok {
		tl.events = append(tl.events, "codec:non-aac")
		tl.ascs = append(tl.ascs, nil)
		return
	}
	asc := append([]byte(nil), aac.AudioSpecificConfig...)
	tl.events = append(tl.events, "codec")
	tl.ascs = append(tl.ascs, asc)
}

// framesBeforeFirstCodecUpdate returns how many frames were delivered before the
// first codec update, and whether one fired at all.
func (tl *timeline) framesBeforeFirstCodecUpdate() (int, bool) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	n := 0
	for _, e := range tl.events {
		if e == "frame" {
			n++
			continue
		}
		return n, true
	}
	return n, false
}

func (tl *timeline) codecUpdates() [][]byte {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return append([][]byte(nil), tl.ascs...)
}

func (tl *timeline) frameCount() int {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return tl.frames
}

// twoInitLiveServer builds a live playlist that starts on init A with one
// fragment and, on the second and later reloads, advertises init B with a second
// fragment appended and ENDLIST. That is the shape of a live stream scrolling in
// a replacement EXT-X-MAP: the first fragment is already delivered under init A,
// so only the second triggers the re-initialization.
func twoInitLiveServer(t *testing.T, segs map[string][]byte, initRelA, initRelB string) *hlsServer {
	t.Helper()
	v1 := buildFMP4MediaPlaylist(1, 0, false, initRelA, []segSpec{{uri: fragRel0, duration: 1.0}})
	v2 := buildFMP4MediaPlaylist(1, 0, true, initRelB, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
	})
	return &hlsServer{
		segments: segs,
		playlist: func(n int) (string, int) {
			if n == 1 {
				return v1, http.StatusOK
			}
			return v2, http.StatusOK
		},
	}
}

// TestFMP4InitChangeSameASCContinues covers a replacement initialization segment
// that carries the SAME AudioSpecificConfig: a re-publish, not a codec change.
// The stream must keep playing and deliver both segments' access units, and
// OnCodecUpdate must NOT fire, since nothing a decoder cares about changed.
func TestFMP4InitChangeSameASCContinues(t *testing.T) {
	fastReload(t)
	s0 := fmp4Samples(2, 40)
	s1 := fmp4Samples(2, 44)
	segs := map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, 1),
		initURL2: buildInitSegment(wantASC, 44100, 1),
		fragURL0: buildFragment(1, s0, 1024),
		fragURL1: buildFragment(1, s1, 1024),
	}
	h := twoInitLiveServer(t, segs, initRel, initRel2)
	srv := h.start(t)
	tl := &timeline{}
	c, err := Open(context.Background(), Config{
		URL:           srv.URL + "/live.m3u8",
		OnFrame:       tl.onFrame,
		OnCodecUpdate: tl.onCodecUpdate,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded (a replaced init must not end the stream)", werr)
	}
	if want := len(s0) + len(s1); tl.frameCount() != want {
		t.Errorf("delivered %d AUs across the init change, want %d", tl.frameCount(), want)
	}
	if ups := tl.codecUpdates(); len(ups) != 0 {
		t.Errorf("OnCodecUpdate fired %d times for an unchanged AudioSpecificConfig, want 0", len(ups))
	}
}

// TestFMP4InitChangeNewASCFiresCodecUpdate covers the real codec change: a
// replacement init whose AudioSpecificConfig differs must keep the stream
// playing, fire OnCodecUpdate exactly once with the NEW config, and fire it
// after the last frame under the old config and before the first frame under the
// new one. Format() keeps reporting what Open resolved, which is the documented
// contract and what keeps it lock-free for a concurrent caller.
func TestFMP4InitChangeNewASCFiresCodecUpdate(t *testing.T) {
	fastReload(t)
	s0 := fmp4Samples(2, 40)
	s1 := fmp4Samples(3, 44)
	segs := map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, 1),
		initURL2: buildInitSegment(altASC, 44100, 1),
		fragURL0: buildFragment(1, s0, 1024),
		fragURL1: buildFragment(1, s1, 1024),
	}
	h := twoInitLiveServer(t, segs, initRel, initRel2)
	srv := h.start(t)
	tl := &timeline{}
	c, err := Open(context.Background(), Config{
		URL:           srv.URL + "/live.m3u8",
		OnFrame:       tl.onFrame,
		OnCodecUpdate: tl.onCodecUpdate,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", werr)
	}

	if want := len(s0) + len(s1); tl.frameCount() != want {
		t.Errorf("delivered %d AUs across the codec change, want %d", tl.frameCount(), want)
	}
	ups := tl.codecUpdates()
	if len(ups) != 1 {
		t.Fatalf("OnCodecUpdate fired %d times, want exactly 1", len(ups))
	}
	if !bytes.Equal(ups[0], altASC) {
		t.Errorf("update ASC = %x, want the new config %x", ups[0], altASC)
	}

	// The ordering guarantee: every access unit of the first segment was demuxed
	// under the old config and must precede the update, and the second segment's
	// units must follow it.
	before, fired := tl.framesBeforeFirstCodecUpdate()
	if !fired {
		t.Fatal("no codec update in the event timeline")
	}
	if before != len(s0) {
		t.Errorf("%d frames preceded OnCodecUpdate, want %d (all of the old config's units, none of the new)",
			before, len(s0))
	}

	// Format is fixed at Open and does not follow the update.
	aac, ok := c.Format().Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("Format codec = %T, want CodecAAC", c.Format().Codec)
	}
	if !bytes.Equal(aac.AudioSpecificConfig, wantASC) {
		t.Errorf("Format ASC = %x, want the Open-time %x (Format must not follow OnCodecUpdate)",
			aac.AudioSpecificConfig, wantASC)
	}
}

// TestFMP4RepeatedInitChangesFireEachUpdate drives three configurations across
// successive reloads. The snapshot comparison must hold up repeatedly: one
// update per genuine change, each carrying the config that took effect, and none
// for the segments that reuse a config already in effect.
func TestFMP4RepeatedInitChangesFireEachUpdate(t *testing.T) {
	fastReload(t)
	s0 := fmp4Samples(1, 40)
	s1 := fmp4Samples(1, 44)
	s2 := fmp4Samples(1, 48)
	segs := map[string][]byte{
		initURL:   buildInitSegment(wantASC, 44100, 1),
		initURL2:  buildInitSegment(altASC, 44100, 1),
		initURL3:  buildInitSegment(thirdASC, 44100, 1),
		fragURL0:  buildFragment(1, s0, 1024),
		fragURL1:  buildFragment(1, s1, 1024),
		"/f2.m4s": buildFragment(1, s2, 1024),
	}
	v1 := buildFMP4MediaPlaylist(1, 0, false, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
	v2 := buildFMP4MediaPlaylist(1, 0, false, initRel2, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
	})
	v3 := buildFMP4MediaPlaylist(1, 0, true, initRel3, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0}, {uri: "f2.m4s", duration: 1.0},
	})
	h := &hlsServer{
		segments: segs,
		playlist: func(n int) (string, int) {
			switch n {
			case 1:
				return v1, http.StatusOK
			case 2:
				return v2, http.StatusOK
			default:
				return v3, http.StatusOK
			}
		},
	}
	srv := h.start(t)
	tl := &timeline{}
	c, err := Open(context.Background(), Config{
		URL:           srv.URL + "/live.m3u8",
		OnFrame:       tl.onFrame,
		OnCodecUpdate: tl.onCodecUpdate,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", werr)
	}
	ups := tl.codecUpdates()
	if len(ups) != 2 {
		t.Fatalf("OnCodecUpdate fired %d times across two changes, want 2", len(ups))
	}
	if !bytes.Equal(ups[0], altASC) {
		t.Errorf("first update ASC = %x, want %x", ups[0], altASC)
	}
	if !bytes.Equal(ups[1], thirdASC) {
		t.Errorf("second update ASC = %x, want %x", ups[1], thirdASC)
	}
	if want := len(s0) + len(s1) + len(s2); tl.frameCount() != want {
		t.Errorf("delivered %d AUs across two changes, want %d", tl.frameCount(), want)
	}
}

// TestFMP4InitChangeFetchFailureEndsStream covers the replacement init that
// cannot be fetched: the stream ends with the transport cause rather than
// silently continuing to demux fragments with a demuxer built for the old init.
func TestFMP4InitChangeFetchFailureEndsStream(t *testing.T) {
	fastReload(t)
	s0 := fmp4Samples(2, 40)
	s1 := fmp4Samples(2, 44)
	segs := map[string][]byte{
		initURL: buildInitSegment(wantASC, 44100, 1),
		// initURL2 is deliberately absent: the origin serves a playlist body for
		// any unknown path, so the init read yields a non-fMP4 body.
		fragURL0: buildFragment(1, s0, 1024),
		fragURL1: buildFragment(1, s1, 1024),
	}
	h := twoInitLiveServer(t, segs, initRel, initRel2)
	srv := h.start(t)
	c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	werr := c.Wait(context.Background())
	if errors.Is(werr, ErrStreamEnded) || werr == nil {
		t.Fatalf("Wait = %v, want a failure cause for an unreadable replacement init", werr)
	}
}

// TestFMP4InitChangeUnsupportedCodecEndsStream covers a replacement init whose
// audio sample entry is encrypted: the stream ends with ErrUnsupportedCodec, the
// same verdict Open would give for that init, rather than the demuxer being left
// on the old configuration.
func TestFMP4InitChangeUnsupportedCodecEndsStream(t *testing.T) {
	fastReload(t)
	s0 := fmp4Samples(2, 40)
	s1 := fmp4Samples(2, 44)
	segs := map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, 1),
		initURL2: buildEncryptedInitSegment(wantASC, 44100, 1),
		fragURL0: buildFragment(1, s0, 1024),
		fragURL1: buildFragment(1, s1, 1024),
	}
	h := twoInitLiveServer(t, segs, initRel, initRel2)
	srv := h.start(t)
	c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrUnsupportedCodec) {
		t.Fatalf("Wait = %v, want ErrUnsupportedCodec (encrypted replacement init)", werr)
	}
}

// TestFMP4InitChangeMalformedEndsStream covers a replacement init that is not a
// parseable initialization segment at all.
func TestFMP4InitChangeMalformedEndsStream(t *testing.T) {
	fastReload(t)
	s0 := fmp4Samples(2, 40)
	s1 := fmp4Samples(2, 44)
	segs := map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, 1),
		initURL2: make([]byte, 64), // 64 zero bytes: not a parseable ftyp/moov
		fragURL0: buildFragment(1, s0, 1024),
		fragURL1: buildFragment(1, s1, 1024),
	}
	h := twoInitLiveServer(t, segs, initRel, initRel2)
	srv := h.start(t)
	c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrMalformedSegment) {
		t.Fatalf("Wait = %v, want ErrMalformedSegment (unparseable replacement init)", werr)
	}
}

// TestFMP4ContainerSwitchIsUnsupported pins the boundary the re-initialization
// deliberately stops at: replacing one fMP4 init with another is played, but a
// stream that ADDS or DROPS EXT-X-MAP mid-stream is switching demuxer families
// (MPEG-TS and fMP4 have different framing semantics), which stays
// ErrUnsupportedPlaylist.
func TestFMP4ContainerSwitchIsUnsupported(t *testing.T) {
	t.Run("fmp4 to ts", func(t *testing.T) {
		fastReload(t)
		s0 := fmp4Samples(2, 40)
		tsStream, _ := adtsStream(2, 40)
		segs := map[string][]byte{
			initURL:    buildInitSegment(wantASC, 44100, 1),
			fragURL0:   buildFragment(1, s0, 1024),
			"/seg1.ts": buildTSSegment(tsStream, 0x1000, 0x0100),
		}
		v1 := buildFMP4MediaPlaylist(1, 0, false, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
		// No EXT-X-MAP at all on the reload: the second segment is plain MPEG-TS.
		v2 := buildMediaPlaylist(1, 0, true, []segSpec{
			{uri: fragRel0, duration: 1.0}, {uri: "seg1.ts", duration: 1.0},
		})
		h := &hlsServer{segments: segs, playlist: func(n int) (string, int) {
			if n == 1 {
				return v1, http.StatusOK
			}
			return v2, http.StatusOK
		}}
		srv := h.start(t)
		c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8"})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if werr := c.Wait(context.Background()); !errors.Is(werr, ErrUnsupportedPlaylist) {
			t.Fatalf("Wait = %v, want ErrUnsupportedPlaylist (fMP4 to TS)", werr)
		}
	})

	t.Run("ts to fmp4", func(t *testing.T) {
		fastReload(t)
		tsStream, _ := adtsStream(2, 40)
		s1 := fmp4Samples(2, 44)
		segs := map[string][]byte{
			tsSegURL0: buildTSSegment(tsStream, 0x1000, 0x0100),
			initURL:   buildInitSegment(wantASC, 44100, 1),
			fragURL1:  buildFragment(1, s1, 1024),
		}
		v1 := buildMediaPlaylist(1, 0, false, []segSpec{{uri: tsSegRel0, duration: 1.0}})
		v2 := buildFMP4MediaPlaylist(1, 0, true, initRel, []segSpec{
			{uri: tsSegRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
		})
		h := &hlsServer{segments: segs, playlist: func(n int) (string, int) {
			if n == 1 {
				return v1, http.StatusOK
			}
			return v2, http.StatusOK
		}}
		srv := h.start(t)
		c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8"})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if werr := c.Wait(context.Background()); !errors.Is(werr, ErrUnsupportedPlaylist) {
			t.Fatalf("Wait = %v, want ErrUnsupportedPlaylist (TS to fMP4)", werr)
		}
	})
}

// TestFMP4InitChangeKeepsMalformedCounterMonotonic guards the counter across the
// demuxer swap. The replacement demuxer starts its own gap count at zero, so
// publishing it verbatim would make Stats().Malformed jump backwards; the
// retired demuxer's total is carried instead. The first segment is truncated so
// there is a real gap to lose.
func TestFMP4InitChangeKeepsMalformedCounterMonotonic(t *testing.T) {
	fastReload(t)
	// Two samples, with the mdat truncated so the SECOND overruns: one access
	// unit is still delivered (Open needs one to resolve the ASC) and one gap is
	// counted on the first demuxer.
	s0 := fmp4Samples(2, 30)
	shortFrag := buildFragment(1, s0, 1024)
	shortFrag = shortFrag[:len(shortFrag)-20]
	s1 := fmp4Samples(2, 44)
	segs := map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, 1),
		initURL2: buildInitSegment(altASC, 44100, 1),
		fragURL0: shortFrag,
		fragURL1: buildFragment(1, s1, 1024),
	}
	h := twoInitLiveServer(t, segs, initRel, initRel2)
	srv := h.start(t)
	c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", werr)
	}
	// Precondition: the truncated first segment must actually have produced a gap,
	// else the assertion below would hold trivially at zero.
	got := c.Stats().Tracks[0].Malformed
	if got == 0 {
		t.Fatal("no gap was counted on the first segment; the fixture no longer exercises the swap")
	}
	if got < 1 {
		t.Errorf("Malformed = %d after the init change, want the retired demuxer's count carried over", got)
	}
}
