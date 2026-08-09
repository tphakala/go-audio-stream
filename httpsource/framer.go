package httpsource

import (
	"errors"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// compressedFramer is the codec-neutral surface the reader drives for a
// compressed source. Each codec's framer (mp3Stream, adtsStream) buffers the
// body across reads and yields one deliverable coded payload at a time; the
// reader below owns the socket read, the watchdog stamp, and delivery, so the
// two framers share one reader loop rather than each carrying a copy.
//
// nextFrame returns the deliverable payload (for MP3 the whole coded frame; for
// ADTS the raw access unit with its header stripped) and that frame's
// presentation duration. The returned slice aliases the framer's buffer and is
// valid only until the next feed or compact, matching the library's
// reader-owns-Data contract.
type compressedFramer interface {
	feed(p []byte)
	nextFrame() (data []byte, dur time.Duration, ok bool)
	compact()
	// setEOF marks the stream ended so the framer delivers a final frame that
	// has no following header to confirm it.
	setEOF()
	// finish counts a truncated final frame once, after the EOF drain.
	finish()
	// gapCount is the running discard count (leading garbage, a false sync, a
	// dropped frame, or a truncated tail), surfaced as the source's malformed
	// counter.
	gapCount() uint64
}

// readCompressed is the reader loop for a compressed body. It accumulates body
// bytes into the framer and delivers each whole coded frame, carrying a partial
// frame across reads. It runs until a terminal condition, whose shutdown it
// funnels before returning. It is shared by every compressed codec through the
// compressedFramer interface.
func (c *Client) readCompressed() {
	for {
		select {
		case <-c.closing:
			return
		default:
		}
		n, err := c.br.Read(c.rbuf[:])
		if n > 0 {
			now := time.Now()
			c.lastReadAt.Store(now.UnixNano())
			c.framer.feed(c.rbuf[:n])
			c.drainCompressed(now)
		}
		if err != nil {
			cause := c.classifyReadErr(err)
			if errors.Is(cause, ErrStreamEnded) {
				// Deliver a final frame that has no following header to confirm
				// it, then count any truncated tail.
				c.framer.setEOF()
				c.drainCompressed(time.Now())
				c.framer.finish()
				c.malformed.Store(c.framer.gapCount())
			}
			c.initiateShutdown(cause)
			return
		}
	}
}

// drainCompressed delivers every whole frame the framer can yield, then compacts
// its buffer and publishes the running discard count.
func (c *Client) drainCompressed(now time.Time) {
	for {
		data, dur, ok := c.framer.nextFrame()
		if !ok {
			break
		}
		c.deliverCompressed(data, dur, now)
	}
	c.framer.compact()
	c.malformed.Store(c.framer.gapCount())
}

// deliverCompressed counts one coded-frame delivery and hands it to OnFrame. The
// frame's PTS is the presentation time of its first sample, computed from the
// running media time before this frame advances it, so the first frame is at PTS
// 0 and successive PTSs increase by each frame's duration. A non-positive
// duration (a frame whose header carried no usable rate) does not advance the
// clock. data aliases reader-owned memory and is valid only during the callback.
func (c *Client) deliverCompressed(data []byte, dur time.Duration, now time.Time) {
	c.packets.Add(1)
	c.payload.Add(uint64(len(data)))
	pts := c.mediaPTS
	if dur > 0 {
		c.mediaPTS += dur
	}
	if c.cfg.OnFrame == nil {
		return
	}
	c.cfg.OnFrame(audiostream.Frame{
		TrackID:    0,
		Data:       data,
		RTPTime:    0,
		PTS:        pts,
		ReceivedAt: now,
		SeqGap:     0,
	})
}
