package httpsource

import (
	"encoding/binary"
	"errors"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/mp3"
)

// mp3MaxFrameLen sizes the framer's initial buffer so that a whole Layer III
// frame plus the next header's lookahead fit without repeated growth. It is the
// Layer III worst case (MPEG-1 320 kbps at 32 kHz, plus a padding slot = 1441
// bytes), not a hard ceiling: a rarer, larger Layer II frame simply grows the
// buffer via append, so nothing is lost by not sizing for it up front.
const mp3MaxFrameLen = 1441

// id3v2HeaderLen is the fixed ID3v2 tag-header size in bytes, preceding the
// syncsafe tag-size field (byte 5 holds the flags, bytes 6-9 the size).
const id3v2HeaderLen = 10

// id3v2FooterFlag is the ID3v2 header flag (byte 5, bit 4) that signals a
// 10-byte footer at the tag's end, which the skip length must include.
const id3v2FooterFlag = 0x10

// mp3Stream frames a raw MPEG audio byte stream into whole coded frames. It
// buffers bytes across reads, skips a leading ID3v2 tag, and resynchronizes
// past leading garbage or a false sync with a next-frame consistency check. It
// never decodes. It is not safe for concurrent use; the reader goroutine owns
// it.
type mp3Stream struct {
	// buf holds unconsumed body bytes; off is the read cursor into it. A frame
	// is returned as a sub-slice of buf[off:], valid only until the next feed or
	// compact, matching the library's reader-owns-Data contract.
	buf []byte
	off int
	// id3Skip counts bytes still to discard for a leading ID3v2 tag. The tag is
	// consumed as bytes arrive, so an arbitrarily large tag is never buffered.
	id3Skip int64
	// synced is set once a frame has been confirmed by a following valid header.
	// A synced stream trusts a frame with no lookahead left (the stream tail).
	synced bool
	// ended is set at EOF: the final frame is then delivered without a following
	// header to confirm it.
	ended bool
	// gaps counts discard events (leading garbage, a false sync, or a truncated
	// final frame). It is the framer's malformed-frame equivalent.
	gaps uint64
}

// feed appends freshly read body bytes to the buffer.
func (s *mp3Stream) feed(p []byte) {
	s.buf = append(s.buf, p...)
}

// next returns the next whole frame and its header, or ok=false when the buffer
// does not yet hold a confirmable frame. The returned frame aliases the buffer
// and is valid only until the next feed or compact.
func (s *mp3Stream) next() (frame []byte, hdr mp3.Header, ok bool) {
	for {
		if !s.consumeID3() {
			return nil, mp3.Header{}, false
		}
		rem := s.buf[s.off:]
		if isID3, needMore := looksLikeID3(rem); isID3 {
			if needMore {
				return nil, mp3.Header{}, false
			}
			s.startID3Skip(rem)
			continue
		}
		i, h, found := scanHeader(rem)
		if !found {
			s.dropUnsynced(len(rem))
			return nil, mp3.Header{}, false
		}
		if i > 0 {
			// Bytes before the header are garbage: sync is lost, so the next
			// candidate must be reconfirmed rather than trusted on the stale flag.
			s.discard(i)
			s.synced = false
			rem = s.buf[s.off:]
		}
		if len(rem) < h.FrameLen {
			return nil, mp3.Header{}, false // wait for the whole frame
		}
		if len(rem) >= h.FrameLen+mp3.HeaderLen {
			if !headerConsistent(rem, h) {
				// A false sync: the bytes at the next-frame offset are not a valid
				// header. Advance one byte and rescan from the next position.
				s.discard(1)
				s.synced = false
				continue
			}
			s.synced = true
		} else if !s.synced && !s.ended {
			// The first sync cannot be confirmed yet and more bytes may arrive.
			return nil, mp3.Header{}, false
		}
		frame = s.buf[s.off : s.off+h.FrameLen]
		s.off += h.FrameLen
		return frame, h, true
	}
}

