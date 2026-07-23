package rtsp

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// playRange is the default PLAY Range. RFC 2326 defaults to the whole
// presentation, but some cameras misbehave without an explicit npt=0.000-,
// so it is always sent.
const playRange = "npt=0.000-"

// Play issues a PLAY for the whole presentation, seeds each track's timestamp
// origin from the response RTP-Info when present and plausible, transitions to
// the playing state, and starts the keepalive/RTCP timer goroutine. It is
// legal only in the setup state, else it returns a *StateError. A clean
// non-2xx protocol response is returned without tearing down the session; a
// transport failure is returned already funneled into shutdown.
//
// It holds lifecycleMu for the whole call, as Describe and Setup do, and for
// the same reason: the state check and the commit straddle a network round
// trip, so two concurrent Plays would both pass the check and each start a
// keepalive goroutine, doubling the RTSP keepalives and the Receiver Reports
// for the rest of the session.
func (c *Client) Play(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.mu.Lock()
	if serr := c.requireState(methodPlay); serr != nil {
		c.mu.Unlock()
		return serr
	}
	reqURL := c.baseURL
	c.mu.Unlock()

	req := &Request{Method: methodPlay, URL: reqURL, Header: Header{}}
	req.Header.Set("Range", playRange)
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}

	c.seedFromRTPInfo(resp.Header.Get("RTP-Info"))

	// Store the watchdog origin before taking mu so the reader (which may
	// already be delivering early frames) never blocks on the lifecycle lock.
	c.lastFrameAt.Store(time.Now().UnixNano())

	// Take mu for the transition and the timer launch together. wg.Add(1) runs
	// under mu only after confirming shutdown has not begun, so it can never
	// race the reader's single wg.Wait() (the WaitGroup discipline).
	c.mu.Lock()
	if c.termErr != nil {
		terr := c.termErr
		c.mu.Unlock()
		return terr
	}
	c.commitState(methodPlay)
	c.playing.Store(true)
	c.wg.Add(1)
	c.mu.Unlock()

	// Arm the read-idle watchdog now that playing is set: the reader may be
	// parked in a deadline-less pre-play read, so re-arm the deadline here to
	// lastFrameAt+ReadIdle. SetReadDeadline applies to the pending read, so the
	// watchdog fires even when no frame ever arrives. A no-op when ReadIdle is 0.
	c.armReadDeadline()

	go c.keepaliveLoop()
	return nil
}

// seededOrigin returns the frame-delivery baseline for a track's first frame.
// The RTP-Info seed applies only to the very first baseline (tr.baselineFixed
// is still false): with a plausible seed (seed <= firstTS, so PTS never goes
// negative) the advertised origin is used, otherwise the first packet's
// timestamp is the baseline. Once a baseline has ever been fixed, the seed is
// permanently ignored (decision 6), so a later SSRC reset re-baselines cleanly
// from its first packet rather than re-applying the stale origin.
func (c *Client) seededOrigin(tr *track, firstTS uint64) uint64 {
	if !tr.baselineFixed && tr.hasSeed.Load() {
		if seed := tr.seed.Load(); seed <= firstTS {
			return seed
		}
	}
	return firstTS
}

// seedFromRTPInfo parses an RTP-Info header and records the timestamp origin
// for each matched track. Only the origin is seeded, never the sequence
// (decision 6). An absent or unparseable header is ignored, leaving the
// first-packet baseline. The seed is published atomically because the reader
// may already be delivering early frames on the caller's goroutine.
func (c *Client) seedFromRTPInfo(header string) {
	if header == "" {
		return
	}
	c.mu.Lock()
	tracks := c.tracks
	base := c.baseURL
	c.mu.Unlock()
	if len(tracks) == 0 {
		return
	}
	for i, entry := range strings.Split(header, ",") {
		url, _, rtptime, ok := parseRTPInfoEntry(entry)
		if !ok {
			continue
		}
		tr := matchTrack(tracks, base, url, i)
		if tr == nil {
			continue
		}
		tr.seed.Store(rtptime) // store the value before flagging it present.
		tr.hasSeed.Store(true)
	}
}

// parseRTPInfoEntry parses one RTP-Info entry (url=...;seq=...;rtptime=...).
// ok is true only when both seq and rtptime are present and parse as unsigned
// 32-bit integers (the plausibility test); rtptime is returned widened to
// uint64. The seq value itself is unused (sequence is never seeded), only its
// presence gates plausibility.
func parseRTPInfoEntry(entry string) (url string, seq, rtptime uint64, ok bool) {
	var haveSeq, haveTime bool
	for _, part := range strings.Split(entry, ";") {
		name, val, has := strings.Cut(strings.TrimSpace(part), "=")
		if !has {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "url":
			url = val
		case "seq":
			if n, err := strconv.ParseUint(val, 10, 32); err == nil {
				seq, haveSeq = n, true
			}
		case "rtptime":
			if n, err := strconv.ParseUint(val, 10, 32); err == nil {
				rtptime, haveTime = n, true
			}
		}
	}
	return url, seq, rtptime, haveSeq && haveTime
}

// matchTrack maps an RTP-Info entry to a set-up track by resolved-control
// equality, falling back to positional order (the entry's index across the
// set-up tracks) when the url is absent or matches no track.
func matchTrack(tracks []*track, base, rawURL string, pos int) *track {
	if rawURL != "" {
		resolved := rawURL
		if r, err := ResolveControlURL(base, rawURL); err == nil {
			resolved = r
		}
		for _, tr := range tracks {
			if tr.control == resolved || tr.control == rawURL {
				return tr
			}
		}
	}
	if pos >= 0 && pos < len(tracks) {
		return tracks[pos]
	}
	return nil
}
