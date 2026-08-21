package hlssource

import (
	"fmt"
	"strings"
)

// The test AAC configuration: AAC-LC / 44100 Hz / stereo, whose synthesized
// AudioSpecificConfig is the well-known 0x1210.
const (
	fxProfile = 1 // AAC-LC: audioObjectType 2 (profile+1)
	fxSRIdx   = 4 // 44100 Hz
	fxChanCfg = 2 // stereo
)

var wantASC = []byte{0x12, 0x10}

// adtsHeader hand-encodes a 7-byte ADTS header (no CRC) for the fixed test
// configuration with the given whole-frame length.
func adtsHeader(frameLen int) []byte {
	b := make([]byte, 7)
	b[0] = 0xFF
	b[1] = 0xF1 // syncword low nibble + layer 00 + protection_absent
	b[2] = byte(fxProfile&0x03)<<6 | byte(fxSRIdx&0x0F)<<2 | byte((fxChanCfg>>2)&0x01)
	b[3] = byte(fxChanCfg&0x03)<<6 | byte((frameLen>>11)&0x03)
	b[4] = byte((frameLen >> 3) & 0xFF)
	b[5] = byte((frameLen&0x07)<<5) | 0x1F
	b[6] = 0xFC
	return b
}

// adtsFrame builds one single-block ADTS frame whose payload is filled with
// marker, returning the whole frame and the access unit (payload) a framer
// should deliver.
func adtsFrame(marker byte, payloadLen int) (frame, au []byte) {
	frameLen := 7 + payloadLen
	frame = adtsHeader(frameLen)
	for range payloadLen {
		frame = append(frame, marker)
	}
	return frame, frame[7:]
}

// adtsStream concatenates n distinct single-block ADTS frames, returning the
// stream and the access units in order.
func adtsStream(n, payloadLen int) (stream []byte, aus [][]byte) {
	aus = make([][]byte, n)
	for i := range n {
		frame, au := adtsFrame(byte(i+1), payloadLen)
		aus[i] = au
		stream = append(stream, frame...)
	}
	return stream, aus
}

// tsPacket builds one 188-byte TS packet for pid carrying data, setting PUSI and
// advancing the continuity counter. When data is shorter than the available
// payload space, an adaptation field of stuffing bytes pads the packet to 188.
// data must not exceed the available payload space (184 bytes).
func tsPacket(pid uint16, pusi bool, cc *uint8, data []byte) []byte {
	if len(data) > 184 {
		panic("tsPacket: data exceeds payload space")
	}
	pkt := make([]byte, tsPacketLen)
	pkt[0] = tsSync
	pkt[1] = byte(pid >> 8)
	if pusi {
		pkt[1] |= 0x40
	}
	pkt[2] = byte(pid)
	*cc &= 0x0F
	if len(data) == 184 {
		pkt[3] = 0x10 | *cc // afc=01 payload only
		copy(pkt[4:], data)
	} else {
		pkt[3] = 0x30 | *cc // afc=11 adaptation + payload
		afLen := 183 - len(data)
		pkt[4] = byte(afLen)
		// afLen bytes follow: a flags byte (when afLen>0) then 0xFF stuffing.
		for i := 5; i < 5+afLen; i++ {
			pkt[i] = 0xFF
		}
		if afLen > 0 {
			pkt[5] = 0x00 // adaptation flags: none
		}
		copy(pkt[5+afLen:], data)
	}
	*cc++
	return pkt
}

// buildPAT builds a single-packet PAT mapping program 1 to pmtPID. The CRC is
// left zero; the demux does not validate it.
func buildPAT(pmtPID uint16) []byte {
	sec := []byte{
		0x00,       // table_id
		0xB0, 0x0D, // section_syntax_indicator + section_length = 13
		0x00, 0x01, // transport_stream_id
		0xC1,       // version + current_next
		0x00, 0x00, // section_number, last_section_number
		0x00, 0x01, // program_number 1
		byte(0xE0 | (pmtPID>>8)&0x1F), byte(pmtPID), // reserved + program_map_PID
		0x00, 0x00, 0x00, 0x00, // CRC (ignored)
	}
	var cc uint8
	return tsPacket(patPID, true, &cc, append([]byte{0x00}, sec...))
}