// consumeID3 discards any pending ID3v2 bytes that have arrived. It returns true
// when nothing is pending (or the tag is fully skipped) and false when more
// bytes are still needed to finish the skip.
func (s *mp3Stream) consumeID3() bool {
	if s.id3Skip == 0 {
		return true
	}
	avail := int64(len(s.buf) - s.off)
	drop := min(s.id3Skip, avail)
	s.off += int(drop)
	s.id3Skip -= drop
	return s.id3Skip == 0
}

// startID3Skip records the byte count of a leading ID3v2 tag to discard. rem
// must be at least id3v2HeaderLen bytes and start with the "ID3" magic. The tag
// is metadata, not corruption, so skipping it is not counted as a gap.
func (s *mp3Stream) startID3Skip(rem []byte) {
	skip := int64(id3v2HeaderLen) + int64(syncsafe(rem[6:10]))
	if rem[5]&id3v2FooterFlag != 0 {
		skip += id3v2HeaderLen
	}
	s.id3Skip = skip
}

// discard advances past n unusable bytes and counts one gap.
func (s *mp3Stream) discard(n int) {
	s.off += n
	s.gaps++
}

// dropUnsynced discards a run with no header, keeping only a short tail that may
// hold the start of a header split across reads, so the search bounds memory
// without losing a sync that straddles a read boundary.
func (s *mp3Stream) dropUnsynced(remLen int) {
	keep := mp3.HeaderLen - 1
	if remLen <= keep {
		// Only a short tail is left: it may be the front of a header split across
		// reads, so keep waiting without disturbing an established sync.
		return
	}
	s.off += remLen - keep
	// A full buffer with no valid header means sync is lost: a later candidate
	// with no lookahead must be reconfirmed, not delivered on the stale flag.
	s.synced = false
	s.gaps++
}

// finish counts a truncated final frame once, called after the EOF drain when
// unconsumed bytes remain that could not form a whole frame.
func (s *mp3Stream) finish() {
	if len(s.buf)-s.off > 0 {
		s.gaps++
	}
}

// compact slides the unconsumed bytes to the front so the buffer tracks the
// backlog rather than the whole stream. It runs after each drain, once every
// returned frame slice has been delivered.
func (s *mp3Stream) compact() {
	if s.off == 0 {
		return
	}
	n := copy(s.buf, s.buf[s.off:])
	s.buf = s.buf[:n]
	s.off = 0
}

// looksLikeID3 reports whether rem begins with an ID3v2 tag, and whether more
// bytes are needed before its 10-byte header can be read.
func looksLikeID3(rem []byte) (isID3, needMore bool) {
	if len(rem) < 3 || rem[0] != 'I' || rem[1] != 'D' || rem[2] != '3' {
		return false, false
	}
	if len(rem) < id3v2HeaderLen {
		return true, true
	}
	// A real ID3v2 tag encodes its size as a syncsafe integer: the top bit of
	// each of the four size bytes is 0. If any is set, the "ID3" bytes are a
	// payload coincidence hit during a resync, not a tag header; treat them as
	// audio to scan rather than skipping a bogus (possibly huge) tag length.
	if rem[6]&0x80 != 0 || rem[7]&0x80 != 0 || rem[8]&0x80 != 0 || rem[9]&0x80 != 0 {
		return false, false
	}
	return true, false
}

// syncsafe decodes the ID3v2 4-byte syncsafe tag size (7 active bits per byte).
func syncsafe(b []byte) int {
	return int(b[0]&0x7F)<<21 | int(b[1]&0x7F)<<14 | int(b[2]&0x7F)<<7 | int(b[3]&0x7F)
}

