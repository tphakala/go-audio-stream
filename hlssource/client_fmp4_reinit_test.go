package hlssource

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
)

// The AudioSpecificConfigs these tests move between. wantASC is the shared
// fixture default; altASC and thirdASC stand in for a live playlist scrolling in
// an initialization segment that genuinely changes the audio configuration.
var (
	altASC   = []byte{0x11, 0x90}
	thirdASC = []byte{0x13, 0x08}
)

const (
	initURL2 = "/init2.mp4"
	initRel2 = "init2.mp4"
	initURL3 = "/init3.mp4"
	initRel3 = "init3.mp4"
	fragURL2 = "/f2.m4s"
	fragRel2 = "f2.m4s"
	fragURL3 = "/f3.m4s"
	fragRel3 = "f3.m4s"
)

// Each initialization segment in these fixtures declares a DIFFERENT audio
// track_ID, and each fragment is built for the track_ID of the init it belongs
// to. That is what makes a missing demuxer swap observable: internal/mp4 locates
// a fragment's traf by the track_ID resolved from the init, so a demuxer left on
// the previous init cannot parse the replacement fragments at all. With every
// init sharing one track_ID (and one timescale) a stale demuxer parses the new
// fragments identically, and a test that only counts frames passes even when the
// swap is deleted outright.
const (
	trackInit1 = 1
	trackInit2 = 2
	trackInit3 = 3
)

// timeline records frames and codec updates in ONE ordered sequence, so a test
// can assert not just that OnCodecUpdate fired but where it fell relative to the
// delivered frames. That relative position is the ordering guarantee
// Config.OnCodecUpdate documents, and two independent counters cannot express it.
type timeline struct {
	mu     sync.Mutex
	events []string // "frame", "codec", or "codec:non-aac"
	ascs   [][]byte // the ASC of each codec event, in order
	// mutateOnUpdate zeroes each AudioSpecificConfig handed to onCodecUpdate,
	// after copying it, to prove the source handed out a copy rather than a slice
	// its own state still points at.
	mutateOnUpdate bool
}

//nolint:gocritic // OnFrame is func(audiostream.Frame); the value parameter is required by that signature.
func (tl *timeline) onFrame(audiostream.Frame) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.events = append(tl.events, "frame")
}

func (tl *timeline) onCodecUpdate(u audiostream.CodecUpdate) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	aac, ok := u.Codec.(audiostream.CodecAAC)
	if !ok {
		tl.events = append(tl.events, "codec:non-aac")
		tl.ascs = append(tl.ascs, nil)
		return
	}
	tl.events = append(tl.events, "codec")
	tl.ascs = append(tl.ascs, append([]byte(nil), aac.AudioSpecificConfig...))
	if tl.mutateOnUpdate {
		// The contract documents the slice as read-only; a consumer that ignores
		// that must not be able to corrupt the source's comparison snapshot or the
		// live demuxer's resolved configuration.
		for i := range aac.AudioSpecificConfig {
			aac.AudioSpecificConfig[i] = 0
		}
	}
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

// frameCount derives the frame total from the one ordered event log, so there is
// no second counter to keep in sync with it.
func (tl *timeline) frameCount() int {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	n := 0
	for _, e := range tl.events {
		if e == "frame" {
			n++
		}
	}
	return n
}

// twoInitFixture builds the segment map and the two playlist bodies for a live
// stream that starts on init A with one fragment and, from the second reload,
// advertises init B with a second fragment appended and ENDLIST. Each fragment
// carries the track_ID of its own init, so a demuxer left on init A cannot parse
// the init B fragment.
func twoInitFixture(initB []byte, s0, s1 [][]byte) (segs map[string][]byte, v1, v2 string) {
	segs = map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, trackInit1),
		initURL2: initB,
		fragURL0: buildFragment(trackInit1, s0, 1024),
		fragURL1: buildFragment(trackInit2, s1, 1024),
	}
	v1 = buildFMP4MediaPlaylist(1, 0, false, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
	v2 = buildFMP4MediaPlaylist(1, 0, true, initRel2, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
	})
	return segs, v1, v2
}

