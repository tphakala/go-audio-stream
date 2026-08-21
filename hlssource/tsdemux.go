package hlssource

import (
	"fmt"
	"time"

	"github.com/tphakala/go-audio-stream/internal/adtsframe"
)

// MPEG-TS constants.
const (
	tsPacketLen = 188
	tsSync      = 0x47
	nullPID     = 0x1FFF
	patPID      = 0x0000

	// streamTypeAAC is the ISO/IEC 13818-7 (ADTS AAC) elementary-stream type in
	// the PMT. Other audio stream types (0x03/0x04 MP3, 0x11 MPEG-4 LATM) are
	// recognized only to report ErrUnsupportedCodec rather than a bare
	// "no audio" verdict.
	streamTypeAAC  = 0x0F
	streamTypeMP3a = 0x03
	streamTypeMP3b = 0x04
	streamTypeLATM = 0x11
)

// tsDemux demuxes AAC access units out of an MPEG-TS byte stream, one segment at
// a time. It is long-lived across the segments of a continuity domain: the ADTS
// framer persists between segments so an access unit split across a segment
// boundary is not flushed unconfirmed, and the acquired PAT/PMT/audio PID carry
// forward. A discontinuity resets that domain. It never decodes.
type tsDemux struct {
	pmtPID    uint16
	havePMT   bool // pmtPID has been learned from the PAT
	audioPID  uint16
	haveAudio bool // audioPID has been learned from the PMT

	// PSI section reassembly buffers, one per table PID. A section can span TS
	// packets, so bytes accumulate until table_id + section_length are complete.
	patSec []byte
	pmtSec []byte

	// PES header bytes still to skip on continuation packets, when a PES header
	// ran past the first TS packet of the PES. pesDropping is set when a PES
	// start packet could not be parsed (its header did not fit or its start code
	// was wrong), so the rest of that PES is dropped until the next PES start
	// rather than fed to the framer as if it were the elementary stream.
	pesHdrSkip  int
	pesDropping bool

	framer *adtsframe.Stream
	asc    []byte
	// accumGaps carries the framer discard count across continuity-domain resets,
	// which replace the framer, so gapCount reports a running total for Stats.
	accumGaps uint64

	// audioCC is the continuity_counter of the last payload-bearing packet seen on
	// the audio PID; haveCC guards its validity before the first such packet. A
	// break in the expected sequence means a TS packet was dropped, which would
	// splice non-contiguous audio into the framer, so the framer is reset and the
	// loss is counted distinctly. See handlePacket.
	audioCC uint8
	haveCC  bool
}

// newTSDemux returns a demuxer with a fresh ADTS framer.
func newTSDemux() *tsDemux {
	return &tsDemux{framer: adtsframe.NewStream(tsPacketLen * 8)}
}

// demux processes one whole segment. When discontinuity is true, the continuity
// domain is reset first: any complete trailing frame from the previous domain is
// flushed, then the framer and the acquired PSI state start fresh. Each AAC
// access unit is delivered to onAU in order with its presentation duration. It
// returns ErrMalformedSegment when the bytes carry no usable TS structure,
// ErrUnsupportedCodec when the only audio stream is not AAC, and
// ErrUnsupportedPlaylist when a packet is scrambled (encrypted).
func (d *tsDemux) demux(seg []byte, discontinuity bool, onAU func(au []byte, dur time.Duration)) error {
	if discontinuity {
		d.resetDomain(onAU)
	}
	// The continuity_counter is contiguous only within a single segment's packet
	// stream: a real encoder may restart it at each segment boundary, and segments
	// arrive as whole files over TCP with no packet loss between them. So the CC
	// baseline is re-established per segment rather than carried across the
	// boundary, which would otherwise read every boundary as a dropped packet.
	d.haveCC = false
	start, ok := tsResync(seg)
	if !ok {
		return fmt.Errorf("%w: no MPEG-TS sync byte in segment", ErrMalformedSegment)
	}
	for off := start; off+tsPacketLen <= len(seg); off += tsPacketLen {
		pkt := seg[off : off+tsPacketLen]
		if pkt[0] != tsSync {
			// Lost alignment mid-segment: try to recover to the next sync.
			if r, ok := tsResync(seg[off:]); ok {
				off = off + r - tsPacketLen // -tsPacketLen offsets the loop's +=
				continue
			}
			break
		}
		if err := d.handlePacket(pkt, onAU); err != nil {
			return err
		}
	}
	return nil
}

