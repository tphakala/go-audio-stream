package audiostream

import "time"

// TrackStats are cumulative per-track receive statistics.
type TrackStats struct {
	// Packets is the number of frames counted on the track's RTP channel. For
	// an active (parsed) track it is the number of RTP packets accepted and
	// delivered. For a discard track it is the number of frames seen on the RTP
	// channel; it no longer includes RTCP compounds, which are never media.
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
	// summed over every frame routed to the track whether it parsed or not. It
	// is the network-bandwidth figure. It strictly measures the RTP channel and
	// excludes RTCP overhead and RTSP control messages. WireBytes grows on a
	// malformed or rejected frame while PayloadBytes and Packets do not, so the
	// Malformed count explains any delta between them.
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