// twoPhaseServer serves body1 on the first playlist request and body2 on every
// one after it.
func twoPhaseServer(segs map[string][]byte, body1, body2 string) *hlsServer {
	return &hlsServer{
		segments: segs,
		playlist: func(n int) (string, int) {
			if n == 1 {
				return body1, http.StatusOK
			}
			return body2, http.StatusOK
		},
	}
}

// TestFMP4InitChangeSameASCContinues covers a replacement initialization segment
// that carries the SAME AudioSpecificConfig: a re-publish, not a codec change.
// The stream must keep playing and deliver both segments' access units, and
// OnCodecUpdate must NOT fire, since nothing a decoder cares about changed.
func TestFMP4InitChangeSameASCContinues(t *testing.T) {
	fastReload(t)
	s0, s1 := fmp4Samples(2, 40), fmp4Samples(2, 44)
	segs, v1, v2 := twoInitFixture(buildInitSegment(wantASC, 44100, trackInit2), s0, s1)
	srv := twoPhaseServer(segs, v1, v2).start(t)

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
// playing, fire OnCodecUpdate exactly once with the NEW config, and fire it after
// the last frame under the old config and before the first frame under the new
// one. Format() keeps reporting what Open resolved, which is the documented
// contract and what keeps it lock-free for a concurrent caller.
func TestFMP4InitChangeNewASCFiresCodecUpdate(t *testing.T) {
	fastReload(t)
	// The two segments carry a DIFFERENT number of access units, so firing the
	// callback too early (0 frames before) and too late (all 5) are both failures
	// rather than one of them being indistinguishable from correct.
	s0, s1 := fmp4Samples(2, 40), fmp4Samples(3, 44)
	segs, v1, v2 := twoInitFixture(buildInitSegment(altASC, 44100, trackInit2), s0, s1)
	srv := twoPhaseServer(segs, v1, v2).start(t)

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
	// under the old config and must precede the update; the second segment's must
	// follow it.
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

// TestFMP4InitChangeWithoutCallbackEndsStream pins the safety rule behind the
// feature. Format().Codec is fixed at Open and audiostream.Source has no Format
// method at all, so Config.OnCodecUpdate is the ONLY channel by which a new
// AudioSpecificConfig can reach a consumer. With no callback registered, playing
// on would feed access units encoded under the new configuration to a decoder
// configured for the old one, silently and indefinitely. The stream ends instead,
// with a retryable cause, so a supervisor reconnects and re-resolves the
// configuration from the new init exactly as it did before this path existed.
func TestFMP4InitChangeWithoutCallbackEndsStream(t *testing.T) {
	fastReload(t)
	s0, s1 := fmp4Samples(2, 40), fmp4Samples(2, 44)
	segs, v1, v2 := twoInitFixture(buildInitSegment(altASC, 44100, trackInit2), s0, s1)
	srv := twoPhaseServer(segs, v1, v2).start(t)

	tl := &timeline{} // OnFrame only: OnCodecUpdate is deliberately left nil.
	c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8", OnFrame: tl.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrUnsupportedPlaylist) {
		t.Fatalf("Wait = %v, want ErrUnsupportedPlaylist (a codec change with nowhere to report it)", werr)
	}
	// The first segment, under the configuration Open resolved, is still delivered
	// in full: the stream ends at the change, not before it.
	if tl.frameCount() != len(s0) {
		t.Errorf("delivered %d AUs before the refusal, want %d", tl.frameCount(), len(s0))
	}
}

// TestFMP4RepeatedInitChangesFireEachUpdate drives three configurations across
// successive reloads. The snapshot comparison must hold up repeatedly: one update
// per genuine change, each carrying the config that took effect, and none for a
// segment reusing a config already in effect.
//
// The last playlist adds TWO segments under the third init, which is what makes a
// missing c.initURI update observable: the client would then treat each of them as
// a fresh init change and re-fetch the same initialization segment. The ASC
// comparison suppresses the duplicate callbacks, so the repeated work is visible
// only in the per-path request count, which is asserted here.
func TestFMP4RepeatedInitChangesFireEachUpdate(t *testing.T) {
	fastReload(t)
	s0, s1 := fmp4Samples(1, 40), fmp4Samples(1, 44)
	s2, s3 := fmp4Samples(1, 48), fmp4Samples(1, 52)
	segs := map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, trackInit1),
		initURL2: buildInitSegment(altASC, 44100, trackInit2),
		initURL3: buildInitSegment(thirdASC, 44100, trackInit3),
		fragURL0: buildFragment(trackInit1, s0, 1024),
		fragURL1: buildFragment(trackInit2, s1, 1024),
		fragURL2: buildFragment(trackInit3, s2, 1024),
		fragURL3: buildFragment(trackInit3, s3, 1024),
	}
	v1 := buildFMP4MediaPlaylist(1, 0, false, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
	v2 := buildFMP4MediaPlaylist(1, 0, false, initRel2, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
	})
	v3 := buildFMP4MediaPlaylist(1, 0, true, initRel3, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
		{uri: fragRel2, duration: 1.0}, {uri: fragRel3, duration: 1.0},
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

	tl := &timeline{mutateOnUpdate: true}
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
	// The callback zeroed each slice it was handed (mutateOnUpdate). The second
	// update still carrying the right bytes proves the source handed out a copy
	// rather than a slice its own comparison snapshot still points at.
	if !bytes.Equal(ups[1], thirdASC) {
		t.Errorf("second update ASC = %x, want %x (a consumer mutating the slice it was given must not corrupt the source)",
			ups[1], thirdASC)
	}
	if want := len(s0) + len(s1) + len(s2) + len(s3); tl.frameCount() != want {
		t.Errorf("delivered %d AUs across two changes, want %d", tl.frameCount(), want)
	}
	// Each initialization segment is fetched exactly once. A client that failed to
	// record the init now in effect would re-fetch it for every following segment.
	for _, u := range []string{initURL, initURL2, initURL3} {
		if n := h.requestCount(u); n != 1 {
			t.Errorf("%s fetched %d times, want exactly 1", u, n)
		}
	}
}

