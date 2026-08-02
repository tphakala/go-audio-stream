package doctor

import (
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// CaptureStats are the computed packet statistics for a capture.
type CaptureStats struct {
	// Packets is the number of RTP packets accepted (from the library
	// Stats).
	Packets uint64
	// Received is the number of RTP headers the stream observed (from the
	// library Stats.Received). For an active capture it equals Packets, since
	// the reader observes exactly the packets it accepts.
	Received uint64
	// Bytes is the number of RTP payload bytes accepted (from the library
	// Stats).
	Bytes uint64
	// Lost is the number of packets lost per sequence tracking (from the
	// library Stats.SeqGaps).
	Lost uint64
	// Duplicates is the number of duplicate or reordered packets the stream
	// observed (from the library Stats.Duplicates). Rare over TCP; a nonzero
	// value points at the server resending or reordering.
	Duplicates uint64
	// Malformed is the number of packets the library discarded without
	// delivering (from the library Stats.Malformed): an unparseable RTP
	// header, a payload type the track does not carry, or a depacketizer
	// rejecting the payload. A climbing count points at a codec or framing
	// mismatch worth reporting.
	Malformed uint64
	// SSRCResets is the number of mid-stream SSRC changes the library
	// tolerated (from the library Stats.SSRCResets).
	SSRCResets uint64
	// MaxGap is the largest single sequence-number gap observed across
	// frames.
	MaxGap int
	// LossRatio is Lost / (Packets + Lost); 0 when the denominator is 0.
	LossRatio float64
	// Bitrate is Bytes*8 / Elapsed seconds, in bit/s.
	Bitrate float64
	// JitterTicks is the RFC 3550 interarrival jitter in RTP timestamp
	// ticks.
	JitterTicks float64
	// JitterMS is JitterTicks / clockRate * 1000.
	JitterMS float64
}

// computeStats derives CaptureStats from the captured frames, the library's
// TrackStats counters, the track clock rate, and the elapsed capture time.
func computeStats(frames []CapturedFrame, lib audiostream.TrackStats, clockRate int, elapsed time.Duration) CaptureStats {
	stats := CaptureStats{
		Packets:    lib.Packets,
		Received:   lib.Received,
		Bytes:      lib.Bytes,
		Lost:       lib.SeqGaps,
		Duplicates: lib.Duplicates,
		Malformed:  lib.Malformed,
		SSRCResets: lib.SSRCResets,
	}

	if denom := stats.Packets + stats.Lost; denom > 0 {
		stats.LossRatio = float64(stats.Lost) / float64(denom)
	}
	if elapsed > 0 {
		stats.Bitrate = float64(stats.Bytes) * 8 / elapsed.Seconds()
	}
	for _, f := range frames {
		if f.SeqGap > stats.MaxGap {
			stats.MaxGap = f.SeqGap
		}
	}

	stats.JitterTicks = computeJitter(frames, clockRate)
	if clockRate > 0 {
		stats.JitterMS = stats.JitterTicks / float64(clockRate) * 1000
	}

	return stats
}

// computeJitter implements the RFC 3550 section 6.4.1 interarrival jitter
// estimator over RTP packets, not frames. A new packet begins at the first
// frame and whenever RTPTime differs from the previous frame's RTPTime, so
// several access-unit frames sharing one RTP timestamp count as a single
// packet and do not inflate jitter. For each consecutive packet pair,
// D = (R_i - R_{i-1}) - (S_i - S_{i-1}) and the smoothed jitter is updated
// with J += (abs(D) - J) / 16, J starting at 0. Fewer than two packets
// yields 0.
func computeJitter(frames []CapturedFrame, clockRate int) float64 {
	if len(frames) == 0 {
		return 0
	}

	// packets holds one representative frame per distinct RTP timestamp,
	// in arrival order.
	packets := make([]CapturedFrame, 0, len(frames))
	packets = append(packets, frames[0])
	for _, f := range frames[1:] {
		if f.RTPTime != packets[len(packets)-1].RTPTime {
			packets = append(packets, f)
		}
	}
	if len(packets) < 2 {
		return 0
	}

	base := packets[0].ReceivedAt
	rate := float64(clockRate)

	var jitter float64
	prevS := packets[0].PTS.Seconds() * rate
	prevR := packets[0].ReceivedAt.Sub(base).Seconds() * rate
	for _, p := range packets[1:] {
		s := p.PTS.Seconds() * rate
		r := p.ReceivedAt.Sub(base).Seconds() * rate
		d := (r - prevR) - (s - prevS)
		if d < 0 {
			d = -d
		}
		jitter += (d - jitter) / 16
		prevS, prevR = s, r
	}
	return jitter
}
