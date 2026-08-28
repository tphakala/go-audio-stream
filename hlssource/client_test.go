package hlssource

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// Repeated segment paths, factored to satisfy goconst.
const (
	segURL  = "/s.ts"
	segURL0 = "/s0.ts"
	segURL1 = "/s1.ts"
	segRel0 = "s0.ts"
	segRel1 = "s1.ts"

	// tsSegURL0 and tsSegRel0 name a second MPEG-TS segment path, distinct from
	// segURL0 so a fixture map can hold both without collision.
	tsSegURL0 = "/seg0.ts"
	tsSegRel0 = "seg0.ts"
)

// fastReload shrinks the live-reload cadence for deterministic tests and returns
// a cleanup that restores the production cadence.
func fastReload(t *testing.T) {
	t.Helper()
	prev := reloadDelayFor
	reloadDelayFor = func(_ time.Duration, _ bool) time.Duration { return 5 * time.Millisecond }
	t.Cleanup(func() { reloadDelayFor = prev })
}

// hlsServer is an in-process HLS origin. It serves segments from a fixed map and
// a playlist from a function of the reload count, so a live test can evolve it.
type hlsServer struct {
	mu       sync.Mutex
	segments map[string][]byte
	playlist func(reloadN int) (body string, status int)
	reloads  int
	// requests counts every request by path, so a test can assert how many times
	// a given segment or initialization segment was actually fetched. A count is
	// the only way to observe work the client repeats needlessly, since a
	// redundant re-fetch is invisible in the delivered frames.
	requests map[string]int
}

// requestCount reports how many times path has been requested.
func (h *hlsServer) requestCount(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.requests[path]
}

func (h *hlsServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		if h.requests == nil {
			h.requests = make(map[string]int)
		}
		h.requests[r.URL.Path]++
		h.mu.Unlock()
		if body, ok := h.segment(r.URL.Path); ok {
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(body)
			return
		}
		h.mu.Lock()
		h.reloads++
		n := h.reloads
		fn := h.playlist
		h.mu.Unlock()
		body, status := fn(n)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *hlsServer) segment(path string) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b, ok := h.segments[path]
	return b, ok
}

// collector accumulates delivered frames for assertions after Wait.
type collector struct {
	mu     sync.Mutex
	frames []audiostream.Frame
	datas  [][]byte
}

