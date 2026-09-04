package hlssource

import (
	"errors"
	"net/http"
	"testing"
)

// A live playlist (no EXT-X-ENDLIST) whose only segment is EXT-X-GAP is a
// recoverable transient per RFC 8216, so Open must report the retryable
// ErrNoPlayableSegment and NOT the permanent-shaped ErrMalformedPlaylist.
func TestOpenLiveAllGapReturnsRetryable(t *testing.T) {
	body := buildMediaPlaylist(6, 0, false, []segSpec{
		{uri: segRel0, duration: 6.0, gap: true},
	})
	h := &hlsServer{playlist: func(int) (string, int) { return body, http.StatusOK }}
	srv := h.start(t)

	_, err := Open(t.Context(), Config{URL: srv.URL + "/live.m3u8"})
	if !errors.Is(err, ErrNoPlayableSegment) {
		t.Fatalf("Open err = %v, want ErrNoPlayableSegment", err)
	}
	if errors.Is(err, ErrMalformedPlaylist) {
		t.Errorf("Open err = %v, must NOT be ErrMalformedPlaylist for a live all-gap playlist", err)
	}
}

// A VOD playlist (EXT-X-ENDLIST) with no playable segment is terminal: no reload
// can ever add one, so Open must keep the permanent-shaped ErrMalformedPlaylist
// and must NOT return the retryable ErrNoPlayableSegment, which would spin a
// supervisor forever against a dead playlist.
func TestOpenVODAllGapReturnsMalformed(t *testing.T) {
	body := buildMediaPlaylist(6, 0, true, []segSpec{
		{uri: segRel0, duration: 6.0, gap: true},
	})
	h := &hlsServer{playlist: func(int) (string, int) { return body, http.StatusOK }}
	srv := h.start(t)

	_, err := Open(t.Context(), Config{URL: srv.URL + "/vod.m3u8"})
	if !errors.Is(err, ErrMalformedPlaylist) {
		t.Fatalf("Open err = %v, want ErrMalformedPlaylist", err)
	}
	if errors.Is(err, ErrNoPlayableSegment) {
		t.Errorf("Open err = %v, must NOT be ErrNoPlayableSegment for a terminal VOD playlist", err)
	}
}

// A live playlist that starts all-gap and later publishes a playable segment must
// recover: the first Open reports ErrNoPlayableSegment, and a subsequent Open
// (as a supervisor retry would drive) resolves cleanly once the reload brings a
// playable segment. This exercises the reason the cause is kept retryable.
func TestOpenLiveAllGapRecoversOnReload(t *testing.T) {
	stream, _ := adtsStream(3, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)

	allGap := buildMediaPlaylist(6, 0, false, []segSpec{
		{uri: tsSegRel0, duration: 6.0, gap: true},
	})
	playable := buildMediaPlaylist(6, 1, false, []segSpec{
		{uri: tsSegRel0, duration: 6.0},
	})
	h := &hlsServer{
		segments: map[string][]byte{tsSegURL0: seg},
		// The first playlist fetch is all-gap; every later fetch (a supervisor's
		// retry re-opens and re-fetches) carries a playable segment.
		playlist: func(n int) (string, int) {
			if n == 1 {
				return allGap, http.StatusOK
			}
			return playable, http.StatusOK
		},
	}
	srv := h.start(t)
	url := srv.URL + "/live.m3u8"

	if _, err := Open(t.Context(), Config{URL: url}); !errors.Is(err, ErrNoPlayableSegment) {
		t.Fatalf("first Open err = %v, want ErrNoPlayableSegment", err)
	}
	c, err := Open(t.Context(), Config{URL: url})
	if err != nil {
		t.Fatalf("second Open err = %v, want success after the playlist gained a playable segment", err)
	}
	_ = c.Close()
	// Join the reader goroutine before returning. Close only signals shutdown; it
	// does not wait. Without this the reader can outlive the test and race a later
	// test's fastReload swap of the package-global reloadDelayFor under -race.
	_ = c.Wait(t.Context())
}
