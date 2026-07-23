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