// handlePacket routes one 188-byte TS packet by PID after locating its payload.
func (d *tsDemux) handlePacket(pkt []byte, onAU func(au []byte, dur time.Duration)) error {
	if pkt[1]&0x80 != 0 { // transport_error_indicator
		return nil
	}
	if pkt[3]&0xC0 != 0 { // transport_scrambling_control
		return fmt.Errorf("%w: scrambled (encrypted) MPEG-TS packet", ErrUnsupportedPlaylist)
	}
	pusi := pkt[1]&0x40 != 0
	pid := uint16(pkt[1]&0x1F)<<8 | uint16(pkt[2])
	if pid == nullPID {
		return nil
	}
	// The audio-PID discontinuity_indicator announces a splice and may ride on a
	// packet with no payload (adaptation-only, afc=0b10), so honor it before the
	// no-payload early return below drops such a packet: reset the framer and drop
	// the continuity_counter baseline so the following payload packet's counter
	// break is expected, not counted as a dropped packet.
	if d.haveAudio && pid == d.audioPID && tsDiscIndicator(pkt) {
		d.resetContinuity(onAU)
		d.haveCC = false
	}
	payload, ok := tsPayload(pkt)
	if !ok {
		return nil // adaptation-only or no payload
	}
	switch {
	case pid == patPID:
		if sec, ok := psiSection(&d.patSec, payload, pusi); ok {
			d.parsePAT(sec)
		}
	case d.havePMT && pid == d.pmtPID:
		if sec, ok := psiSection(&d.pmtSec, payload, pusi); ok {
			if err := d.parsePMT(sec); err != nil {
				return err
			}
		}
	case d.haveAudio && pid == d.audioPID:
		// An announced discontinuity was already handled above (framer reset,
		// haveCC cleared), so such a packet falls through the !haveCC baseline here
		// rather than being counted as a dropped packet.
		cc := pkt[3] & 0x0F
		switch {
		case !d.haveCC:
			// First audio packet of the segment, or the packet after an announced
			// discontinuity; nothing to compare against.
		case cc == d.audioCC:
			// A permitted single duplicate (ISO/IEC 13818-1 allows one repeat):
			// dropping it keeps a repeated partial frame from being spliced into the
			// framer.
			return nil
		case cc == (d.audioCC+1)&0x0F:
			// Contiguous, the expected case.
		default:
			// A gap in the counter means a TS packet was dropped: the audio bytes
			// that follow are not contiguous with what the framer holds. Reset the
			// framer and count the loss so it surfaces distinctly in Stats rather
			// than only as downstream resync corruption.
			d.resetContinuity(onAU)
			d.accumGaps++
		}
		d.audioCC = cc
		d.haveCC = true
		d.handlePES(payload, pusi, onAU)
	}
	return nil
}

// handlePES feeds the AAC elementary stream carried by an audio-PID packet to the
// framer and drains whole access units. On a PES start (PUSI), the PES header is
// skipped to reach the elementary stream; a header that runs past this packet is
// skipped across the following continuation packets.
func (d *tsDemux) handlePES(payload []byte, pusi bool, onAU func(au []byte, dur time.Duration)) {
	if pusi {
		d.pesHdrSkip = 0
		d.pesDropping = false
		// packet_start_code_prefix (0x000001) then stream_id, PES_packet_length,
		// two flag bytes, and PES_header_data_length at byte 8. A start packet
		// whose payload cannot even hold the fixed 9-byte header (a large
		// adaptation field), or whose start code is wrong, is not a PES header we
		// can locate the elementary stream within: drop the PES until the next
		// start rather than feed its header bytes to the framer as audio.
		if len(payload) < 9 || payload[0] != 0x00 || payload[1] != 0x00 || payload[2] != 0x01 {
			d.pesDropping = true
			return
		}
		esStart := 9 + int(payload[8])
		if esStart <= len(payload) {
			d.feed(payload[esStart:], onAU)
		} else {
			d.pesHdrSkip = esStart - len(payload)
		}
		return
	}
	if d.pesDropping {
		return
	}
	if d.pesHdrSkip > 0 {
		skip := min(d.pesHdrSkip, len(payload))
		payload = payload[skip:]
		d.pesHdrSkip -= skip
	}
	d.feed(payload, onAU)
}

