package httpsource

import (
	"fmt"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/adts"
)

// adtsMaxFrameLen is the largest ADTS frame the 13-bit aac_frame_length can
// express. It sizes the framer's initial buffer so a whole frame plus the next
// header's lookahead fit without repeated growth; a typical frame is far smaller.
const adtsMaxFrameLen = 8191

// adtsProbeLen bounds how many leading body bytes setupAAC peeks to resolve the
// first frame's AudioSpecificConfig during Open. It comfortably spans a typical
// frame plus a following header for the consistency check; a rare larger frame
// is adopted without the second-frame confirmation rather than read further, so
// Open never waits on an unbounded prefix.
const adtsProbeLen = 2048

// maxID3v2SkipBytes bounds how much of a leading ID3v2 tag Open consumes to reach
// the first ADTS frame. A syncsafe tag size permits up to 2^28-1 (~256 MiB); a
// real album-art tag is at most a few MiB, so a larger declared length is a
// mislabelled or hostile stream. Capping the skip keeps Open's read bounded for
// an untrusted source instead of streaming up to the open-phase timeout.
const maxID3v2SkipBytes = 16 << 20 // 16 MiB

// adtsStream frames a raw ADTS (MPEG-2/4 AAC) byte stream into raw access units.
// It buffers bytes across reads, resynchronizes past leading garbage or a false
// sync with a next-frame consistency check, strips each frame's ADTS header, and
// drops a multi-block frame it cannot split. It never decodes. It is not safe
// for concurrent use; the reader goroutine owns it.
type adtsStream struct {
	// buf holds unconsumed body bytes; off is the read cursor into it. A frame is
	// returned as a sub-slice of buf[off:], valid only until the next feed or
	// compact, matching the library's reader-owns-Data contract.
	buf []byte
	off int
	// synced is set once a frame has been confirmed by a following valid header.
	// A synced stream trusts a frame with no lookahead left (the stream tail).
	synced bool
	// ended is set at EOF: the final frame is then delivered without a following
	// header to confirm it.
	ended bool
	// gaps counts discard events (leading garbage, a false sync, a dropped
	// multi-block frame, or a truncated final frame). It is the framer's
	// malformed-frame equivalent.
	gaps uint64
}

// compile-time: adtsStream implements compressedFramer.
var _ compressedFramer = (*adtsStream)(nil)

// feed appends freshly read body bytes to the buffer.
func (s *adtsStream) feed(p []byte) { s.buf = append(s.buf, p...) }

// nextFrame returns the next deliverable raw access unit (its ADTS header
// stripped) and its duration, or ok=false when the buffer holds no confirmable
// whole frame yet. A multi-block frame is dropped and counted, since the fixed
// header carries no per-block boundary to split it on. The returned slice
// aliases the buffer and is valid only until the next feed or compact.
func (s *adtsStream) nextFrame() (data []byte, dur time.Duration, ok bool) {
	for {
		rem := s.buf[s.off:]
		i, h, found := scanADTS(rem)
		if !found {
			s.dropUnsynced(len(rem))
			return nil, 0, false
		}
		if i > 0 {
			// Bytes before the header are garbage: sync is lost, so the next
			// candidate must be reconfirmed rather than trusted on the stale flag.
			s.discard(i)
			s.synced = false
			rem = s.buf[s.off:]
		}
		if len(rem) < h.FrameLen {
			return nil, 0, false // wait for the whole frame
		}
		if len(rem) >= h.FrameLen+adts.MinHeaderLen {
			if !adtsConsistent(rem[h.FrameLen:], h) {
				// A false sync: the bytes at the next-frame offset are not a
				// consistent ADTS header. Advance one byte and rescan.
				s.discard(1)
				s.synced = false
				continue
			}
			s.synced = true
		} else if !s.synced && !s.ended {
			// The first sync cannot be confirmed yet and more bytes may arrive.
			return nil, 0, false
		}
		frame := s.buf[s.off : s.off+h.FrameLen]
		s.off += h.FrameLen
		if h.NumRawBlocks > 0 {
			// Multi-block ADTS: the fixed header gives no per-block boundary, so
			// the access units cannot be separated without a full decode. Drop
			// the frame and count it, rather than deliver concatenated blocks a
			// decoder would misread as one access unit.
			s.gaps++
			continue
		}
		return frame[h.HeaderLen:], adtsFrameDuration(h), true
	}
}

