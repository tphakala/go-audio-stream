package rtsp

import (
	"context"
	"errors"
	"fmt"
)

var (
	// ErrUnknownTrack is returned by Setup when the Track did not come from
	// this client's most recent Describe: either its ID does not index the
	// retained descriptors, or its Control does not match the descriptor the
	// ID selects. Both matter because the ID chooses the depacketizer while
	// Control chooses the stream, so an ID and Control paired by hand would
	// set up one track and decode it as another for the life of the session.
	ErrUnknownTrack = errors.New("rtsp: track did not come from this client's Describe")

	// ErrTrackAlreadySetUp is returned by a second Setup for a track that is
	// already set up. Allowing it would publish a second pipeline and a second
	// channel pair under one track ID, and per-track stats are keyed by that
	// ID, so one of the two would silently win.
	ErrTrackAlreadySetUp = errors.New("rtsp: track is already set up")

	// ErrNoChannelsLeft is returned by Setup when no interleaved channel pair
	// remains below maxInterleavedChannel. It is channel-space exhaustion
	// rather than a track count: a server that renumbers the first track to
	// 254-255 exhausts the space with one track set up.
	ErrNoChannelsLeft = errors.New("rtsp: no interleaved channel pair left")
)

// Setup issues a SETUP for one track over the TCP-interleaved profile,
// negotiates its interleaved channel pair, and constructs and publishes the
// track's depacketization pipeline into the channel routing table. It is legal
// only in the described or setup state, else it returns a *StateError.
//
// trk must be a Track returned by this client's most recent Describe. Anything
// else is ErrUnknownTrack, and a track already set up is ErrTrackAlreadySetUp.
//
// A clean protocol response the caller may recover from (a non-2xx
// *ResponseError, or *audiostream.RedirectError) is returned without tearing
// down the session, so the caller may Close or try another track. A 401 is
// answered and retried automatically, so it reaches the caller only as an error
// matching ErrAuthFailed, which wraps the challenge that was refused.
//
// A Transport this client rejects (ErrNoInterleaved, ErrBadChannelPair,
// ErrChannelConflict, ErrMalformedTransport) is not the same case. The server
// answered 2xx, so it has already allocated that stream and its channels, and
// this client neither records them nor tears that one track down. Prefer Close
// after such a rejection: continuing with another track risks the server
// streaming two tracks over one channel once PLAY starts the aggregate session.
//
// On success it transitions the client into the setup state.
//
// Concurrent Setup calls are outside the documented contract, but they
// serialize rather than corrupting the routing table.
//
//nolint:gocritic // Track is the documented public Setup signature; hugeParam does not apply to a per-track lifecycle call.
func (c *Client) Setup(ctx context.Context, trk Track, opts SetupOptions) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	desc, err := c.describedTrackFor(&trk)
	if err != nil {
		return err
	}
	rtpCh, rtcpCh, aerr := c.nextChannelPair()
	if aerr != nil {
		return aerr
	}

	req := &Request{Method: methodSetup, URL: trk.Control, Header: Header{}}
	req.Header.Set("Transport", BuildTransport(rtpCh, rtcpCh))
	resp, derr := c.do(ctx, req)
	if derr != nil {
		return derr
	}

	c.recordSession(trk.ID, ParseSession(resp.Header.Get("Session")))

	raw := resp.Header.Get("Transport")
	th, terr := ParseTransport(raw)
	if terr != nil {
		return fmt.Errorf("track %d: %w (Transport: %q)", trk.ID, terr, raw)
	}
	gotRTP, gotRTCP, cerr := th.InterleavedChannels(c.claimedChannels())
	if cerr != nil {
		return fmt.Errorf("track %d: %w (Transport: %q)", trk.ID, cerr, raw)
	}

	tr := newTrack(trk.ID, desc, opts, gotRTCP, c.cfg.Logger)
	return c.publishTrack(tr, gotRTP, gotRTCP)
}

// describedTrackFor checks the lifecycle gate and validates trk against the
// descriptors Describe retained, returning the descriptor the track's ID
// selects. The gate and the lookup share one critical section because both read
// mu-guarded state that a concurrent shutdown may change between them.
func (c *Client) describedTrackFor(trk *Track) (describedTrack, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if serr := c.requireState(methodSetup); serr != nil {
		return describedTrack{}, serr
	}
	if trk.ID < 0 || trk.ID >= len(c.described) {
		return describedTrack{}, fmt.Errorf("%w: id %d, Describe returned %d track(s)",
			ErrUnknownTrack, trk.ID, len(c.described))
	}
	desc := c.described[trk.ID]
	if trk.Control != desc.control {
		// The URLs are deliberately not interpolated: RedactURL masks userinfo
		// but not query parameters, and some firmware carries credentials
		// there. The id is enough to identify which Track was mispaired.
		return describedTrack{}, fmt.Errorf("%w: id %d does not carry the control URL Describe resolved for it",
			ErrUnknownTrack, trk.ID)
	}
	for _, p := range c.channelPairs {
		if p.TrackID == trk.ID {
			return describedTrack{}, fmt.Errorf("%w: id %d on channels %d-%d",
				ErrTrackAlreadySetUp, trk.ID, p.RTP, p.RTCP)
		}
	}
	return desc, nil
}