// scanHeader finds the first valid frame header in rem, returning its index and
// parsed geometry. The 0xFF plus top-three-bits prefilter skips the parse for
// the common non-sync byte before validating a candidate.
func scanHeader(rem []byte) (int, mp3.Header, bool) {
	for i := 0; i+mp3.HeaderLen <= len(rem); i++ {
		if rem[i] != 0xFF || rem[i+1]&0xE0 != 0xE0 {
			continue
		}
		if h, err := mp3.Parse(binary.BigEndian.Uint32(rem[i : i+mp3.HeaderLen])); err == nil {
			return i, h, true
		}
	}
	return 0, mp3.Header{}, false
}

// headerConsistent reports whether the header at the next-frame offset (h's
// FrameLen bytes past the start of rem) is a valid header of the same version,
// layer, and sampling rate. rem must hold at least FrameLen+HeaderLen bytes.
func headerConsistent(rem []byte, h mp3.Header) bool {
	next, err := mp3.Parse(binary.BigEndian.Uint32(rem[h.FrameLen : h.FrameLen+mp3.HeaderLen]))
	if err != nil {
		return false
	}
	return next.Version == h.Version && next.Layer == h.Layer && next.SampleRate == h.SampleRate
}

// setupMP3 configures a compressed MPEG audio (MP3) source. An MP3 stream has no
// container header (framing is inline), so there is nothing to parse during
// Open; the reader frames the body into coded frames instead of fixed-width PCM.
func (c *Client) setupMP3() error {
	c.codec = audiostream.CodecMP3{}
	c.framer = &mp3Stream{buf: make([]byte, 0, readBufSize+mp3MaxFrameLen)}
	return nil
}

// readMP3 is the reader loop for a compressed MPEG audio body. It accumulates
// body bytes into the framer and delivers each whole coded frame, carrying a
// partial frame across reads. It runs until a terminal condition, whose
// shutdown it funnels before returning.
func (c *Client) readMP3() {
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
			c.drainMP3(now)
		}
		if err != nil {
			cause := c.classifyReadErr(err)
			if errors.Is(cause, ErrStreamEnded) {
				// Deliver a final frame that has no following header to confirm it,
				// then count any truncated tail.
				c.framer.ended = true
				c.drainMP3(time.Now())
				c.framer.finish()
				c.malformed.Store(c.framer.gaps)
			}
			c.initiateShutdown(cause)
			return
		}
	}
}

// drainMP3 delivers every whole frame the framer can yield, then compacts its
// buffer and publishes the running discard count.
func (c *Client) drainMP3(now time.Time) {
	for {
		frame, hdr, ok := c.framer.next()
		if !ok {
			break
		}
		c.deliverCompressed(frame, hdr, now)
	}
	c.framer.compact()
	c.malformed.Store(c.framer.gaps)
}

// deliverCompressed counts one coded-frame delivery and hands it to OnFrame. The
// frame's PTS is the presentation time of its first sample, computed from the
// running media time before this frame advances it, so the first frame is at
// PTS 0 and successive PTSs increase by each frame's duration. frame aliases
// reader-owned memory and is valid only during the callback.
func (c *Client) deliverCompressed(frame []byte, hdr mp3.Header, now time.Time) {
	c.packets.Add(1)
	c.payload.Add(uint64(len(frame)))
	pts := c.mediaPTS
	c.advanceMediaPTS(hdr)
	if c.cfg.OnFrame == nil {
		return
	}
	c.cfg.OnFrame(audiostream.Frame{
		TrackID:    0,
		Data:       frame,
		RTPTime:    0,
		PTS:        pts,
		ReceivedAt: now,
		SeqGap:     0,
	})
}

// advanceMediaPTS adds one frame's duration to the running media time. The
// duration is SamplesPerFrame/SampleRate seconds; accumulating it per frame,
// each term bounded by a single frame, avoids the large-multiply overflow the
// PCM path guards against on a long stream.
func (c *Client) advanceMediaPTS(hdr mp3.Header) {
	if hdr.SampleRate <= 0 {
		return
	}
	c.mediaPTS += time.Duration(hdr.SamplesPerFrame) * time.Second / time.Duration(hdr.SampleRate)
}