// compact slides the unconsumed bytes to the front so the buffer tracks the
// backlog rather than the whole stream. It runs after each drain, once every
// returned frame slice has been delivered.
func (s *adtsStream) compact() {
	if s.off == 0 {
		return
	}
	n := copy(s.buf, s.buf[s.off:])
	s.buf = s.buf[:n]
	s.off = 0
}

// setEOF marks the stream ended so the final frame is delivered without a
// following header to confirm it.
func (s *adtsStream) setEOF() { s.ended = true }

// finish counts a truncated final frame once, called after the EOF drain when
// unconsumed bytes remain that could not form a whole frame.
func (s *adtsStream) finish() {
	if len(s.buf)-s.off > 0 {
		s.gaps++
	}
}

// gapCount is the running discard count, surfaced as the source's malformed
// counter.
func (s *adtsStream) gapCount() uint64 { return s.gaps }

// discard advances past n unusable bytes and counts one gap.
func (s *adtsStream) discard(n int) {
	s.off += n
	s.gaps++
}

// dropUnsynced discards a run with no header, keeping only a short tail that may
// hold the start of a header split across reads, so the search bounds memory
// without losing a sync that straddles a read boundary.
func (s *adtsStream) dropUnsynced(remLen int) {
	keep := adts.MinHeaderLen - 1
	if remLen <= keep {
		// Only a short tail is left: it may be the front of a header split
		// across reads, so keep waiting without disturbing an established sync.
		return
	}
	s.off += remLen - keep
	// A full buffer with no valid header means sync is lost: a later candidate
	// with no lookahead must be reconfirmed, not delivered on the stale flag.
	s.synced = false
	s.gaps++
}

// scanADTS finds the first valid ADTS frame header in rem, returning its index
// and parsed geometry. The 0xFF plus syncword-low-nibble-and-layer prefilter
// (b[1] high nibble 0xF and layer bits 0) skips the parse for the common
// non-sync byte before validating a candidate.
func scanADTS(rem []byte) (int, adts.Header, bool) {
	for i := 0; i+adts.MinHeaderLen <= len(rem); i++ {
		if rem[i] != 0xFF || rem[i+1]&0xF6 != 0xF0 {
			continue
		}
		if h, err := adts.Parse(rem[i:]); err == nil {
			return i, h, true
		}
	}
	return 0, adts.Header{}, false
}

// adtsConsistent reports whether next begins with a valid ADTS header carrying
// the same codec configuration (object type, sampling index, channel config) as
// h. It is the false-sync guard: a coincidental sync inside a frame's payload
// will not usually parse, and if it does will not carry the same config. Frame
// length is deliberately not compared, since a VBR stream varies it per frame.
// next must hold at least adts.MinHeaderLen bytes.
func adtsConsistent(next []byte, h adts.Header) bool {
	n, err := adts.Parse(next)
	if err != nil {
		return false
	}
	return n.AudioObjectType == h.AudioObjectType &&
		n.SampleRateIndex == h.SampleRateIndex &&
		n.ChannelConfig == h.ChannelConfig
}

// adtsFrameDuration is one AAC frame's presentation duration, 1024 samples over
// the frame's sample rate. A non-positive rate, which a parsed header never
// produces, yields 0 so a malformed frame does not advance the clock.
func adtsFrameDuration(h adts.Header) time.Duration {
	if h.SampleRate <= 0 {
		return 0
	}
	return time.Duration(adts.SamplesPerFrame) * time.Second / time.Duration(h.SampleRate)
}

// setupAAC configures a compressed AAC source from an ADTS byte stream (Icecast
// or SHOUTcast AAC, or a progressive .aac response). The AudioSpecificConfig a
// decoder needs is carried by no container header, so setupAAC peeks the first
// frame's ADTS header during Open and synthesizes the ASC from it, keeping
// Format stable before the reader spawns. The peek does not consume the audio,
// so the reader frames the whole body from this first frame; only a leading
// ID3v2 metadata tag, if present, is consumed first. A CodecAAC track from
// this source is then indistinguishable to a consumer from an RTSP AAC track:
// raw access units plus the ASC.
func (c *Client) setupAAC() error {
	h, err := c.probeADTS()
	if err != nil {
		return err
	}
	c.codec = audiostream.CodecAAC{AudioSpecificConfig: h.AudioSpecificConfig()}
	c.framer = &adtsStream{buf: make([]byte, 0, readBufSize+adtsMaxFrameLen)}
	return nil
}

