package rtsp

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// The resync gate classifies an interleaved frame header by its channel byte:
// a channel the session bound re-locks the stream, an unbound one is garbage,
// and a header not yet fully read must be filled rather than discarded.
func TestGateResyncFrame(t *testing.T) {
	t.Parallel()
	bound := func() *Client {
		c := &Client{}
		c.channels.Store(newChannelTable(nil, &track{id: 0}, 0, 1))
		return c
	}

	cases := []struct {
		name string
		c    *Client
		buf  []byte
		want resyncVerdict
	}{
		{name: "bound rtp channel", c: bound(), buf: []byte{'$', 0x00, 0x00, 0x04}, want: resyncAccept},
		{name: "bound rtcp channel", c: bound(), buf: []byte{'$', 0x01, 0x00, 0x04}, want: resyncAccept},
		{name: "unbound channel", c: bound(), buf: []byte{'$', 0x07, 0x00, 0x04}, want: resyncDiscard},
		{name: "channel byte not read yet", c: bound(), buf: []byte{'$'}, want: resyncFill},
		// Before the first Setup the table is nil, so nothing is vouched for
		// and every frame-looking byte run stays garbage.
		{name: "no table yet", c: &Client{}, buf: []byte{'$', 0x00, 0x00, 0x04}, want: resyncDiscard},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.c.rbuf = tc.buf
			if got := tc.c.gateResyncFrame(); got != tc.want {
				t.Errorf("gateResyncFrame() = %d, want %d", got, tc.want)
			}
		})
	}
}

// The gate reads the channel byte at the parse offset, not at the head of the
// accumulation buffer, so a partially consumed buffer is classified correctly.
func TestGateResyncFrameHonoursParseOffset(t *testing.T) {
	t.Parallel()
	c := &Client{}
	c.channels.Store(newChannelTable(nil, &track{id: 0}, 4, 5))
	c.rbuf = []byte{0xff, 0xff, '$', 0x04, 0x00, 0x02}
	c.start = 2
	if got := c.gateResyncFrame(); got != resyncAccept {
		t.Errorf("gateResyncFrame() = %d, want resyncAccept at start=%d", got, c.start)
	}
}

// The discard path admits a frame as re-lock evidence only when it looks like a
// real RTP packet: at least a full header, and version 2. Both halves matter,
// because a run of >=12-byte non-RTP garbage on a discarded track's channel
// would otherwise clear the resync budget during a desync. This guards the
// version check directly, which the integration tests miss: one frames a
// sub-header payload that short-circuits before the version compare, the other
// asserts only allocation.
func TestDiscardTrackUsableVerdict(t *testing.T) {
	t.Parallel()
	c := &Client{}
	tr := &track{id: 0, discard: true}
	c.channels.Store(newChannelTable(nil, tr, 0, 1))

	header := func(b0 byte) InterleavedFrame {
		p := make([]byte, rtp.HeaderSize)
		p[0] = b0
		return InterleavedFrame{Channel: 0, Payload: p}
	}
	cases := []struct {
		name   string
		frame  InterleavedFrame
		usable bool
	}{
		{name: "valid version 2", frame: header(0x80), usable: true},
		{name: "wrong version", frame: header(0x00), usable: false},
		{name: "too short", frame: InterleavedFrame{Channel: 0, Payload: []byte{0x80, 0x00}}, usable: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.handleInterleaved(tc.frame); got != tc.usable {
				t.Errorf("handleInterleaved usable = %v, want %v", got, tc.usable)
			}
		})
	}
}

// A frame on a discard track's RTCP channel is not media: it is counted
// nowhere and never stamps the media clock, yet it still returns the same shape
// check the RTP branch does so it cannot clear the resync budget any more
// easily than real RTP. Before the isRTCP-before-discard restructure the
// discard branch swallowed these compounds and inflated Packets and the byte
// count, so this pins the fix at the routing level, where an integration test
// cannot see the exact zero.
func TestDiscardTrackRTCPChannelNotCounted(t *testing.T) {
	t.Parallel()
	// newChannelTable binds channel 0 as RTP and channel 1 as RTCP, both to tr.
	rtcpFrame := func(payload []byte) InterleavedFrame {
		return InterleavedFrame{Channel: 1, Payload: payload}
	}
	full := make([]byte, rtp.HeaderSize)
	full[0] = 0x80 // version 2, so the shape check passes
	cases := []struct {
		name   string
		frame  InterleavedFrame
		usable bool
	}{
		{name: "version 2 compound", frame: rtcpFrame(full), usable: true},
		{name: "short compound", frame: rtcpFrame([]byte{0x80, 0xC9}), usable: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{}
			tr := &track{id: 0, discard: true}
			c.channels.Store(newChannelTable(nil, tr, 0, 1))
			if got := c.handleInterleaved(tc.frame); got != tc.usable {
				t.Errorf("handleInterleaved usable = %v, want %v (verdict expression must be unchanged)", got, tc.usable)
			}
			if got := tr.packets.Load(); got != 0 {
				t.Errorf("packets = %d, want 0 (an RTCP compound must not count as a media packet)", got)
			}
			if got := tr.wireBytes.Load(); got != 0 {
				t.Errorf("wireBytes = %d, want 0 (RTCP is excluded from the RTP-channel wire count)", got)
			}
			if got := tr.payloadBytes.Load(); got != 0 {
				t.Errorf("payloadBytes = %d, want 0", got)
			}
			if got := tr.lastFrameUnixNano.Load(); got != 0 {
				t.Errorf("lastFrameUnixNano = %d, want 0 (RTCP is not a media-liveness stamp)", got)
			}
		})
	}
}

