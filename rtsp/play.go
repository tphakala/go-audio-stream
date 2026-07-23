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

	// Stamp the watchdog origin before playing is set. armReadDeadline derives
	// the read deadline from this value, so a zero here plus playing would mean
	// a 1970 deadline that expired decades ago, killing a healthy stream on its
	// first read.
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
// timestamp is the baseline. Once a baseline has ever been fixed the seed is
// permanently ignored, so a later SSRC reset re-baselines cleanly from its
// first packet rather than re-applying the stale origin.
//
// The seed is therefore best-effort by construction. Play stores it after the
// PLAY response returns, while the reader may already be routing frames that
// arrived in the same TCP segment as that response, so a server that starts
// streaming with its PLAY response can fix the baseline from a packet before
// the seed lands. Both outcomes are correct baselines; only the origin differs,
// and the first delivered frame winning is what makes the rule simple.
func (c *Client) seededOrigin(tr *track, firstTS uint64) uint64 {
	if !tr.baselineFixed && tr.hasSeed.Load() {
		if seed := tr.seed.Load(); seed <= firstTS {
			return seed
		}
	}
	return firstTS
}

// seedFromRTPInfo parses an RTP-Info header and records the timestamp origin
// for each matched track. Only the origin is seeded, never the sequence: a
// server's advertised sequence number is not needed to interpret the stream,
// and trusting it would desynchronize loss accounting for no gain. An absent or
// unparseable header is ignored, leaving the first-packet baseline. The seed is published atomically because the reader
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
//
// A url carrying a control character is rejected outright. It is a header a
// remote server controls, and it flows into URL resolution and into comparisons
// against this client's own control URLs, so it has no business carrying CR, LF
// or NUL; the fuzz target asserts this.
func parseRTPInfoEntry(entry string) (url string, seq, rtptime uint64, ok bool) {
	var haveSeq, haveTime bool
	for part := range strings.SplitSeq(entry, ";") {
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
	if strings.ContainsAny(url, "\r\n\x00") {
		return "", 0, 0, false
	}
	return url, seq, rtptime, haveSeq && haveTime
}

// matchTrack maps an RTP-Info entry to a set-up track by resolved-control
// equality, falling back to positional order (the entry's index across the
// set-up tracks) only when the entry carries no url at all.
//
// A url that resolves to no set-up track yields no match rather than the
// positional one. Servers commonly report only their primary track, so an entry
// naming a track this client did not set up is a routine case, and treating it
// positionally would seed the wrong track: with video reported and only audio
// set up, the audio track would take the video stream's origin and every PTS
// would be wrong by the offset between the two clocks. Declining to seed costs
// nothing, because the first-packet baseline is already the correct fallback.
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
		return nil
	}
	if pos < len(tracks) {
		return tracks[pos]
	}
	return nil
}