// buildPMT builds a single-packet PMT declaring one elementary stream of
// streamType on audioPID. The CRC is left zero.
func buildPMT(pmtPID, audioPID uint16, streamType byte) []byte {
	body := []byte{
		0x00, 0x01, // program_number
		0xC1,       // version + current_next
		0x00, 0x00, // section_number, last_section_number
		byte(0xE0 | (audioPID>>8)&0x1F), byte(audioPID), // PCR_PID
		0xF0, 0x00, // program_info_length = 0
		streamType,
		byte(0xE0 | (audioPID>>8)&0x1F), byte(audioPID), // elementary_PID
		0xF0, 0x00, // ES_info_length = 0
	}
	sectionLen := len(body) + 4 // + CRC
	sec := make([]byte, 0, 3+len(body)+4)
	sec = append(sec, 0x02, byte(0xB0|(sectionLen>>8)&0x0F), byte(sectionLen))
	sec = append(sec, body...)
	sec = append(sec, 0x00, 0x00, 0x00, 0x00) // CRC (ignored)
	var cc uint8
	return tsPacket(pmtPID, true, &cc, append([]byte{0x00}, sec...))
}

// buildPES wraps an elementary stream in a PES packet with a 5-byte PTS header
// (the demux ignores the PTS value; the header is present to exercise the header
// skip). PES_packet_length is left 0 (unbounded), valid for audio.
func buildPES(es []byte) []byte {
	pes := make([]byte, 0, 14+len(es))
	pes = append(pes,
		0x00, 0x00, 0x01, // packet_start_code_prefix
		0xC0,       // stream_id: audio stream 0
		0x00, 0x00, // PES_packet_length = 0 (unbounded)
		0x80,                         // marker bits '10', no scrambling
		0x80,                         // PTS_DTS_flags = 10
		0x05,                         // PES_header_data_length
		0x21, 0x00, 0x01, 0x00, 0x01, // a 33-bit PTS (value ignored)
	)
	return append(pes, es...)
}

// buildTSSegment builds a whole MPEG-TS segment: one PAT, one PMT declaring an
// AAC (0x0F) elementary stream, then the ADTS stream carried as one PES sliced
// into audioPID packets.
func buildTSSegment(adts []byte, pmtPID, audioPID uint16) []byte {
	return buildTSSegmentType(adts, pmtPID, audioPID, streamTypeAAC)
}

// buildTSSegmentType is buildTSSegment with a caller-chosen PMT stream_type, so a
// test can build a non-AAC segment.
func buildTSSegmentType(es []byte, pmtPID, audioPID uint16, streamType byte) []byte {
	seg := buildPAT(pmtPID)
	seg = append(seg, buildPMT(pmtPID, audioPID, streamType)...)
	pes := buildPES(es)
	var cc uint8
	first := true
	for len(pes) > 0 {
		n := min(184, len(pes))
		seg = append(seg, tsPacket(audioPID, first, &cc, pes[:n])...)
		pes = pes[n:]
		first = false
	}
	return seg
}

// segSpec describes one segment entry for buildMediaPlaylist.
type segSpec struct {
	uri           string
	duration      float64
	discontinuity bool
	gap           bool
}

// buildMediaPlaylist emits a media playlist body for the given segments.
func buildMediaPlaylist(targetDur int, mediaSeq uint64, endList bool, segs []segSpec) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", targetDur)
	fmt.Fprintf(&b, "#EXT-X-MEDIA-SEQUENCE:%d\n", mediaSeq)
	for _, s := range segs {
		if s.discontinuity {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if s.gap {
			b.WriteString("#EXT-X-GAP\n")
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n", s.duration, s.uri)
	}
	if endList {
		b.WriteString("#EXT-X-ENDLIST\n")
	}
	return b.String()
}