// feed appends elementary-stream bytes to the framer and delivers every whole
// access unit it can yield, capturing the AudioSpecificConfig from the first one.
func (d *tsDemux) feed(es []byte, onAU func(au []byte, dur time.Duration)) {
	if len(es) == 0 {
		return
	}
	d.framer.Feed(es)
	for {
		au, dur, ok := d.framer.NextFrame()
		if !ok {
			break
		}
		if d.asc == nil {
			d.asc = d.framer.Header().AudioSpecificConfig()
		}
		onAU(au, dur)
	}
	d.framer.Compact()
}

// end flushes the final buffered frame at true stream end (VOD ENDLIST fully
// consumed, or Close), delivering a last access unit that has no following header
// to confirm it.
func (d *tsDemux) end(onAU func(au []byte, dur time.Duration)) {
	d.framer.SetEOF()
	for {
		au, dur, ok := d.framer.NextFrame()
		if !ok {
			break
		}
		if d.asc == nil {
			d.asc = d.framer.Header().AudioSpecificConfig()
		}
		onAU(au, dur)
	}
	d.framer.Finish()
}

// resetContinuity flushes any complete trailing frame from the current framer,
// then starts a fresh framer and PES-assembly state. It preserves the acquired
// PAT/PMT/audio PID, so it is the reset for a mid-segment continuity break (a
// dropped or spliced TS packet) where the mux is unchanged but the audio bytes
// are no longer contiguous. The retiring framer's discard count is folded into
// accumGaps so Stats keeps a running total across the reset.
func (d *tsDemux) resetContinuity(onAU func(au []byte, dur time.Duration)) {
	d.end(onAU)
	d.accumGaps += d.framer.GapCount()
	d.framer = adtsframe.NewStream(tsPacketLen * 8)
	d.pesHdrSkip = 0
	d.pesDropping = false
}

// resetDomain resets the continuity domain across an EXT-X-DISCONTINUITY: on top
// of the framer reset it drops the acquired PSI and PES state, and the audio-PID
// continuity_counter expectation, so a discontinuity's independent timeline and
// mux are re-learned.
func (d *tsDemux) resetDomain(onAU func(au []byte, dur time.Duration)) {
	d.resetContinuity(onAU)
	d.patSec = nil
	d.pmtSec = nil
	d.havePMT = false
	d.haveAudio = false
	d.haveCC = false
}

// audioSpecificConfig returns the AAC AudioSpecificConfig resolved from the first
// delivered frame, or nil when no frame has been delivered yet.
func (d *tsDemux) audioSpecificConfig() []byte { return d.asc }

// gapCount is the running framer discard count across every continuity domain,
// surfaced by the client as the source's malformed counter.
func (d *tsDemux) gapCount() uint64 { return d.accumGaps + d.framer.GapCount() }

// parsePAT reads the first program's program_map_PID from a complete PAT section.
func (d *tsDemux) parsePAT(sec []byte) {
	body, ok := psiBody(sec, 0x00)
	if !ok {
		return
	}
	// Each program entry is 4 bytes: program_number (2), reserved+PID (2).
	for i := 0; i+4 <= len(body); i += 4 {
		prog := uint16(body[i])<<8 | uint16(body[i+1])
		pid := uint16(body[i+2]&0x1F)<<8 | uint16(body[i+3])
		if prog != 0 { // program 0 is the network PID, not a program map
			d.pmtPID = pid
			d.havePMT = true
			return
		}
	}
}

// parsePMT finds the AAC audio elementary PID in a complete PMT section. A PMT
// whose only audio stream is a non-AAC type is ErrUnsupportedCodec; a PMT with no
// audio stream leaves haveAudio false, which the caller treats as a segment with
// no audio.
func (d *tsDemux) parsePMT(sec []byte) error {
	body, ok := psiBody(sec, 0x02)
	if !ok {
		return nil
	}
	// PMT body: PCR_PID (2), program_info_length (2, low 12 bits), descriptors,
	// then the elementary-stream loop.
	if len(body) < 4 {
		return nil
	}
	pil := int(body[2]&0x0F)<<8 | int(body[3])
	i := 4 + pil
	sawOtherAudio := false
	for i+5 <= len(body) {
		streamType := body[i]
		pid := uint16(body[i+1]&0x1F)<<8 | uint16(body[i+2])
		esil := int(body[i+3]&0x0F)<<8 | int(body[i+4])
		i += 5 + esil
		switch streamType {
		case streamTypeAAC:
			d.audioPID = pid
			d.haveAudio = true
			return nil
		case streamTypeMP3a, streamTypeMP3b, streamTypeLATM:
			sawOtherAudio = true
		}
	}
	if !d.haveAudio && sawOtherAudio {
		return fmt.Errorf("%w: the only audio stream is not AAC-in-ADTS", ErrUnsupportedCodec)
	}
	return nil
}