//nolint:gocritic // OnFrame is func(audiostream.Frame); the value parameter is required by that signature.
func (c *collector) onFrame(f audiostream.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.datas = append(c.datas, append([]byte(nil), f.Data...))
	c.frames = append(c.frames, f)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// vodServer builds a server hosting a VOD playlist referencing segs (path ->
// TS bytes) in order with an ENDLIST.
func vodServer(t *testing.T, order []string, segs map[string][]byte) *httptest.Server {
	t.Helper()
	specs := make([]segSpec, len(order))
	for i, p := range order {
		specs[i] = segSpec{uri: p[1:], duration: 1.0} // strip leading "/" for a relative URI
	}
	body := buildMediaPlaylist(1, 0, true, specs)
	h := &hlsServer{
		segments: segs,
		playlist: func(int) (string, int) { return body, http.StatusOK },
	}
	return h.start(t)
}

func TestOpenResolvesASC(t *testing.T) {
	stream, _ := adtsStream(3, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	srv := vodServer(t, []string{tsSegURL0}, map[string][]byte{tsSegURL0: seg})
	c, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	f := c.Format()
	aac, ok := f.Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("codec = %T, want CodecAAC", f.Codec)
	}
	if !bytes.Equal(aac.AudioSpecificConfig, wantASC) {
		t.Errorf("ASC = %x, want %x", aac.AudioSpecificConfig, wantASC)
	}
	if f.Kind != audiostream.KindCompressed || f.SampleRate != 0 || f.Channels != 0 {
		t.Errorf("format = %+v, want KindCompressed with zero rate/channels", f)
	}
}

func TestVODDeliversAllSegmentsThenEnds(t *testing.T) {
	var wantAUs [][]byte
	segs := map[string][]byte{}
	order := []string{segURL0, segURL1, "/s2.ts"}
	for i, p := range order {
		stream, aus := adtsStream(2, 40+i) // distinct payload lengths per segment
		segs[p] = buildTSSegment(stream, 0x1000, 0x0100)
		wantAUs = append(wantAUs, aus...)
	}
	srv := vodServer(t, order, segs)
	col := &collector{}
	c, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(context.Background()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(wantAUs) {
		t.Fatalf("delivered %d AUs, want %d", col.count(), len(wantAUs))
	}
	// Byte identity end to end: the delivered AUs equal the fixture's, in order,
	// across the pending-first-segment and streamed-remainder boundary.
	for i := range wantAUs {
		if !bytes.Equal(col.datas[i], wantAUs[i]) {
			t.Errorf("AU %d bytes mismatch end to end", i)
		}
	}
	// PTS strictly increasing from 0.
	var prev time.Duration = -1
	for i, f := range col.frames {
		if f.PTS <= prev && i > 0 {
			t.Errorf("frame %d PTS %v not increasing (prev %v)", i, f.PTS, prev)
		}
		prev = f.PTS
	}
	if col.frames[0].PTS != 0 {
		t.Errorf("first PTS = %v, want 0", col.frames[0].PTS)
	}
	st := c.Stats().Tracks[0]
	if st.Packets != uint64(len(wantAUs)) {
		t.Errorf("Stats packets = %d, want %d", st.Packets, len(wantAUs))
	}
}

func TestMasterSelectsMediaPlaylist(t *testing.T) {
	stream, aus := adtsStream(2, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	mediaBody := buildMediaPlaylist(1, 0, true, []segSpec{{uri: "seg.ts", duration: 1.0}})
	master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=128000\nmedia.m3u8\n"
	h := &hlsServer{
		segments: map[string][]byte{"/seg.ts": seg},
		playlist: func(n int) (string, int) {
			// First request is the master, subsequent are the media playlist.
			if n == 1 {
				return master, http.StatusOK
			}
			return mediaBody, http.StatusOK
		},
	}
	srv := h.start(t)
	col := &collector{}
	c, err := Open(context.Background(), Config{URL: srv.URL + "/master.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(context.Background()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(aus) {
		t.Errorf("delivered %d AUs via master, want %d", col.count(), len(aus))
	}
}

func TestLiveReloadDeliversNewSegments(t *testing.T) {
	fastReload(t)
	s0, au0 := adtsStream(2, 40)
	s1, au1 := adtsStream(2, 45)
	segs := map[string][]byte{
		segURL0: buildTSSegment(s0, 0x1000, 0x0100),
		segURL1: buildTSSegment(s1, 0x1000, 0x0100),
	}
	v1 := buildMediaPlaylist(1, 0, false, []segSpec{{uri: segRel0, duration: 1.0}})
	v2 := buildMediaPlaylist(1, 0, true, []segSpec{
		{uri: segRel0, duration: 1.0}, {uri: segRel1, duration: 1.0},
	})
	h := &hlsServer{
		segments: segs,
		playlist: func(n int) (string, int) {
			if n == 1 {
				return v1, http.StatusOK
			}
			return v2, http.StatusOK // reload gains s1 and ENDLIST
		},
	}
	srv := h.start(t)
	col := &collector{}
	c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(context.Background()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	want := len(au0) + len(au1)
	if col.count() != want {
		t.Fatalf("delivered %d AUs across reload, want %d", col.count(), want)
	}
}

func TestExtXGapAdvancesAndSignalsLoss(t *testing.T) {
	s0, au0 := adtsStream(2, 40)
	s2, au2 := adtsStream(2, 50)
	segs := map[string][]byte{
		segURL0:  buildTSSegment(s0, 0x1000, 0x0100),
		"/s2.ts": buildTSSegment(s2, 0x1000, 0x0100),
	}
	body := buildMediaPlaylist(1, 0, true, []segSpec{
		{uri: segRel0, duration: 1.0},
		{uri: "gap.ts", duration: 1.0, gap: true},
		{uri: "s2.ts", duration: 1.0},
	})
	h := &hlsServer{segments: segs, playlist: func(int) (string, int) { return body, http.StatusOK }}
	srv := h.start(t)
	col := &collector{}
	c, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(context.Background()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	if col.count() != len(au0)+len(au2) {
		t.Fatalf("delivered %d AUs, want %d (gap not fetched)", col.count(), len(au0)+len(au2))
	}
	// The first frame after the gap must carry the loss as SeqGap > 0.
	firstAfterGap := col.frames[len(au0)]
	if firstAfterGap.SeqGap == 0 {
		t.Error("frame after gap has SeqGap 0, want > 0")
	}
}

func TestNilOnFrameStillCounts(t *testing.T) {
	stream, aus := adtsStream(3, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	srv := vodServer(t, []string{segURL}, map[string][]byte{segURL: seg})
	c, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = c.Wait(context.Background())
	if got := c.Stats().Tracks[0].Packets; got != uint64(len(aus)) {
		t.Errorf("Stats packets with nil OnFrame = %d, want %d", got, len(aus))
	}
}

func TestCloseFromInsideOnFrame(t *testing.T) {
	stream, _ := adtsStream(8, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	srv := vodServer(t, []string{segURL}, map[string][]byte{segURL: seg})
	var c *Client
	// started gates the callback until c is assigned, so reading c inside OnFrame
	// (on the reader goroutine) happens-after the assignment: no data race.
	started := make(chan struct{})
	closed := make(chan struct{})
	var once sync.Once
	cfg := Config{URL: srv.URL + "/vod.m3u8", OnFrame: func(audiostream.Frame) {
		<-started
		once.Do(func() { _ = c.Close(); close(closed) })
	}}
	var err error
	c, err = Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	close(started)
	<-closed
	if werr := c.Wait(context.Background()); werr == nil {
		t.Error("Wait returned nil after Close from OnFrame")
	}
}

func TestWatchdogFiresOnStalledSegment(t *testing.T) {
	stream, _ := adtsStream(2, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	// The playlist references a second segment the server never answers (it
	// blocks), so no segment read completes and the read-idle watchdog fires.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	body := buildMediaPlaylist(1, 0, true, []segSpec{
		{uri: segRel0, duration: 1.0}, {uri: "stall.ts", duration: 1.0},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/s0.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			_, _ = w.Write(seg)
		case "/stall.ts":
			// Block until the client cancels the request (the watchdog firing) or
			// the test ends, so the server handler unblocks and Close can complete.
			select {
			case <-block:
			case <-r.Context().Done():
			}
		default:
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	c, err := Open(context.Background(), Config{
		URL:      srv.URL + "/vod.m3u8",
		ReadIdle: 100 * time.Millisecond,
		Timeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if werr := c.Wait(context.Background()); !errors.Is(werr, audiostream.ErrReadTimeout) {
		t.Errorf("Wait = %v, want ErrReadTimeout", werr)
	}
}

func TestLiveWindowDropSignalsSeqGap(t *testing.T) {
	fastReload(t)
	s0, au0 := adtsStream(2, 40)
	s1, au1 := adtsStream(2, 42)
	s5, _ := adtsStream(2, 44)
	s6, _ := adtsStream(2, 46)
	segs := map[string][]byte{
		"/s0.ts": buildTSSegment(s0, 0x1000, 0x0100),
		"/s1.ts": buildTSSegment(s1, 0x1000, 0x0100),
		"/s5.ts": buildTSSegment(s5, 0x1000, 0x0100),
		"/s6.ts": buildTSSegment(s6, 0x1000, 0x0100),
	}
	v1 := buildMediaPlaylist(1, 0, false, []segSpec{
		{uri: "s0.ts", duration: 1.0}, {uri: "s1.ts", duration: 1.0},
	})
	// The reload jumps MEDIA-SEQUENCE to 5: segments 2,3,4 fell out of the window.
	v2 := buildMediaPlaylist(1, 5, true, []segSpec{
		{uri: "s5.ts", duration: 1.0}, {uri: "s6.ts", duration: 1.0},
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
	c, err := Open(context.Background(), Config{URL: srv.URL + "/live.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(context.Background()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	// The first frame of s5 (index after s0+s1's AUs) reports the 3 dropped
	// segments (seq 2,3,4) as SeqGap; every other frame reports 0.
	firstAfterDrop := len(au0) + len(au1)
	if col.count() <= firstAfterDrop {
		t.Fatalf("delivered %d frames, expected the post-drop segment too", col.count())
	}
	if got := col.frames[firstAfterDrop].SeqGap; got != 3 {
		t.Errorf("SeqGap at the resume frame = %d, want 3", got)
	}
	for i, f := range col.frames {
		if i != firstAfterDrop && f.SeqGap != 0 {
			t.Errorf("frame %d SeqGap = %d, want 0", i, f.SeqGap)
		}
	}
}

func TestReloadDelayCadence(t *testing.T) {
	// A reload that delivered new segments waits a full target duration; one that
	// did not waits half; a non-positive target falls back to DefaultTimeout.
	if got := reloadDelayFor(10*time.Second, true); got != 10*time.Second {
		t.Errorf("delivered cadence = %v, want 10s", got)
	}
	if got := reloadDelayFor(10*time.Second, false); got != 5*time.Second {
		t.Errorf("unchanged cadence = %v, want 5s", got)
	}
	if got := reloadDelayFor(0, true); got != DefaultTimeout {
		t.Errorf("zero-target delivered = %v, want DefaultTimeout", got)
	}
	if got := reloadDelayFor(0, false); got != DefaultTimeout/2 {
		t.Errorf("zero-target unchanged = %v, want DefaultTimeout/2", got)
	}
}

func TestCredentialsAttachedSameHostStrippedOnCrossHostRedirect(t *testing.T) {
	stream, _ := adtsStream(2, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	var originGotAuth, cdnGotAuth atomic.Bool
	// The CDN (a different host:port) serves the real segment and records whether
	// it received an Authorization header.
	cdn := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			cdnGotAuth.Store(true)
		}
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(seg)
	}))
	t.Cleanup(cdn.Close)
	// The origin serves the playlist (recording auth) and redirects the segment
	// request cross-host to the CDN.
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			originGotAuth.Store(true)
		}
		switch r.URL.Path {
		case "/seg.ts":
			http.Redirect(w, r, cdn.URL+"/real.ts", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte(buildMediaPlaylist(1, 0, true, []segSpec{{uri: "seg.ts", duration: 1.0}})))
		}
	}))
	t.Cleanup(origin.Close)

	c, err := Open(context.Background(), Config{
		URL:         origin.URL + "/vod.m3u8",
		Username:    "u",
		Password:    "p",
		InsecureTLS: true,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	if !originGotAuth.Load() {
		t.Error("origin (same host, https) did not receive Basic credentials")
	}
	if cdnGotAuth.Load() {
		t.Error("credentials leaked to the CDN across a cross-host redirect")
	}
}

func TestOpenInvalidURL(t *testing.T) {
	for _, u := range []string{"", "ftp://h/x.m3u8", "http://h:99999/x.m3u8"} {
		if _, err := Open(context.Background(), Config{URL: u}); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("Open(%q) = %v, want ErrInvalidURL", u, err)
		}
	}
}

func TestOpenBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	_, err := Open(context.Background(), Config{URL: srv.URL + "/missing.m3u8"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Errorf("Open = %v, want *StatusError 404", err)
	}
	if !errors.Is(err, ErrBadStatus) {
		t.Errorf("Open error does not match ErrBadStatus: %v", err)
	}
}

func TestOpenMalformedPlaylist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not a playlist"))
	}))
	t.Cleanup(srv.Close)
	if _, err := Open(context.Background(), Config{URL: srv.URL + "/x.m3u8"}); !errors.Is(err, ErrMalformedPlaylist) {
		t.Errorf("Open = %v, want ErrMalformedPlaylist", err)
	}
}

func TestOpenPlaylistTooLarge(t *testing.T) {
	big := make([]byte, 1024)
	for i := range big {
		big[i] = 'a'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	t.Cleanup(srv.Close)
	_, err := Open(context.Background(), Config{URL: srv.URL + "/x.m3u8", MaxPlaylistBytes: 256})
	if !errors.Is(err, ErrPlaylistTooLarge) {
		t.Errorf("Open = %v, want ErrPlaylistTooLarge", err)
	}
}

func TestOpenSegmentTooLarge(t *testing.T) {
	stream, _ := adtsStream(3, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	srv := vodServer(t, []string{segURL}, map[string][]byte{segURL: seg})
	_, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8", MaxSegmentBytes: 100})
	if !errors.Is(err, ErrSegmentTooLarge) {
		t.Errorf("Open = %v, want ErrSegmentTooLarge", err)
	}
}

func TestInfoStripsCredentials(t *testing.T) {
	srv := vodServer(t, []string{segURL}, map[string][]byte{
		segURL: buildTSSegment(func() []byte { s, _ := adtsStream(2, 40); return s }(), 0x1000, 0x0100),
	})
	c, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8", Username: "u", Password: "p"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = c.Close() }()
	if got := c.Info().URL; got != srv.URL+"/vod.m3u8" {
		t.Errorf("Info().URL = %q, should carry no credentials", got)
	}
}

func TestOpenExpiredContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, Config{URL: "https://h/x.m3u8"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Open with cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestOpenUnsupportedCodecEndToEnd(t *testing.T) {
	// A segment whose PMT declares a non-AAC audio stream_type must surface
	// ErrUnsupportedCodec from Open, not a bare "no audio" verdict. This is the
	// client-level counterpart to the demux unit test: the error must propagate
	// through Open's first-segment handshake.
	for _, tc := range []struct {
		name       string
		streamType byte
	}{
		{"mp3-a", streamTypeMP3a},
		{"mp3-b", streamTypeMP3b},
		{"latm", streamTypeLATM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, _ := adtsStream(2, 40)
			seg := buildTSSegmentType(stream, 0x1000, 0x0100, tc.streamType)
			srv := vodServer(t, []string{segURL0}, map[string][]byte{segURL0: seg})
			if _, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8"}); !errors.Is(err, ErrUnsupportedCodec) {
				t.Fatalf("Open with stream_type %#x = %v, want ErrUnsupportedCodec", tc.streamType, err)
			}
		})
	}
}

func TestDiscontinuityDrivesDemuxResetEndToEnd(t *testing.T) {
	// A media playlist carrying EXT-X-DISCONTINUITY before its second segment must
	// drive a demux reset through the client: both segments' access units arrive,
	// in order and byte-identical, with strictly increasing PTS. The internal
	// framer swap is covered by the demux unit test; here we prove the tag threads
	// parse -> processSegment discontinuity flag -> demux end to end.
	s0, au0 := adtsStream(2, 40)
	s1, au1 := adtsStream(2, 48)
	segs := map[string][]byte{
		segURL0: buildTSSegment(s0, 0x1000, 0x0100),
		segURL1: buildTSSegment(s1, 0x1000, 0x0100),
	}
	body := buildMediaPlaylist(1, 0, true, []segSpec{
		{uri: segRel0, duration: 1.0},
		{uri: segRel1, duration: 1.0, discontinuity: true},
	})
	h := &hlsServer{
		segments: segs,
		playlist: func(int) (string, int) { return body, http.StatusOK },
	}
	srv := h.start(t)
	col := &collector{}
	c, err := Open(context.Background(), Config{URL: srv.URL + "/vod.m3u8", OnFrame: col.onFrame})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := c.Wait(context.Background()); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded", err)
	}
	want := append(append([][]byte{}, au0...), au1...)
	if col.count() != len(want) {
		t.Fatalf("delivered %d AUs across the discontinuity, want %d", col.count(), len(want))
	}
	for i := range want {
		if !bytes.Equal(col.datas[i], want[i]) {
			t.Errorf("AU %d bytes mismatch across the discontinuity", i)
		}
	}
	var prev time.Duration = -1
	for i, f := range col.frames {
		if i > 0 && f.PTS <= prev {
			t.Errorf("frame %d PTS %v not strictly increasing (prev %v)", i, f.PTS, prev)
		}
		prev = f.PTS
	}
}
