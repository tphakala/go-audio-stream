package rtp

import (
	"encoding/binary"
	"errors"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

const (
	// PTSenderReport is the RTCP Sender Report packet type (RFC 3550).
	PTSenderReport = 200
	// PTReceiverReport is the RTCP Receiver Report packet type.
	PTReceiverReport = 201

	// senderInfoSize is the byte length of an SR header plus its sender
	// info block, before any reception report blocks.
	senderInfoSize = 28
	// maxReportBlocks is the largest block count the 5-bit RC field can
	// express.
	maxReportBlocks = 31
	// rrHeaderSize is the byte length of an RR header plus the reporter
	// SSRC, before any reception report blocks.
	rrHeaderSize = 8
	// reportBlockSize is the byte length of one reception report block.
	reportBlockSize = 24
)

var (
	// ErrShortRTCP is returned when a compound sub-packet is shorter than
	// the 4-byte RTCP header.
	ErrShortRTCP = errors.New("rtp: RTCP packet shorter than header")
	// ErrRTCPVersion is returned when an RTCP sub-packet version is not 2.
	ErrRTCPVersion = errors.New("rtp: unsupported RTCP version")
	// ErrRTCPLength is returned when a sub-packet's length field runs past
	// the end of the buffer.
	ErrRTCPLength = errors.New("rtp: RTCP length field exceeds buffer")
)

// SenderReport holds the sender-info fields of an RTCP Sender Report
// (RFC 3550 section 6.4.1). Report blocks are not retained.
type SenderReport struct {
	// SSRC is the synchronization source of the sender.
	SSRC uint32
	// NTPTimestamp is the 64-bit NTP timestamp (high 32 bits seconds,
	// low 32 bits fraction).
	NTPTimestamp uint64
	// RTPTimestamp is the RTP timestamp corresponding to NTPTimestamp.
	RTPTimestamp uint32
	// PacketCount is the sender's cumulative packet count.
	PacketCount uint32
	// OctetCount is the sender's cumulative payload octet count.
	OctetCount uint32
}

// ParseCompound parses an RTCP compound packet and returns every Sender
// Report it contains, in order. Non-SR sub-packets (Receiver Report,
// SDES, BYE, APP) are skipped by their length field. A sub-packet whose
// length runs past the buffer is a hard error; an SR too short to hold
// the sender info is skipped. ParseCompound never panics.
func ParseCompound(buf []byte) ([]SenderReport, error) {
	var reports []SenderReport
	for off := 0; off < len(buf); {
		if len(buf)-off < 4 {
			return nil, ErrShortRTCP
		}
		if buf[off]>>6 != 2 {
			return nil, ErrRTCPVersion
		}
		pt := buf[off+1]
		wordLen := int(binary.BigEndian.Uint16(buf[off+2 : off+4]))
		pktLen := (wordLen + 1) * 4
		if pktLen > len(buf)-off {
			return nil, ErrRTCPLength
		}
		if pt == PTSenderReport && pktLen >= senderInfoSize {
			reports = append(reports, SenderReport{
				SSRC: binary.BigEndian.Uint32(buf[off+4 : off+8]),
				NTPTimestamp: uint64(binary.BigEndian.Uint32(buf[off+8:off+12]))<<32 |
					uint64(binary.BigEndian.Uint32(buf[off+12:off+16])),
				RTPTimestamp: binary.BigEndian.Uint32(buf[off+16 : off+20]),
				PacketCount:  binary.BigEndian.Uint32(buf[off+20 : off+24]),
				OctetCount:   binary.BigEndian.Uint32(buf[off+24 : off+28]),
			})
		}
		off += pktLen // always at least 4, so the loop always advances
	}
	return reports, nil
}

// SenderClockFrom builds the RTP-to-wall-clock correspondence a track publishes
// to TrackStats from a Sender Report, or returns nil when the report maps
// nothing. It is the single shared construction point for both the TCP
// (interleaved) and UDP RTCP paths, which build the value identically; the
// surrounding orchestration (different receivers, and the interleaved path's
// RR/re-lock bookkeeping) legitimately differs and stays at the call sites.
//
// A nil return encodes the RFC 3550 section 6.4.1 rule directly: a sender with
// no wall clock sends an all-zero NTP timestamp, which maps nothing, so the
// caller stores nil to clear any prior correspondence rather than keep
// extrapolating a stale pair. On a usable pair it decodes the NTP timestamp and
// returns a Valid SenderClock stamped with receivedAt (the local receive time)
// and clockRate (the track's RTP clock rate in ticks per second).
func SenderClockFrom(sr SenderReport, receivedAt time.Time, clockRate int) *audiostream.SenderClock {
	if sr.NTPTimestamp == 0 {
		return nil
	}
	return &audiostream.SenderClock{
		RTPTime:    sr.RTPTimestamp,
		NTPTime:    NTPTime(sr.NTPTimestamp),
		ReceivedAt: receivedAt,
		ClockRate:  clockRate,
		Valid:      true,
	}
}

// ReceiverReport is a reception report the client sends back to the
// server (RFC 3550 section 6.4.2).
type ReceiverReport struct {
	// ReporterSSRC is the SSRC of this client, the report sender.
	ReporterSSRC uint32
	// Blocks holds one report block per source being received.
	Blocks []ReportBlock
}

// ReportBlock is one reception report block.
type ReportBlock struct {
	// SSRC is the source this block reports on.
	SSRC uint32
	// FractionLost is the fraction of packets lost since the last report,
	// as an 8-bit fixed-point value.
	FractionLost uint8
	// CumulativeLost is the cumulative packets lost, carried in the low
	// 24 bits.
	CumulativeLost uint32
	// HighestSequence is the extended highest sequence number received.
	HighestSequence uint32
	// Jitter is the interarrival jitter estimate.
	Jitter uint32
	// LastSR is the middle 32 bits of the NTP timestamp from the most
	// recent Sender Report (LSR), 0 if none received.
	LastSR uint32
	// DelaySinceLastSR is the delay since the last SR in units of 1/65536
	// seconds (DLSR), 0 if no SR received.
	DelaySinceLastSR uint32
}

// Marshal encodes the Receiver Report into RTCP wire format (RFC 3550
// section 6.4.2). At most 31 blocks are emitted, the maximum the 5-bit RC
// field can express; any blocks beyond 31 are dropped so the RC field,
// the length word, and the body stay mutually consistent. The length word
// is computed from the emitted block count. It never panics.
func (r ReceiverReport) Marshal() []byte {
	// Clamp the block count into the 5-bit RC field first, then drive the
	// RC field, the total size, the length word, and the block loop from
	// this same n so they never disagree with the bytes written.
	n := len(r.Blocks)
	if n > maxReportBlocks {
		n = maxReportBlocks
	}
	size := rrHeaderSize + n*reportBlockSize
	wordLen := size/4 - 1

	buf := make([]byte, 0, size)
	buf = append(buf, byte(2<<6|n), PTReceiverReport)
	buf = binary.BigEndian.AppendUint16(buf, uint16(wordLen))
	buf = binary.BigEndian.AppendUint32(buf, r.ReporterSSRC)

	for _, b := range r.Blocks[:n] {
		buf = binary.BigEndian.AppendUint32(buf, b.SSRC)
		buf = append(buf, b.FractionLost,
			byte(b.CumulativeLost>>16), byte(b.CumulativeLost>>8), byte(b.CumulativeLost))
		buf = binary.BigEndian.AppendUint32(buf, b.HighestSequence)
		buf = binary.BigEndian.AppendUint32(buf, b.Jitter)
		buf = binary.BigEndian.AppendUint32(buf, b.LastSR)
		buf = binary.BigEndian.AppendUint32(buf, b.DelaySinceLastSR)
	}
	return buf
}