// skipLeadingID3 consumes any leading ID3v2 tag(s) from the buffered reader so
// the ASC probe, and then the reader, begins at the first ADTS frame. A body
// served as a static .aac file commonly carries such a tag, often with album art
// that exceeds the reader buffer; live Icecast and SHOUTcast AAC carry none. The
// tag is consumed by its declared length rather than scanned past, so a tag
// larger than the buffer (which Peek cannot see beyond) is handled uniformly and
// sync-looking bytes inside binary album-art data cannot seed a false frame. A
// declared length above maxID3v2SkipBytes is rejected before any bytes are read,
// so a mislabelled or hostile response cannot make Open stream a huge prefix. A
// stream with no tag, or too short to hold a tag header, is left untouched for
// the probe to classify. Consecutive tags are consumed in turn, matching the MP3
// framer's skip loop.
func (c *Client) skipLeadingID3() error {
	for {
		head, _ := c.br.Peek(id3v2HeaderLen)
		isID3, needMore := looksLikeID3(head)
		if !isID3 || needMore {
			return nil // no tag, or too few bytes to be one: let the probe proceed
		}
		// syncsafe caps the body at 2^28-1 (~256 MiB), so the length fits an int on
		// every supported architecture; this cast cannot overflow. Reject a tag
		// larger than the cap before reading it, so an untrusted source cannot make
		// Open read and discard a very large prefix (bounded only by the timeout).
		tagLen := int(id3v2TagLen(head))
		if tagLen > maxID3v2SkipBytes {
			return fmt.Errorf("%w: ID3v2 tag length %d exceeds the %d-byte limit", ErrFormatUnknown, tagLen, maxID3v2SkipBytes)
		}
		if _, err := c.br.Discard(tagLen); err != nil {
			// The stream stalled or dropped while skipping the tag during Open.
			// Surface it as a format error, consistent with the probe's existing
			// read-error handling; issue #92 will refine open-phase read-error
			// classification across the WAV and AAC header parsers.
			return fmt.Errorf("%w: skipping leading ID3v2 tag: %w", ErrFormatUnknown, err)
		}
	}
}

// probeADTS peeks the buffered body prefix and returns the first ADTS header it
// can accept, without consuming it. It first skips a leading ID3v2 tag (see
// skipLeadingID3), so a static .aac file that carries one still resolves. It
// confirms a candidate against the following frame's header when the prefix
// reaches it, so a coincidental sync in leading noise is rejected. A candidate at
// offset 0 whose frame is too large to confirm within the prefix is trusted,
// since a body labeled AAC begins with a real frame; an unconfirmable candidate
// found only after skipped leading bytes is NOT adopted, so a coincidental sync
// in that noise cannot seed a wrong ASC. A prefix with no confirmable ADTS header
// fails Open rather than delivering unframed bytes.
func (c *Client) probeADTS() (adts.Header, error) {
	if err := c.skipLeadingID3(); err != nil {
		return adts.Header{}, err
	}
	n := adtsProbeLen
	if sz := c.br.Size(); n > sz {
		n = sz
	}
	// A short read returns fewer bytes with a non-nil error; scan whatever the
	// prefix held rather than failing on a stream shorter than the probe window.
	head, _ := c.br.Peek(n)
	for i := 0; i+adts.MinHeaderLen <= len(head); i++ {
		if head[i] != 0xFF || head[i+1]&0xF6 != 0xF0 {
			continue
		}
		h, err := adts.Parse(head[i:])
		if err != nil {
			continue
		}
		end := i + h.FrameLen
		if end+adts.MinHeaderLen <= len(head) {
			if adtsConsistent(head[end:], h) {
				return h, nil
			}
			continue // a following header is present but inconsistent: a false sync
		}
		if i == 0 {
			// The frame runs past the prefix and cannot be confirmed here; trust
			// it only at offset 0, where a body labeled AAC really begins.
			return h, nil
		}
		// Unconfirmable and not at offset 0: keep scanning for a confirmable one.
	}
	return adts.Header{}, fmt.Errorf("%w: no ADTS frame header in the stream prefix", ErrFormatUnknown)
}
