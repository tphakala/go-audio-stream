package rtsp

import (
	"math"
	"testing"
	"time"
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

// dlsrUnits is the DLSR conversion the Receiver Report builder uses. The field
// is 32 bits in units of 1/65536 s, so it saturates rather than wrapping: a
// single Sender Report followed by a long silence used to read back as a much
// shorter delay.
func TestDLSRUnits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		elapsed time.Duration
		want    uint32
	}{
		{name: "zero", elapsed: 0, want: 0},
		{name: "negative clock step", elapsed: -time.Second, want: 0},
		{name: "one second", elapsed: time.Second, want: 1 << 16},
		{name: "half second", elapsed: 500 * time.Millisecond, want: 1 << 15},
		{name: "ten seconds", elapsed: 10 * time.Second, want: 10 << 16},
		{name: "just under the field width", elapsed: (maxDLSRSeconds - 1) * time.Second, want: (maxDLSRSeconds - 1) << 16},
		{name: "at the field width", elapsed: maxDLSRSeconds * time.Second, want: math.MaxUint32},
		{name: "a day", elapsed: 24 * time.Hour, want: math.MaxUint32},
		{name: "past the shift overflow", elapsed: 100 * time.Hour, want: math.MaxUint32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := dlsrUnits(tc.elapsed); got != tc.want {
				t.Errorf("dlsrUnits(%v) = %d, want %d", tc.elapsed, got, tc.want)
			}
		})
	}
	// Monotonic within the representable range: a longer wait must never read
	// back as a shorter delay, which is exactly what the old shift did.
	prev := uint32(0)
	for _, d := range []time.Duration{time.Second, time.Minute, time.Hour, 5 * time.Hour, 18 * time.Hour} {
		got := dlsrUnits(d)
		if got < prev {
			t.Errorf("dlsrUnits(%v) = %d, lower than the previous %d", d, got, prev)
		}
		prev = got
	}
}