// tsResync returns the offset of the first byte from which 0x47 recurs at the
// 188-byte cadence, so a stream with leading garbage or that is not 188-aligned
// still parses. A lone trailing 0x47 with no confirming packet after it is
// accepted only when the buffer is too short to confirm.
func tsResync(b []byte) (int, bool) {
	for i := range b {
		if b[i] != tsSync {
			continue
		}
		next := i + tsPacketLen
		if next >= len(b) || b[next] == tsSync {
			return i, true
		}
	}
	return 0, false
}

// tsPayload returns the payload bytes of a TS packet, honoring the
// adaptation_field_control and the adaptation-field length. ok is false when the
// packet carries no payload (adaptation-only or reserved) or the adaptation
// length overruns the packet.
func tsPayload(pkt []byte) ([]byte, bool) {
	afc := (pkt[3] >> 4) & 0x03
	switch afc {
	case 0x01: // payload only
		return pkt[4:], true
	case 0x03: // adaptation field then payload
		afLen := int(pkt[4])
		start := 5 + afLen
		if start >= tsPacketLen {
			return nil, false
		}
		return pkt[start:], true
	default: // 0x00 reserved, 0x02 adaptation only
		return nil, false
	}
}

// tsDiscIndicator reports the adaptation-field discontinuity_indicator, which an
// encoder sets to announce an intentional splice (a new timeline) so the
// continuity_counter break that follows is expected rather than a dropped
// packet. It is false for a packet with no adaptation field or an empty one.
func tsDiscIndicator(pkt []byte) bool {
	afc := (pkt[3] >> 4) & 0x03
	// Only afc 0b10 (adaptation only) and 0b11 (adaptation then payload) carry an
	// adaptation field. Both reach here: handlePacket checks this on the audio PID
	// before the no-payload early return, so a splice announced on an adaptation-
	// only (0b10) packet is honored too.
	if afc != 0x02 && afc != 0x03 {
		return false
	}
	afLen := int(pkt[4])
	if afLen == 0 {
		return false // no adaptation flags byte present
	}
	// pkt is a full 188-byte packet, so pkt[5] is in range whenever afLen > 0.
	return pkt[5]&0x80 != 0
}

// psiSection assembles a PSI section that may span TS packets. On a PUSI packet
// the pointer_field selects the section start and a new section begins; a
// continuation packet appends. It returns the complete section (table_id through
// CRC) once table_id and section_length are satisfied.
func psiSection(buf *[]byte, payload []byte, pusi bool) ([]byte, bool) {
	if pusi {
		if len(payload) < 1 {
			return nil, false
		}
		ptr := int(payload[0])
		if 1+ptr > len(payload) {
			return nil, false
		}
		*buf = append((*buf)[:0], payload[1+ptr:]...)
	} else {
		if len(*buf) == 0 {
			return nil, false // continuation with no started section
		}
		*buf = append(*buf, payload...)
	}
	sec := *buf
	if len(sec) < 3 {
		return nil, false
	}
	// section_length is 12 bits, so total is at most 3+4095; the buffer therefore
	// completes (and clears) within 4098 bytes and cannot grow without bound.
	total := 3 + (int(sec[1]&0x0F)<<8 | int(sec[2]))
	if len(sec) < total {
		return nil, false
	}
	// Clear the buffer once the section is complete, so subsequent continuation
	// packets for this PID (trailing stuffing, or a crafted replay) neither
	// re-parse the same section nor grow the buffer without bound. sec still
	// points at the (unzeroed) backing array and is consumed synchronously by the
	// caller before the next packet reuses it.
	*buf = (*buf)[:0]
	return sec[:total], true
}

// psiBody validates a section's table_id and returns the bytes between the
// 5-byte section header (table_id, section_length, table_id_extension, version,
// section_number, last_section_number) and the trailing 4-byte CRC. ok is false
// when the table_id does not match or the section is too short to hold a body.
func psiBody(sec []byte, tableID byte) ([]byte, bool) {
	if len(sec) < 12 || sec[0] != tableID { // 3 prefix + 5 header + 4 CRC minimum
		return nil, false
	}
	sectionLen := int(sec[1]&0x0F)<<8 | int(sec[2])
	end := 3 + sectionLen
	if end > len(sec) || end < 3+5+4 {
		return nil, false
	}
	// Body is after the 3-byte prefix and 5-byte header, before the 4-byte CRC.
	return sec[8 : end-4], true
}
