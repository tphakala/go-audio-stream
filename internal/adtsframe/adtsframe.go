// Package adtsframe frames a raw ADTS (MPEG-2/4 AAC) byte stream into whole raw
// access units. It buffers bytes across feeds, resynchronizes past leading
// garbage or a false sync with a next-frame consistency check, strips each
// frame's ADTS header, and drops a multi-block frame it cannot split. It never
// decodes.
//
// It is the shared framer behind the httpsource (progressive/Icecast ADTS AAC)
// and hlssource (AAC elementary stream demuxed from MPEG-TS) sources, so both
// cut an ADTS byte stream the same way and both synthesize the byte-identical
// AudioSpecificConfig an RTSP AAC track resolves from its SDP. A Stream is not
// safe for concurrent use; the owning reader goroutine drives it.
package adtsframe

import (
	"time"

	"github.com/tphakala/go-audio-stream/internal/adts"
)

// Stream frames a raw ADTS byte stream into raw access units. Construct one with
// NewStream so its buffer is pre-sized; the zero value works but reallocates on
// the first feeds. It is not safe for concurrent use.
type Stream struct {
	// buf holds unconsumed bytes; off is the read cursor into it. A frame is
	// returned as a sub-slice of buf[off:], valid only until the next Feed or
	// Compact, matching the library's reader-owns-Data contract.
	buf []byte
	off int
	// synced is set once a frame has been confirmed by a following valid header.
	// A synced stream trusts a frame with no lookahead left (the stream tail).
	synced bool
	// ended is set at end of stream: the final frame is then delivered without a
	// following header to confirm it.
	ended bool
	// gaps counts discard events (leading garbage, a false sync, a dropped
	// multi-block frame, or a truncated final frame). It is the framer's
	// malformed-frame equivalent.
	gaps uint64
}

// NewStream returns a Stream whose buffer is pre-allocated to sizeHint bytes, so
// a steady feed of that size plus a frame's worth of lookahead does not
// repeatedly grow it. A non-positive sizeHint just starts from an empty buffer.
func NewStream(sizeHint int) *Stream {
	if sizeHint < 0 {
		sizeHint = 0
	}
	return &Stream{buf: make([]byte, 0, sizeHint)}
}

// Feed appends freshly read bytes to the buffer.
func (s *Stream) Feed(p []byte) { s.buf = append(s.buf, p...) }

// NextFrame returns the next deliverable raw access unit (its ADTS header
// stripped) and its presentation duration, or ok=false when the buffer holds no
// confirmable whole frame yet. A multi-block frame is dropped and counted, since
// the fixed header carries no per-block boundary to split it on. The returned
// slice aliases the buffer and is valid only until the next Feed or Compact.
func (s *Stream) NextFrame() (data []byte, dur time.Duration, ok bool) {
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
			if !Consistent(rem[h.FrameLen:], h) {
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
		return frame[h.HeaderLen:], frameDuration(h), true
	}
}

// Compact slides the unconsumed bytes to the front so the buffer tracks the
// backlog rather than the whole stream. It runs after each drain, once every
// returned frame slice has been delivered.
func (s *Stream) Compact() {
	if s.off == 0 {
		return
	}
	n := copy(s.buf, s.buf[s.off:])
	s.buf = s.buf[:n]
	s.off = 0
}

// SetEOF marks the stream ended so the final frame is delivered without a
// following header to confirm it.
func (s *Stream) SetEOF() { s.ended = true }

// Finish counts a truncated final frame once, called after the EOF drain when
// unconsumed bytes remain that could not form a whole frame.
func (s *Stream) Finish() {
	if len(s.buf)-s.off > 0 {
		s.gaps++
	}
}

// GapCount is the running discard count, surfaced as the source's malformed
// counter.
func (s *Stream) GapCount() uint64 { return s.gaps }

// discard advances past n unusable bytes and counts one gap.
func (s *Stream) discard(n int) {
	s.off += n
	s.gaps++
}

// dropUnsynced discards a run with no header, keeping only a short tail that may
// hold the start of a header split across feeds, so the search bounds memory
// without losing a sync that straddles a feed boundary.
func (s *Stream) dropUnsynced(remLen int) {
	keep := adts.MinHeaderLen - 1
	if remLen <= keep {
		// Only a short tail is left: it may be the front of a header split
		// across feeds, so keep waiting without disturbing an established sync.
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

// Consistent reports whether next begins with a valid ADTS header carrying the
// same codec configuration (object type, sampling index, channel config) as h.
// It is the false-sync guard: a coincidental sync inside a frame's payload will
// not usually parse, and if it does will not carry the same config. Frame length
// is deliberately not compared, since a VBR stream varies it per frame. next
// must hold at least adts.MinHeaderLen bytes. It is exported so an Open-time ASC
// probe (httpsource) can confirm a candidate frame the same way the framer does.
func Consistent(next []byte, h adts.Header) bool {
	n, err := adts.Parse(next)
	if err != nil {
		return false
	}
	return n.AudioObjectType == h.AudioObjectType &&
		n.SampleRateIndex == h.SampleRateIndex &&
		n.ChannelConfig == h.ChannelConfig
}

// frameDuration is one AAC frame's presentation duration, 1024 samples over the
// frame's sample rate. A non-positive rate, which a parsed header never
// produces, yields 0 so a malformed frame does not advance the clock.
func frameDuration(h adts.Header) time.Duration {
	if h.SampleRate <= 0 {
		return 0
	}
	return time.Duration(adts.SamplesPerFrame) * time.Second / time.Duration(h.SampleRate)
}
