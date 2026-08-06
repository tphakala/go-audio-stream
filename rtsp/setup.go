package rtsp

import (
	"context"
	"errors"
	"fmt"
	"time"
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
// Under a UDP transport preference (PreferUDP or PreferUDPThenTCP) it instead
// dispatches to the UDP negotiation (setupUDP), which proposes RTP/AVP unicast
// and, on success, publishes the track without touching the interleaved channel
// table. The description below of the interleaved profile is the TCP path.
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
// answered 2xx, so it has already allocated that stream and its channels. This
// client records nothing for the track and releases the server's stream with a
// best-effort per-track TEARDOWN, so an aggregate PLAY cannot go on to stream it
// over a channel no track claimed. The session survives: the caller may set up
// another track or PLAY the ones that succeeded.
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

	// The pin resolved by an earlier Setup makes the choice session-wide: read
	// it under mu (lifecycleMu already serializes Setups, so no concurrent one
	// can move it) and let resolveTransport fold it together with the caller's
	// preference. attemptUDP picks the path; allowFallback governs only whether
	// a non-2xx UDP rejection may re-issue over TCP.
	c.mu.Lock()
	pin := c.sessionTransport
	c.mu.Unlock()

	attemptUDP, allowFallback := resolveTransport(c.cfg.Transport, pin)
	if attemptUDP {
		return c.setupUDP(ctx, trk, desc, opts, allowFallback)
	}
	return c.setupTCP(ctx, trk, desc, opts)
}

// Media transport pin values. sessionTransport holds one of these once the
// first Setup resolves, and SessionInfo.Transport surfaces it verbatim; "" is
// the unresolved default before any track is set up.
const (
	transportPinTCP = "TCP"
	transportPinUDP = "UDP"
)

// resolveTransport decides, for one Setup, whether to attempt UDP and whether a
// non-2xx UDP rejection may fall back to TCP. It folds the caller's preference
// together with the session pin an earlier Setup resolved (pin is "" until the
// first Setup pins it, then transportPinTCP or transportPinUDP).
//
// The pin is what makes the choice session-wide (D6): once a track is set up,
// every later Setup follows the same transport with no re-attempt, so a
// PreferUDPThenTCP session that fell back to TCP never proposes UDP again, and a
// UDP session never re-SETUPs over TCP. allowFallback is true for exactly one
// case, the first Setup under PreferUDPThenTCP, because that is the only moment
// a UDP rejection has a TCP path to fall back to that no live session sits on.
func resolveTransport(pref TransportPreference, pin string) (attemptUDP, allowFallback bool) {
	switch pin {
	case transportPinTCP:
		return false, false
	case transportPinUDP:
		return true, false
	}
	// Not yet pinned: the first Setup decides from the preference alone.
	switch pref {
	case PreferUDP:
		return true, false
	case PreferUDPThenTCP:
		return true, true
	default: // PreferTCP, the zero value.
		return false, false
	}
}

// setupTCP is the phase 1 SETUP path: RTP/AVP/TCP interleaved, unchanged in
// behavior from before UDP transport support.
//
//nolint:gocritic // Track is the documented public Setup signature; hugeParam does not apply to a per-track lifecycle call.
func (c *Client) setupTCP(ctx context.Context, trk Track, desc describedTrack, opts SetupOptions) error {
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
		// The server answered SETUP 2xx and allocated the stream before echoing
		// a Transport this client cannot parse. Release just that stream so an
		// aggregate PLAY does not go on to stream it (see #15).
		c.teardownRejectedStream(trk.Control)
		return fmt.Errorf("track %d: %w (Transport: %q)", trk.ID, terr, raw)
	}
	gotRTP, gotRTCP, cerr := th.InterleavedChannels(c.claimedChannels())
	if cerr != nil {
		// The server allocated the stream, then echoed an interleaved Transport
		// this client cannot bind (a channel conflict or missing pair). Release
		// just that stream: an aggregate PLAY would otherwise stream it on
		// channels no track claimed, routing its RTP into another track's
		// depacketizer as counterfeit audio (see #15).
		c.teardownRejectedStream(trk.Control)
		return fmt.Errorf("track %d: %w (Transport: %q)", trk.ID, cerr, raw)
	}

	tr := newTrack(trk.ID, desc, opts, gotRTCP, c.cfg.Logger)
	return c.publishTrack(tr, gotRTP, gotRTCP)
}

