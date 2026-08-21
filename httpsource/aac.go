package httpsource

import (
	"fmt"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/internal/adts"
	"github.com/tphakala/go-audio-stream/internal/adtsframe"
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

// setupAAC configures a compressed AAC source from an ADTS byte stream (Icecast
// or SHOUTcast AAC, or a progressive .aac response). The AudioSpecificConfig a
// decoder needs is carried by no container header, so setupAAC peeks the first
// frame's ADTS header during Open and synthesizes the ASC from it, keeping
// Format stable before the reader spawns. The peek does not consume the audio,
// so the reader frames the whole body from this first frame; only a leading
// ID3v2 metadata tag, if present, is consumed first. A CodecAAC track from
// this source is then indistinguishable to a consumer from an RTSP AAC track:
// raw access units plus the ASC. The ADTS framing itself is the shared
// internal/adtsframe.Stream, so this source and hlssource cut an ADTS stream the
// same way.
func (c *Client) setupAAC() error {
	h, err := c.probeADTS()
	if err != nil {
		return err
	}
	c.codec = audiostream.CodecAAC{AudioSpecificConfig: h.AudioSpecificConfig()}
	c.framer = adtsframe.NewStream(readBufSize + adtsMaxFrameLen)
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
		head, err := c.br.Peek(id3v2HeaderLen)
		isID3, needMore := looksLikeID3(head)
		if !isID3 || needMore {
			// No tag, or too few bytes to be one. If the prefix ran short because
			// the open phase stalled or the transport failed rather than a
			// genuinely short stream, surface that here instead of leaning on the
			// probe's own Peek to re-trip the buffered error (issue #92). A clean
			// short read classifies as nil and lets the probe proceed as before.
			if oe := c.classifyOpenRead(err); oe != nil {
				return oe
			}
			return nil
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
			// A stall that tripped the open deadline, a caller cancellation, or a
			// transport failure while skipping the tag is a transient open-phase
			// failure, not a format verdict (issue #92). A clean short read (a tag
			// the stream never finishes delivering) falls through to
			// ErrFormatUnknown, the pre-#92 behavior.
			if oe := c.classifyOpenRead(err); oe != nil {
				return oe
			}
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
	head, perr := c.br.Peek(n)
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
			if adtsframe.Consistent(head[end:], h) {
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
	// No confirmable ADTS header in the prefix. If the prefix ended because the
	// open phase stalled, the caller cancelled, or the transport failed, that is
	// a transient open-phase failure, not proof the body is not AAC: report it
	// through the open-phase taxonomy rather than as a format verdict (issue #92).
	// A clean short read leaves perr as an EOF the classifier ignores, so a
	// genuinely non-ADTS prefix still fails with ErrFormatUnknown.
	if oe := c.classifyOpenRead(perr); oe != nil {
		return adts.Header{}, oe
	}
	return adts.Header{}, fmt.Errorf("%w: no ADTS frame header in the stream prefix", ErrFormatUnknown)
}
