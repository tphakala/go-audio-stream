package rtsp

import "context"

// Setup issues a SETUP for one track over the TCP-interleaved profile,
// negotiates its interleaved channel pair, and constructs and publishes the
// track's depacketization pipeline into the channel routing table. It is legal
// only in the described or setup state, else it returns a *StateError. A clean
// protocol response the caller may recover from (a non-2xx *ResponseError,
// *audiostream.RedirectError, or *UnauthorizedError, or a malformed Transport:
// ErrNoInterleaved, ErrBadChannelPair, ErrChannelConflict, ErrMalformedTransport)
// is returned without tearing down the session, so the caller may Close or try
// another track. On success it transitions the client into the setup state.
func (c *Client) Setup(ctx context.Context, trk Track, opts SetupOptions) error {
	c.mu.Lock()
	if c.state != stateDescribed && c.state != stateSetup {
		st := c.state.String()
		c.mu.Unlock()
		return &StateError{Method: methodSetup, State: st}
	}
	k := len(c.channelPairs)
	desc := describedTrack{}
	if trk.ID >= 0 && trk.ID < len(c.described) {
		desc = c.described[trk.ID]
	}
	c.mu.Unlock()

	req := &Request{Method: methodSetup, URL: trk.Control, Header: Header{}}
	req.Header.Set("Transport", BuildTransport(2*k, 2*k+1))
	resp, err := c.do(ctx, req)
	if err != nil {
		return err
	}

	c.recordSession(ParseSession(resp.Header.Get("Session")))

	th, terr := ParseTransport(resp.Header.Get("Transport"))
	if terr != nil {
		return terr
	}
	rtpCh, rtcpCh, cerr := th.InterleavedChannels(c.claimedChannels())
	if cerr != nil {
		return cerr
	}

	tr := newTrack(trk.ID, desc, opts, rtpCh, rtcpCh, c.cfg.Logger)
	return c.publishTrack(tr, rtpCh, rtcpCh)
}

// recordSession stores the session id and timeout from the first SETUP. A later
// SETUP that reports a different id is a server quirk: the first id governs the
// aggregate session and the mismatch is logged, not fatal.
func (c *Client) recordSession(sh SessionHeader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionID == "" {
		c.sessionID = sh.ID
		c.sessionTimeout = sh.Timeout
		return
	}
	if sh.ID != "" && sh.ID != c.sessionID {
		logWarn(c.cfg.Logger, "SETUP returned a session id different from the established one")
	}
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
// reader relies on, so tr must be fully initialized before this call. When
// shutdown has already begun (the terminal error is set) it does not resurrect
// the state and returns that terminal error.
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