// TestFMP4InitChangeFetchFailureEndsStream covers a replacement init that cannot
// be fetched at all: the EXT-X-MAP points at an origin that is closed, so the GET
// fails at the transport. The stream must end with that cause rather than continue
// demuxing the new fragments with a demuxer built for the old init.
//
// The unreachable origin is a real closed listener rather than an absent path on
// the fixture server, because that server answers any unmapped path with a
// playlist body and HTTP 200: a "missing" init would be fetched successfully and
// fail later as a parse error, which is a different branch (the one
// TestFMP4InitChangeMalformedEndsStream covers).
func TestFMP4InitChangeFetchFailureEndsStream(t *testing.T) {
	fastReload(t)
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing listens here now, so the init GET cannot connect

	s0, s1 := fmp4Samples(2, 40), fmp4Samples(2, 44)
	segs := map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, trackInit1),
		fragURL0: buildFragment(trackInit1, s0, 1024),
		fragURL1: buildFragment(trackInit2, s1, 1024),
	}
	v1 := buildFMP4MediaPlaylist(1, 0, false, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
	// An absolute EXT-X-MAP URI survives resolution against the playlist base.
	v2 := buildFMP4MediaPlaylist(1, 0, true, deadURL+initURL2, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
	})
	srv := twoPhaseServer(segs, v1, v2).start(t)

	tl := &timeline{}
	c, err := Open(context.Background(), Config{
		URL:           srv.URL + "/live.m3u8",
		OnFrame:       tl.onFrame,
		OnCodecUpdate: tl.onCodecUpdate,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrConnectionClosed) {
		t.Fatalf("Wait = %v, want ErrConnectionClosed (the replacement init could not be fetched)", werr)
	}
	// The failure came from the replacement-init path, not from anything earlier:
	// the first segment was delivered in full first.
	if tl.frameCount() != len(s0) {
		t.Errorf("delivered %d AUs before the init fetch failed, want %d", tl.frameCount(), len(s0))
	}
	if ups := tl.codecUpdates(); len(ups) != 0 {
		t.Errorf("OnCodecUpdate fired %d times for an init that never loaded, want 0", len(ups))
	}
}