// setupUDP is the UDP SETUP path: it opens a per-track socket pair, proposes
// RTP/AVP unicast, resolves the server's peers from the response, and
// publishes the track without touching the interleaved channel table (UDP
// does not route by interleaved channel). On any failure it releases the
// sockets, and when the server already allocated the stream, releases that
// stream too, so the session and its other tracks are unaffected.
//
// It splits the two UDP-rejection shapes by SETUP status class (D6), because
// they differ in whether the server created session state. A non-2xx response
// (a *ResponseError, e.g. 461) allocated nothing: with allowFallback it re-runs
// the phase 1 TCP SETUP for the same track, else it returns ErrUDPSetupRejected,
// and neither needs a TEARDOWN. A 2xx whose Transport has no usable server_port
// DID create state: it never falls back (that would re-SETUP a live session),
// tears the accepted session down, and returns ErrUDPSetupRejected regardless of
// allowFallback.
//
//nolint:gocritic // Track is the documented public Setup signature; hugeParam does not apply to a per-track lifecycle call.
func (c *Client) setupUDP(ctx context.Context, trk Track, desc describedTrack, opts SetupOptions, allowFallback bool) error {
	m, oerr := openMediaSockets()
	if oerr != nil {
		return oerr
	}

	req := &Request{Method: methodSetup, URL: trk.Control, Header: Header{}}
	req.Header.Set("Transport", BuildTransportUDP(m.clientRTPPort, m.clientRTCPPort))
	resp, derr := c.do(ctx, req)
	if derr != nil {
		_ = m.Close()
		var respErr *ResponseError
		if errors.As(derr, &respErr) {
			// A non-2xx SETUP response (for example 461 Unsupported Transport):
			// the server declined and allocated nothing, no Session id and no
			// server-side UDP port, so the sockets just closed are the only
			// state to release and no TEARDOWN is warranted. This is the ONE
			// rejection shape that may fall back, precisely because no live
			// session sits on the connection to re-SETUP against.
			if allowFallback {
				// PreferUDPThenTCP's first Setup: re-issue the SAME track's
				// SETUP over the phase 1 TCP-interleaved profile, which records
				// the session and pins it TCP. From here it is byte-for-byte the
				// phase 1 path.
				return c.setupTCP(ctx, trk, desc, opts)
			}
			// The status code is folded in as a value, not wrapped: under
			// PreferUDP the contract is that a UDP rejection matches only
			// ErrUDPSetupRejected, not ErrResponseStatus, so a caller cannot
			// mistake this terminal UDP refusal for a per-track protocol error.
			return fmt.Errorf("track %d: %w (server declined the UDP transport with status %d)",
				trk.ID, ErrUDPSetupRejected, respErr.Code)
		}
		// A redirect, an auth failure, or a connection or timeout error is not a
		// UDP rejection: propagate it unchanged, so the caller and the session
		// react to it exactly as they would from a TCP Setup.
		return derr
	}

	c.recordSession(trk.ID, ParseSession(resp.Header.Get("Session")))

	raw := resp.Header.Get("Transport")
	th, terr := ParseTransport(raw)
	if terr != nil {
		// The server answered SETUP 2xx and allocated the stream before
		// echoing a Transport this client cannot parse. Release the sockets
		// and the stream so an aggregate PLAY does not go on to stream it
		// (see #15, the TCP analog of this same failure).
		_ = m.Close()
		c.teardownRejectedStream(trk.Control)
		return fmt.Errorf("track %d: %w (Transport: %q)", trk.ID, terr, raw)
	}

	if rerr := m.resolveServerPeers(th, remoteIP(c.conn.RemoteAddr())); rerr != nil {
		// The server allocated the stream, then echoed a Transport with no
		// usable server_port. Release the sockets and the stream, and
		// report the rejection rather than the parse-level error, since
		// resolveServerPeers already normalizes it to ErrUDPSetupRejected.
		_ = m.Close()
		c.teardownRejectedStream(trk.Control)
		return fmt.Errorf("track %d: %w (Transport: %q)", trk.ID, rerr, raw)
	}

	// rtcpCh is 0 and unused in UDP mode: Receiver Reports go out over the
	// UDP RTCP socket (sendReceiverReportsUDP in keepalive.go), keyed by
	// track id through c.media rather than by an interleaved channel; the
	// depacketizer and stat wiring newTrack builds are transport-independent.
	tr := newTrack(trk.ID, desc, opts, 0, c.cfg.Logger)
	if perr := c.publishUDPTrack(tr, trk.ID, m); perr != nil {
		return perr
	}

	// Hole-punch now that the server peers are resolved and the track is
	// registered, using an initial Receiver Report built from tr's still-zero
	// snapshot (D4): the return path through a NAT or firewall needs opening
	// before Play starts the receive goroutines, and this is the earliest
	// point both the resolved peers and a track to build the RR from exist.
	// Best-effort: holePunch already logs and ignores its own errors, so a
	// punch failure never fails Setup.
	m.holePunch(tr.buildReceiverReport(c.reporterSSRC, time.Now()).Marshal(), c.cfg.Logger)
	return nil
}

