package rtsp

import (
	"errors"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
)

// These are white-box because the things they pin are not externally
// observable. SessionInfo reports channelPairs, which publishTrack appends
// alongside the routing table rather than deriving from it, so an external test
// asserting SessionInfo passes even with the atomic store deleted. The reader
// does not consult the table yet either, so there is no end-to-end path to it.

func TestPublishTrackPublishesRoutingTable(t *testing.T) {
	t.Parallel()
	c := &Client{state: stateDescribed}
	tr0 := &track{id: 0}
	if err := c.publishTrack(tr0, 4, 5); err != nil {
		t.Fatalf("publishTrack: %v", err)
	}

	tbl := c.channels.Load()
	if tbl == nil {
		t.Fatal("channel table is nil after publishTrack; the atomic store did not happen")
	}
	if b, ok := tbl.lookup(4); !ok || b.track != tr0 || b.isRTCP {
		t.Errorf("lookup(4) = %+v ok=%v, want track 0 RTP", b, ok)
	}
	if b, ok := tbl.lookup(5); !ok || b.track != tr0 || !b.isRTCP {
		t.Errorf("lookup(5) = %+v ok=%v, want track 0 RTCP", b, ok)
	}

	// A second track must be added without dropping the first, since the table
	// is rebuilt from scratch on every publish.
	tr1 := &track{id: 1}
	if err := c.publishTrack(tr1, 6, 7); err != nil {
		t.Fatalf("publishTrack second: %v", err)
	}
	tbl = c.channels.Load()
	if b, ok := tbl.lookup(4); !ok || b.track != tr0 {
		t.Errorf("lookup(4) after second publish = %+v ok=%v, want track 0 still bound", b, ok)
	}
	if b, ok := tbl.lookup(6); !ok || b.track != tr1 {
		t.Errorf("lookup(6) = %+v ok=%v, want track 1", b, ok)
	}
	if c.state != stateSetup {
		t.Errorf("state = %v, want setup", c.state)
	}
}

func TestPublishTrackRefusesAfterShutdown(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("terminal")
	c := &Client{state: stateDescribed, termErr: sentinel}
	if err := c.publishTrack(&track{id: 0}, 0, 1); !errors.Is(err, sentinel) {
		t.Errorf("publishTrack after shutdown = %v, want the terminal cause", err)
	}
	if c.channels.Load() != nil {
		t.Error("a refused publish must not install a routing table")
	}
	if c.state == stateSetup {
		t.Error("a refused publish must not resurrect the state")
	}
}

const (
	testAudioControl = "rtsp://cam/s/audio"
	testVideoControl = "rtsp://cam/s/video"
	// Shared with request_race_test.go, which already uses this id for the
	// session that gates its TEARDOWN assertions.
	testInternalSessionID = "sess-1"
)

func describedFixture() []describedTrack {
	return []describedTrack{
		{control: testAudioControl, codec: audiostream.CodecAAC{}, clockRate: 16000, media: audiostream.MediaAudio},
		{control: testVideoControl, codec: audiostream.CodecUnknown{}, clockRate: 90000, media: audiostream.MediaVideo},
	}
}

// The ID selects the descriptor that builds the depacketizer while Control
// names the stream the SETUP addresses, so this pins that the ID picks the
// right descriptor and that the two cannot be crossed.
func TestDescribedTrackForSelectsByID(t *testing.T) {
	t.Parallel()
	c := &Client{state: stateDescribed, described: describedFixture()}
	desc, err := c.describedTrackFor(Track{ID: 1, Control: testVideoControl})
	if err != nil {
		t.Fatalf("describedTrackFor: %v", err)
	}
	if desc.clockRate != 90000 || desc.media != audiostream.MediaVideo {
		t.Errorf("desc = %+v, want the track-1 descriptor (90000 Hz, video)", desc)
	}
}

func TestDescribedTrackForRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		state state
		pairs []ChannelPair
		trk   Track
		want  error
	}{
		{
			name:  "id past the end",
			state: stateDescribed,
			trk:   Track{ID: 7, Control: testAudioControl},
			want:  ErrUnknownTrack,
		},
		{
			name:  "negative id",
			state: stateDescribed,
			trk:   Track{ID: -1, Control: testAudioControl},
			want:  ErrUnknownTrack,
		},
		{
			// The crossing that silently sets up one stream and decodes it as
			// another: track 0's ID paired with track 1's control URL.
			name:  "id and control from different tracks",
			state: stateDescribed,
			trk:   Track{ID: 0, Control: testVideoControl},
			want:  ErrUnknownTrack,
		},
		{
			name:  "zero Track",
			state: stateDescribed,
			trk:   Track{},
			want:  ErrUnknownTrack,
		},
		{
			name:  "already set up",
			state: stateSetup,
			pairs: []ChannelPair{{TrackID: 0, RTP: 0, RTCP: 1}},
			trk:   Track{ID: 0, Control: testAudioControl},
			want:  ErrTrackAlreadySetUp,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Client{state: tc.state, described: describedFixture(), channelPairs: tc.pairs}
			if _, err := c.describedTrackFor(tc.trk); !errors.Is(err, tc.want) {
				t.Errorf("describedTrackFor = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDescribedTrackForStateGate(t *testing.T) {
	t.Parallel()
	c := &Client{state: stateIdle, described: describedFixture()}
	_, err := c.describedTrackFor(Track{ID: 0, Control: testAudioControl})
	var se *StateError
	if !errors.As(err, &se) {
		t.Fatalf("describedTrackFor in idle = %v, want *StateError", err)
	}
	if se.Method != methodSetup {
		t.Errorf("StateError.Method = %q, want %q", se.Method, methodSetup)
	}
}

func TestNextChannelPair(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		pairs             []ChannelPair
		wantRTP, wantRTCP int
	}{
		{name: "first track", wantRTP: 0, wantRTCP: 1},
		{
			name:    "after a pair the server honoured",
			pairs:   []ChannelPair{{TrackID: 0, RTP: 0, RTCP: 1}},
			wantRTP: 2, wantRTCP: 3,
		},
		{
			// The case a count-based proposal gets wrong: with one pair set up
			// the count says 2-3, but the server put that track on 4-5, so 2-3
			// is below a channel already claimed and the next proposal after it
			// would collide with the client's own track.
			name:    "after the server renumbered upward",
			pairs:   []ChannelPair{{TrackID: 0, RTP: 4, RTCP: 5}},
			wantRTP: 6, wantRTCP: 7,
		},
		{
			name:    "odd highest claimed channel rounds up to an even base",
			pairs:   []ChannelPair{{TrackID: 0, RTP: 2, RTCP: 3}, {TrackID: 1, RTP: 8, RTCP: 9}},
			wantRTP: 10, wantRTCP: 11,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Client{channelPairs: tc.pairs}
			rtpCh, rtcpCh, err := c.nextChannelPair()
			if err != nil {
				t.Fatalf("nextChannelPair: %v", err)
			}
			if rtpCh != tc.wantRTP || rtcpCh != tc.wantRTCP {
				t.Errorf("nextChannelPair = %d-%d, want %d-%d", rtpCh, rtcpCh, tc.wantRTP, tc.wantRTCP)
			}
		})
	}
}

// BuildTransport is a pure serializer whose doc assigns the range check to the
// allocation site, so the ceiling has to be enforced here rather than yielding
// a header naming a channel the format cannot carry.
func TestNextChannelPairCeiling(t *testing.T) {
	t.Parallel()
	c := &Client{channelPairs: []ChannelPair{{TrackID: 0, RTP: 252, RTCP: 253}}}
	rtpCh, rtcpCh, err := c.nextChannelPair()
	if err != nil {
		t.Fatalf("nextChannelPair at 254-255: %v", err)
	}
	if rtpCh != 254 || rtcpCh != 255 {
		t.Fatalf("nextChannelPair = %d-%d, want 254-255 (the last pair that fits)", rtpCh, rtcpCh)
	}

	c = &Client{channelPairs: []ChannelPair{{TrackID: 0, RTP: 254, RTCP: 255}}}
	if _, _, err := c.nextChannelPair(); !errors.Is(err, ErrNoChannelsLeft) {
		t.Errorf("nextChannelPair past the ceiling = %v, want ErrNoChannelsLeft", err)
	}
}

// recordSession must still record during shutdown: the terminal sequence reads
// sessionID to decide whether a TEARDOWN is warranted, and initiateShutdown
// sets termErr before the reader reaches that decision.
func TestRecordSessionDuringShutdown(t *testing.T) {
	t.Parallel()
	c := &Client{termErr: errors.New("terminal")}
	c.recordSession(0, SessionHeader{ID: testInternalSessionID, Timeout: 60})
	if c.sessionID != testInternalSessionID {
		t.Errorf("sessionID = %q, want it recorded so the TEARDOWN can be authorized", c.sessionID)
	}
	if !c.sessionEstablished() {
		t.Error("sessionEstablished = false; the terminal sequence would skip the TEARDOWN")
	}
}

func TestRecordSession(t *testing.T) {
	t.Parallel()
	// A SETUP response with no Session header must not record a defaulted
	// timeout against an empty id.
	c := &Client{}
	c.recordSession(0, ParseSession(""))
	if c.sessionID != "" || c.sessionTimeout != 0 {
		t.Errorf("id=%q timeout=%v, want empty and zero for a missing Session header", c.sessionID, c.sessionTimeout)
	}

	// The first id governs; a later differing id does not overwrite it.
	c = &Client{}
	c.recordSession(0, SessionHeader{ID: "first", Timeout: 90})
	c.recordSession(1, SessionHeader{ID: "second", Timeout: 30})
	if c.sessionID != "first" {
		t.Errorf("sessionID = %q, want the first id to govern", c.sessionID)
	}
	if c.sessionTimeout != 90 {
		t.Errorf("sessionTimeout = %v, want the first timeout", c.sessionTimeout)
	}
}