// A frame on an ACTIVE track's RTCP channel is routed to handleRTCP, which
// touches none of the RTP-channel media counters. Sentinels are preloaded so
// the assertion proves the RTCP path leaves them alone rather than merely never
// having set them.
func TestActiveTrackRTCPDoesNotStampMediaCounters(t *testing.T) {
	t.Parallel()
	c := &Client{}
	tr := &track{id: 0} // active: discard is false
	c.channels.Store(newChannelTable(nil, tr, 0, 1))

	tr.lastFrameUnixNano.Store(12345)
	tr.wireBytes.Store(999)
	tr.packets.Store(7)
	tr.payloadBytes.Store(42)

	// An 8-byte Receiver Report on the RTCP channel: a well-formed compound that
	// carries no Sender Report.
	rtcp := InterleavedFrame{Channel: 1, Payload: []byte{0x80, 0xC9, 0x00, 0x01, 0xCA, 0xFE, 0xBA, 0xBE}}
	c.handleInterleaved(rtcp)

	if got := tr.lastFrameUnixNano.Load(); got != 12345 {
		t.Errorf("lastFrameUnixNano = %d, want 12345 (RTCP must not stamp the media clock)", got)
	}
	if got := tr.wireBytes.Load(); got != 999 {
		t.Errorf("wireBytes = %d, want 999 (RTCP is excluded from the RTP-channel wire count)", got)
	}
	if got := tr.packets.Load(); got != 7 {
		t.Errorf("packets = %d, want 7 (RTCP is not a media packet)", got)
	}
	if got := tr.payloadBytes.Load(); got != 42 {
		t.Errorf("payloadBytes = %d, want 42", got)
	}
}

// A shape-invalid frame on a discard track's RTP channel is wire traffic and
// liveness evidence but not an accepted packet: WireBytes and LastFrameAt
// advance while Packets does not, so a peer cannot inflate the packet count
// with garbage on a discarded channel. This pins the shape-gated increment.
func TestDiscardTrackShapeInvalidNotCountedAsPacket(t *testing.T) {
	t.Parallel()
	// newChannelTable binds channel 0 as the RTP channel and channel 1 as RTCP.
	rtpFrame := func(payload []byte) InterleavedFrame {
		return InterleavedFrame{Channel: 0, Payload: payload}
	}
	valid := make([]byte, rtp.HeaderSize)
	valid[0] = 0x80 // version 2, a full header: shape-valid
	wrongVersion := make([]byte, rtp.HeaderSize)
	wrongVersion[0] = 0x00 // full length but version 0
	cases := []struct {
		name       string
		frame      InterleavedFrame
		wantPacket uint64
	}{
		{name: "shape valid counts", frame: rtpFrame(valid), wantPacket: 1},
		{name: "too short", frame: rtpFrame([]byte{0x80, 0x00}), wantPacket: 0},
		{name: "wrong version", frame: rtpFrame(wrongVersion), wantPacket: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{}
			tr := &track{id: 0, discard: true}
			c.channels.Store(newChannelTable(nil, tr, 0, 1))
			c.handleInterleaved(tc.frame)
			if got := tr.packets.Load(); got != tc.wantPacket {
				t.Errorf("packets = %d, want %d (only a shape-valid frame is an accepted packet)", got, tc.wantPacket)
			}
			// Every RTP-channel frame counts as wire traffic and stamps the media
			// clock, shape-valid or not.
			wantWire := interleavedHeaderLen + uint64(len(tc.frame.Payload))
			if got := tr.wireBytes.Load(); got != wantWire {
				t.Errorf("wireBytes = %d, want %d (every RTP-channel frame is wire traffic)", got, wantWire)
			}
			if tr.lastFrameUnixNano.Load() == 0 {
				t.Error("lastFrameUnixNano is 0, want the frame's arrival stamp")
			}
			if got := tr.payloadBytes.Load(); got != 0 {
				t.Errorf("payloadBytes = %d, want 0 (a discard track is never parsed)", got)
			}
		})
	}
}