// teardownRejectedStream sends a best-effort per-track TEARDOWN for a stream the
// server allocated (it answered SETUP 2xx) but whose echoed Transport this
// client could not bind. Without it the server keeps that stream, and because
// PLAY is an aggregate operation the server would stream it on channels this
// client never bound, feeding its RTP into another track's depacketizer as
// counterfeit audio. Only the one rejected stream is released; the session and
// any already-bound tracks survive, so the caller may still set up other tracks
// or PLAY the ones that succeeded.
//
// It is fire-and-forget through marshalBareRequest and writeMessage, the same
// non-fatal idiom the keepalive and the close-time TEARDOWN use, and
// deliberately NOT c.do/roundTrip. roundTrip funnels a write error, a request
// timeout, or a ctx cancellation into initiateShutdown, which would tear the
// whole session down, and a server that just echoed an unbindable Transport is
// exactly the kind that may never answer this TEARDOWN. The request-URI is the
// rejected track's own control URL (so a compliant server releases just that
// stream), and marshalBareRequest attaches the Session recordSession stored
// before the parse. No pending entry is registered, so any reply is dropped as
// an unknown CSeq; a server that honours only an aggregate TEARDOWN still
// releases the stream when the session is closed.
func (c *Client) teardownRejectedStream(control string) {
	raw, err := c.marshalBareRequest(methodTeardown, control)
	if err != nil {
		logWarn(c.cfg.Logger, "could not marshal a TEARDOWN for a rejected stream; the server may hold it until its own timeout",
			"error", err)
		return
	}
	_ = c.writeMessage(raw)
}

// describedTrackFor checks the lifecycle gate and validates trk against the
// descriptors Describe retained, returning the descriptor the track's ID
// selects. The gate and the lookup share one critical section because both read
// mu-guarded state that a concurrent shutdown may change between them.
func (c *Client) describedTrackFor(trk *Track) (describedTrack, error) {
	if trk == nil {
		return describedTrack{}, fmt.Errorf("%w: nil track", ErrUnknownTrack)
	}
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
	// Scan c.tracks, not c.channelPairs: both publishTrack (TCP) and
	// publishUDPTrack (UDP) append to c.tracks under c.mu, but only publishTrack
	// records a channel pair. Guarding on c.tracks rejects a duplicate Setup of
	// either transport; over UDP a second Setup would otherwise open a second
	// socket pair, orphan the first, and leave two goroutines reading one socket.
	for _, t := range c.tracks {
		if t.id == trk.ID {
			return describedTrack{}, fmt.Errorf("%w: id %d", ErrTrackAlreadySetUp, trk.ID)
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
	// Pin the session TCP on the first track and re-affirm it on every later
	// one (this path is only ever reached in TCP mode, including the UDP-to-TCP
	// fallback), so SessionInfo reports "TCP" and later Setups follow the pin.
	c.sessionTransport = transportPinTCP
	c.commitState(methodSetup)
	return nil
}

// publishUDPTrack records tr and pins the session to UDP transport, the UDP
// counterpart to publishTrack. Unlike publishTrack it does not touch the
// interleaved channel table (UDP does not route by interleaved channel), and
// it separately registers m under mediaMu rather than under mu: mediaMu is
// an independent leaf lock, so the two critical sections are taken one after
// the other, never nested. When shutdown has already begun it does not
// resurrect the state, closes m, and returns the terminal error, the same
// guard publishTrack applies.
func (c *Client) publishUDPTrack(tr *track, trackID int, m *mediaSockets) error {
	c.mu.Lock()
	if c.termErr != nil {
		c.mu.Unlock()
		_ = m.Close()
		return c.termErr
	}
	c.tracks = append(c.tracks, tr)
	c.udpPinned.Store(true)
	c.transport = c.cfg.Transport
	// Pin the session UDP so SessionInfo reports "UDP" and later Setups follow
	// the pin; udpPinned stays the flag the UDP Play and keepalive paths read.
	c.sessionTransport = transportPinUDP
	// Record this track's negotiated UDP port pair under mu, next to the
	// channelPairs equivalent, so SessionInfo can report it without reaching
	// into mediaMu-guarded state.
	c.udpEndpoints = append(c.udpEndpoints, m.endpoint(trackID))
	c.commitState(methodSetup)
	c.mu.Unlock()

	c.registerMediaSockets(trackID, m)
	return nil
}

// registerMediaSockets records m under trackID in c.media, taking mediaMu
// alone (never nested inside mu or lifecycleMu), the leaf-lock discipline
// initiateShutdown's deadline-arming block and closeMediaSockets both rely
// on.
//
// publishUDPTrack passes its termErr check under mu, releases mu, then calls
// this, so shutdown can begin in the gap: closeMediaSockets (which snapshots
// c.media under mediaMu) may already have run without seeing m, and adding m
// afterward would leak a socket pair no one closes. Guard against that here:
// c.closing is closed by initiateShutdown before teardownAndJoin runs
// closeMediaSockets, and both sites serialize on mediaMu, so a closed c.closing
// observed under the lock means teardown has run or will run without m. In that
// case do not register m; close it (outside the lock, keeping mediaMu a pure
// map-access leaf) and return.
func (c *Client) registerMediaSockets(trackID int, m *mediaSockets) {
	c.mediaMu.Lock()
	select {
	case <-c.closing:
		c.mediaMu.Unlock()
		_ = m.Close()
		return
	default:
	}
	if c.media == nil {
		c.media = make(map[int]*mediaSockets)
	}
	c.media[trackID] = m
	c.mediaMu.Unlock()
}