// TestFMP4InitChangeUnsupportedCodecEndsStream covers a replacement init whose
// audio sample entry is encrypted: the stream ends with ErrUnsupportedCodec, the
// same verdict Open would give for that init, rather than the demuxer being left
// on the old configuration.
func TestFMP4InitChangeUnsupportedCodecEndsStream(t *testing.T) {
	fastReload(t)
	s0, s1 := fmp4Samples(2, 40), fmp4Samples(2, 44)
	segs, v1, v2 := twoInitFixture(buildEncryptedInitSegment(wantASC, 44100, trackInit2), s0, s1)
	srv := twoPhaseServer(segs, v1, v2).start(t)

	tl := &timeline{}
	c, err := Open(context.Background(), Config{
		URL:           srv.URL + "/live.m3u8",
		OnFrame:       tl.onFrame,
		OnCodecUpdate: tl.onCodecUpdate,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrUnsupportedCodec) {
		t.Fatalf("Wait = %v, want ErrUnsupportedCodec (encrypted replacement init)", werr)
	}
	if tl.frameCount() != len(s0) {
		t.Errorf("delivered %d AUs before the refusal, want %d", tl.frameCount(), len(s0))
	}
}

// TestFMP4InitChangeMalformedEndsStream covers a replacement init that is not a
// parseable initialization segment at all.
func TestFMP4InitChangeMalformedEndsStream(t *testing.T) {
	fastReload(t)
	s0, s1 := fmp4Samples(2, 40), fmp4Samples(2, 44)
	segs, v1, v2 := twoInitFixture(make([]byte, 64), s0, s1) // 64 zero bytes: no ftyp/moov
	srv := twoPhaseServer(segs, v1, v2).start(t)

	tl := &timeline{}
	c, err := Open(context.Background(), Config{
		URL:           srv.URL + "/live.m3u8",
		OnFrame:       tl.onFrame,
		OnCodecUpdate: tl.onCodecUpdate,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, ErrMalformedSegment) {
		t.Fatalf("Wait = %v, want ErrMalformedSegment (unparseable replacement init)", werr)
	}
	if tl.frameCount() != len(s0) {
		t.Errorf("delivered %d AUs before the refusal, want %d", tl.frameCount(), len(s0))
	}
}

// TestFMP4ContainerSwitchIsUnsupported pins the boundary the re-initialization
// deliberately stops at: replacing one fMP4 init with another is played, but a
// stream that ADDS or DROPS EXT-X-MAP mid-stream is switching demuxer families
// (MPEG-TS and fMP4 have different framing semantics and resolve their
// configuration at different times), which stays ErrUnsupportedPlaylist.
func TestFMP4ContainerSwitchIsUnsupported(t *testing.T) {
	t.Run("fmp4 to ts", func(t *testing.T) {
		fastReload(t)
		s0 := fmp4Samples(2, 40)
		tsStream, _ := adtsStream(2, 40)
		segs := map[string][]byte{
			initURL:   buildInitSegment(wantASC, 44100, trackInit1),
			fragURL0:  buildFragment(trackInit1, s0, 1024),
			tsSegURL0: buildTSSegment(tsStream, 0x1000, 0x0100),
		}
		v1 := buildFMP4MediaPlaylist(1, 0, false, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
		// No EXT-X-MAP at all on the reload: the second segment is plain MPEG-TS.
		v2 := buildMediaPlaylist(1, 0, true, []segSpec{
			{uri: fragRel0, duration: 1.0}, {uri: tsSegRel0, duration: 1.0},
		})
		srv := twoPhaseServer(segs, v1, v2).start(t)
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
			initURL:   buildInitSegment(wantASC, 44100, trackInit1),
			fragURL1:  buildFragment(trackInit1, s1, 1024),
		}
		v1 := buildMediaPlaylist(1, 0, false, []segSpec{{uri: tsSegRel0, duration: 1.0}})
		v2 := buildFMP4MediaPlaylist(1, 0, true, initRel, []segSpec{
			{uri: tsSegRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
		})
		srv := twoPhaseServer(segs, v1, v2).start(t)
		c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8"})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if werr := c.Wait(context.Background()); !errors.Is(werr, ErrUnsupportedPlaylist) {
			t.Fatalf("Wait = %v, want ErrUnsupportedPlaylist (TS to fMP4)", werr)
		}
	})
}

// TestFMP4InitChangeCarriesRetiredDemuxerGapCount guards the malformed counter
// across a demuxer swap. A replacement demuxer starts its own gap count at zero,
// so publishing it verbatim would make Stats().Malformed jump backwards; the
// retired demuxer's total is carried instead.
//
// The first segment is truncated so there is a real gap to lose, and the stream
// then swaps demuxers TWICE. Two swaps are what separate accumulating the retired
// counts from merely assigning the latest one: under an assignment the second swap
// discards the first demuxer's gap and the total reads 0. The exact expected value
// is asserted, so an inflated or double-counted total fails as well.
func TestFMP4InitChangeCarriesRetiredDemuxerGapCount(t *testing.T) {
	fastReload(t)
	// Two samples with the mdat truncated so the SECOND overruns: one access unit
	// is still delivered (Open needs one to resolve the ASC) and exactly one gap is
	// counted on the first demuxer.
	s0 := fmp4Samples(2, 30)
	shortFrag := buildFragment(trackInit1, s0, 1024)
	shortFrag = shortFrag[:len(shortFrag)-20]
	s1, s2 := fmp4Samples(2, 44), fmp4Samples(2, 48)
	segs := map[string][]byte{
		initURL:  buildInitSegment(wantASC, 44100, trackInit1),
		initURL2: buildInitSegment(altASC, 44100, trackInit2),
		initURL3: buildInitSegment(thirdASC, 44100, trackInit3),
		fragURL0: shortFrag,
		fragURL1: buildFragment(trackInit2, s1, 1024),
		fragURL2: buildFragment(trackInit3, s2, 1024),
	}
	v1 := buildFMP4MediaPlaylist(1, 0, false, initRel, []segSpec{{uri: fragRel0, duration: 1.0}})
	v2 := buildFMP4MediaPlaylist(1, 0, false, initRel2, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0},
	})
	v3 := buildFMP4MediaPlaylist(1, 0, true, initRel3, []segSpec{
		{uri: fragRel0, duration: 1.0}, {uri: fragRel1, duration: 1.0}, {uri: fragRel2, duration: 1.0},
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
	if ups := tl.codecUpdates(); len(ups) != 2 {
		t.Fatalf("OnCodecUpdate fired %d times, want 2 (the fixture must swap demuxers twice)", len(ups))
	}
	const want = 1 // the single gap on the first demuxer, retired over twice
	if got := c.Stats().Tracks[0].Malformed; got != want {
		t.Errorf("Malformed = %d after two init changes, want %d: 0 means a retired demuxer's gap was dropped, "+
			"more means it was counted more than once", got, want)
	}
}
