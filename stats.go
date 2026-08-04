package audiostream

import "time"

// TrackStats are cumulative per-track receive statistics.
type TrackStats struct {
	// Packets is the number of accepted frames on the track's RTP channel. On
	// an active (parsed) track it counts RTP packets that parsed and were
	// delivered. On a discard track, which is never parsed, it counts frames
	// that passed the RTP shape check (a full header and version 2); a
	// shape-invalid frame is wire traffic but not an accepted packet, so a peer
	// cannot inflate the count with garbage. It never includes RTCP compounds,
	// which are not media.
	Packets uint64
	// PayloadBytes is the total RTP payload bytes accepted with the RTP header
	// stripped: the compressed-audio figure. It includes any per-codec
	// packetization overhead (for AAC, the RFC 3640 AU headers). It stays zero
	// for a discard track, whose frames are never parsed, so the RTP header
	// length is unknown and no payload boundary can be established. The
	// byte-exact codec bitrate is the sum of the delivered Frame.Data lengths,
	// which a consumer already receives through OnFrame.
	PayloadBytes uint64
	// WireBytes is the total bytes on the track's RTP channel as framed on the
	// wire: the 4-byte interleaved header plus the RTP header plus the payload,
	// summed over every frame routed to the track whether it was accepted or
	// not. It is the network-bandwidth figure, and strictly measures the RTP
	// channel: it excludes RTCP overhead and RTSP control messages. The
	// WireBytes-minus-Packets gap is the rejected traffic on the channel. On an
	// active track those are the frames the Malformed count explains (an
	// unparseable header, or a payload type the track does not carry). On a
	// discard track, which never parses and so never reports Malformed, they are
	// the shape-invalid frames.
	WireBytes uint64
	// SeqGaps is the total number of packets lost per sequence
	// number tracking.
	SeqGaps uint64
	// Duplicates is the number of duplicate or reordered packets the stream
	// observed: a sequence number that did not advance. Rare over the in-order
	// TCP transport, so a nonzero value points at the server resending or
	// reordering rather than ordinary loss.
	Duplicates uint64
	// Malformed is the number of packets discarded without being delivered,
	// for any reason: an RTP header that will not parse, a payload type the
	// track does not carry (a second format multiplexed onto the same
	// channel), or a codec depacketizer rejecting the payload. A steadily
	// climbing count is not by itself evidence of a corrupt stream, because a
	// multiplexed second format increments it on a healthy session.
	Malformed uint64
	// SSRCResets is the number of mid-stream SSRC changes tolerated.
	SSRCResets uint64
	// LastFrameAt is the wall-clock arrival time of the most recent frame on
	// the track's RTP channel, parsed or not; it is the zero Time until the
	// first such frame and is never advanced by RTCP traffic, so it is a media
	// liveness clock. Subtract it from Stats.CapturedAt for the time since the
	// last frame:
	//
	//	age := stats.CapturedAt.Sub(track.LastFrameAt)
	//	receiving := !track.LastFrameAt.IsZero() && age < consumerThreshold
	//
	// The staleness threshold is consumer policy, so no receiving-data boolean
	// is exposed. The difference is wall-clock rather than monotonic: an atomic
	// UnixNano cannot carry Go's monotonic reading, so an NTP step can skew it,
	// which is acceptable for a diagnostic.
	LastFrameAt time.Time
	// SenderClock is the RTP-to-wall-clock correspondence from the track's
	// most recent RTCP Sender Report, invalid until one arrives. Only an
	// RTCP-bearing source (RTSP) ever populates it; other sources leave it
	// invalid.
	SenderClock SenderClock
}

// SenderClock is the RTP-to-wall-clock correspondence taken from a track's
// most recent RTCP Sender Report: the sender sampled its media clock
// (RTPTime) and its wall clock (NTPTime) at the same instant, and WallClock
// extrapolates any frame's RTP timestamp to the sender's wall clock from
// that pair.
//
// The mapping is best effort and may be absent for the whole session. Valid
// is false until a Sender Report carrying a usable pair arrives; many
// cameras send Sender Reports irregularly or not at all, and a sender with
// no wall clock may send an all-zero NTP timestamp (RFC 3550 section 6.4.1),
// which never yields a valid mapping. An SSRC reset clears the mapping until
// the new source's first Sender Report. A discard track is never parsed, so
// its mapping stays invalid. Absence never affects media delivery.
//
// NTPTime is only as trustworthy as the sender's own clock: a camera with an
// unsynchronized clock reports a correspondence to a wrong wall clock, and
// the pair ages as the sender's oscillator drifts, so a consumer that cares
// should check the report's age (Stats.CapturedAt minus ReceivedAt) before
// relying on it.
type SenderClock struct {
	// RTPTime is the Sender Report's RTP timestamp: the sender's media clock
	// sampled at NTPTime, on the same 32-bit clock as Frame.RTPTime.
	RTPTime uint32
	// NTPTime is the report's 64-bit NTP timestamp decoded to a time.Time
	// (era-1 pivot, correct for sender clocks set between 1968 and 2104).
	NTPTime time.Time
	// ReceivedAt is the local wall-clock time the Sender Report was received.
	ReceivedAt time.Time
	// ClockRate is the track's RTP clock rate (ticks per second) recorded with
	// the pair; 0 when the SDP declared no usable rate.
	ClockRate int
	// Valid is true once a Sender Report supplied the pair since the track's
	// last SSRC change; the zero value is invalid.
	Valid bool
}

// WallClock extrapolates the sender's wall-clock time of an RTP timestamp
// (typically Frame.RTPTime) from the Sender Report pair. The 32-bit
// difference is interpreted signed, so timestamps up to 2^31 ticks either
// side of the report convert correctly across the 32-bit RTP wrap; at audio
// clock rates that is hours, far more than the seconds between Sender
// Reports. It returns the zero time.Time when the mapping is not Valid or
// ClockRate is 0.
func (sc SenderClock) WallClock(rtpTime uint32) time.Time {
	if !sc.Valid || sc.ClockRate <= 0 {
		return time.Time{}
	}
	delta := int64(int32(rtpTime - sc.RTPTime))
	return sc.NTPTime.Add(time.Duration(delta * int64(time.Second) / int64(sc.ClockRate)))
}

// Stats is a point-in-time snapshot of session statistics, keyed by
// track ID.
type Stats struct {
	// CapturedAt is when the snapshot read completed. It carries a monotonic
	// reading, so subtracting two snapshots' CapturedAt gives monotonic elapsed
	// time suitable for a rate computation. It is a diagnostic marker, not a
	// transactional boundary: the counters are still read one at a time, not
	// under a single barrier.
	CapturedAt time.Time
	Tracks     map[int]TrackStats
}
