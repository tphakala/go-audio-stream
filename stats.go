package audiostream

// TrackStats are cumulative per-track receive statistics.
type TrackStats struct {
	// Packets is the number of RTP packets accepted.
	Packets uint64
	// Bytes is the total payload bytes accepted. For an active (parsed)
	// track this counts the RTP payload only, with the RTP header stripped.
	// For a discard track it counts the full interleaved payload, RTP
	// header included, because a discard track is validated but never
	// depacketized, so its header is not stripped.
	Bytes uint64
	// SeqGaps is the total number of packets lost per sequence
	// number tracking.
	SeqGaps uint64
	// Malformed is the number of packets discarded without being delivered:
	// unparseable ones, and ones carrying a payload type the track does not
	// carry (a second format multiplexed onto the same channel). A steadily
	// climbing count is not by itself evidence of a corrupt stream.
	Malformed uint64
	// SSRCResets is the number of mid-stream SSRC changes tolerated.
	SSRCResets uint64
}

// Stats is a point-in-time snapshot of session statistics, keyed by
// track ID.
type Stats struct {
	Tracks map[int]TrackStats
}
