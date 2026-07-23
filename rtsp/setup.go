package rtsp

import (
	"context"
	"errors"
	"fmt"
)

// maxInterleavedChannel is the highest channel number an interleaved frame
// header can carry. The channel occupies one byte of the four-byte header, so
// this ceiling is imposed by the format, not chosen by this package.
// BuildTransport is a pure serializer that assigns the range check to whoever
// allocates the pair, which is nextChannelPair below.
const maxInterleavedChannel = 255

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

	// ErrTooManyTracks is returned by Setup when no interleaved channel pair
	// remains below maxInterleavedChannel.
	ErrTooManyTracks = errors.New("rtsp: no interleaved channel pair left")
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
// *ResponseError, *audiostream.RedirectError, or *UnauthorizedError) is
// returned without tearing down the session, so the caller may Close or try
// another track.
//
// A Transport this client rejects (ErrNoInterleaved, ErrBadChannelPair,
// ErrChannelConflict, ErrMalformedTransport) is not the same case. The server
// answered 2xx, so it has already allocated that stream and its channels, and
// this client neither records them nor tears that one track down. Prefer Close
// after such a rejection: continuing with another track risks the server
// streaming two tracks over one channel once PLAY starts the aggregate session.
//
// On success it transitions the client into the setup state.
func (c *Client) Setup(ctx context.Context, trk Track, opts SetupOptions) error {
	desc, err := c.describedTrackFor(trk)
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

	tr := newTrack(trk.ID, desc, opts, gotRTP, gotRTCP, c.cfg.Logger)
	return c.publishTrack(tr, gotRTP, gotRTCP)
}

// describedTrackFor checks the lifecycle gate and validates trk against the
// descriptors Describe retained, returning the descriptor the track's ID
// selects. The gate and the lookup share one critical section because both read
// mu-guarded state that a concurrent shutdown may change between them.
func (c *Client) describedTrackFor(trk Track) (describedTrack, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != stateDescribed && c.state != stateSetup {
		return describedTrack{}, &StateError{Method: methodSetup, State: c.state.String()}
	}
	if trk.ID < 0 || trk.ID >= len(c.described) {
		return describedTrack{}, fmt.Errorf("%w: id %d, Describe returned %d track(s)",
			ErrUnknownTrack, trk.ID, len(c.described))
	}
	desc := c.described[trk.ID]
	if trk.Control != desc.control {
		return describedTrack{}, fmt.Errorf("%w: id %d resolves to %s, not %s",
			ErrUnknownTrack, trk.ID, RedactURL(desc.control), RedactURL(trk.Control))
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
			ErrTooManyTracks, base, base+1, maxInterleavedChannel)
	}
	return base, base + 1, nil
}

// recordSession stores the session id and timeout from the first SETUP. A later
// SETUP that reports a different id is a server quirk: the first id governs the
// aggregate session and the mismatch is logged, not fatal.
//
// It refuses to write once shutdown has begun, for publishTrack's reason and
// one more that is specific to the session id. The reader evaluates
// sessionEstablished exactly once, in its terminal sequence; a SETUP response
// landing after that check could otherwise stamp the first session id onto an
// already-closing client, and the TEARDOWN that id exists to authorize would
// never be sent. The camera would hold the session until its own timeout, and
// one with a single-session limit would refuse the next Dial.
func (c *Client) recordSession(trackID int, sh SessionHeader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.termErr != nil {
		return
	}
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
// reader will rely on once it routes frames, so tr must be fully initialized
// before this call. When shutdown has already begun (the terminal error is set)
// it does not resurrect the state and returns that terminal error.
func (c *Client) publishTrack(tr *track, rtpCh, rtcpCh int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.termErr != nil {
		return c.termErr
	}
	c.channels.Store(newChannelTable(c.channels.Load(), tr, rtpCh, rtcpCh))
	c.tracks = append(c.tracks, tr)
	c.channelPairs = append(c.channelPairs, ChannelPair{TrackID: tr.id, RTP: rtpCh, RTCP: rtcpCh})
	c.state = stateSetup
	return nil
}