// nextChannelPair returns the interleaved pair to propose for the next track:
// the lowest even channel above every channel already claimed.
//
// It is derived from the channels actually claimed rather than from the number
// of tracks set up, because the server may renumber. Once it moves track 0 to
// 4-5, a proposal computed as 2*len(channelPairs) would offer 2-3 for track 1
// and then 4-5 for track 2, a pair this client already holds, so a server that
// honoured the proposal would have it rejected as the client's own
// ErrChannelConflict and that track could never be set up.
func (c *Client) nextChannelPair() (rtpCh, rtcpCh int, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	base := 0
	for _, p := range c.channelPairs {
		if p.RTP >= base {
			base = p.RTP + 1
		}
		if p.RTCP >= base {
			base = p.RTCP + 1
		}
	}
	if base%2 != 0 {
		base++
	}
	if base+1 > maxInterleavedChannel {
		return 0, 0, fmt.Errorf("%w: next pair would be %d-%d, above %d",
			ErrNoChannelsLeft, base, base+1, maxInterleavedChannel)
	}
	return base, base + 1, nil
}

// recordSession stores the session id and timeout from the first SETUP. A later
// SETUP that reports a different id is a server quirk: the first id governs the
// aggregate session and the mismatch is logged, not fatal.
//
// It deliberately does NOT carry publishTrack's terminal-error guard, even
// though the two are siblings that both write mu-guarded session state. That
// guard exists to stop a late success from resurrecting the state machine, and
// this function does not touch state: it writes only the session id and
// timeout. Refusing to record them during shutdown is actively harmful, because
// initiateShutdown sets the terminal error before the reader has unwound to its
// terminal sequence, so a SETUP response arriving in that window would be
// dropped and sessionEstablished would then report no session. The TEARDOWN
// would never be sent and the camera would hold the session until its own
// timeout, which is the failure this guard would look like it was preventing.
func (c *Client) recordSession(trackID int, sh SessionHeader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID != "" {
		if sh.ID != "" && sh.ID != c.sessionID {
			logWarn(c.cfg.Logger, "SETUP returned a session id different from the established one",
				"track", trackID)
		}
		return
	}
	if sh.ID == "" {
		// RFC 2326 requires a Session header on a SETUP response. Recording
		// ParseSession's defaulted timeout against an empty id would leave a
		// snapshot reading "no session, expires in 60s", and the absence is
		// what matters: no later verb can address the session and no TEARDOWN
		// can be sent, so the failure would surface at PLAY with no clue why.
		logWarn(c.cfg.Logger, "SETUP response carried no Session header", "track", trackID)
		return
	}
	c.sessionID = sh.ID
	c.sessionTimeout = sh.Timeout
}

// claimedChannels returns the set of interleaved channels already assigned to
// earlier tracks, so InterleavedChannels can reject an overlapping pair. It is
// nil before the first Setup.
func (c *Client) claimedChannels() map[int]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.channelPairs) == 0 {
		return nil
	}
	claimed := make(map[int]bool, 2*len(c.channelPairs))
	for _, p := range c.channelPairs {
		claimed[p.RTP] = true
		claimed[p.RTCP] = true
	}
	return claimed
}

// publishTrack atomically publishes tr into the channel routing table and
// records it under mu. The atomic store establishes the happens-before edge the
// reader relies on to route frames, so tr must be fully initialized before this
// call. When shutdown has already begun (the terminal error is set) it does not
// resurrect the state and returns that terminal error.
//
// The table is published only AFTER the SETUP response, so interleaved frames a
// server sends between the request and the response are routed to no track and
// dropped. That is deliberate. Pre-publishing the proposed pair would bind
// channels the server has not confirmed, and a server that renumbers (which is
// why nextChannelPair reads the claimed set rather than a count) would then have
// another track's packets fed to this track's depacketizer for as long as the
// stale binding stood. Losing frames a server should not have sent before PLAY
// costs a few pre-roll packets; mis-binding costs the session. Frames dropped
// here are counted nowhere, since there is no track to count them against.
func (c *Client) publishTrack(tr *track, rtpCh, rtcpCh int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.termErr != nil {
		return c.termErr
	}
	c.channels.Store(newChannelTable(c.channels.Load(), tr, rtpCh, rtcpCh))
	c.tracks = append(c.tracks, tr)
	c.channelPairs = append(c.channelPairs, ChannelPair{TrackID: tr.id, RTP: rtpCh, RTCP: rtcpCh})
	c.commitState(methodSetup)
	return nil
}
