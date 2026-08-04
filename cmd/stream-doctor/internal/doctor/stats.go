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
	// Bytes is the number of RTP payload bytes accepted (from the library
	// Stats).
	Bytes uint64
	// WireBytes is the total bytes on the track's RTP channel as framed on
	// the wire (from the library Stats.WireBytes): the network-bandwidth
	// figure, counting rejected traffic too. It stays zero for a source that
	// does not meter transport framing, so the wire lines self-gate on it.
	WireBytes uint64
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
	// WireBitrate is WireBytes*8 / Elapsed seconds, in bit/s: the
	// on-the-wire counterpart to Bitrate, which counts payload only. It
	// shares Bitrate's zero-elapsed guard.
	WireBitrate float64
	// JitterTicks is the RFC 3550 interarrival jitter in RTP timestamp
	// ticks.
	JitterTicks float64
	// JitterMS is JitterTicks / clockRate * 1000.
	JitterMS float64
	// HaveLastFrame reports whether at least one frame has arrived on the
	// track, so LastFrameAge is meaningful; the library leaves LastFrameAt
	// the zero Time until the first frame.
	HaveLastFrame bool
	// LastFrameAge is the time from the most recent frame's arrival to the
	// stats snapshot (Stats.CapturedAt minus TrackStats.LastFrameAt),
	// clamped at zero. The subtraction is wall-clock, so a backward NTP step
	// is clamped rather than surfaced as a negative age.
	LastFrameAge time.Duration
	// SenderClock is the raw RTP-to-wall-clock correspondence from the
	// track's most recent RTCP Sender Report (from the library
	// Stats.SenderClock), invalid until one arrives. It is retained raw for
	// the listen path; only an RTCP-bearing source ever populates it.
	SenderClock audiostream.SenderClock
	// SenderWall is the sender's wall-clock time of the last captured frame,
	// extrapolated from SenderClock; the zero Time when the clock is
	// invalid. When the clock is valid but no frames were captured it holds
	// the Sender Report's own NTP time.
	SenderWall time.Time
	// SenderOffset is SenderWall minus the last frame's local receive time:
	// the gap between the sender's clock and this host's clock. Zero when no
	// frames were captured.
	SenderOffset time.Duration
}

// computeStats derives CaptureStats from the captured frames, the library's
// TrackStats counters, the track clock rate, the elapsed capture time, and
// the snapshot time capturedAt (used for the last-frame age).
func computeStats(frames []CapturedFrame, lib *audiostream.TrackStats, clockRate int, elapsed time.Duration, capturedAt time.Time) CaptureStats {
	stats := CaptureStats{
		Packets:     lib.Packets,
		Bytes:       lib.PayloadBytes,
		WireBytes:   lib.WireBytes,
		Lost:        lib.SeqGaps,
		Duplicates:  lib.Duplicates,
		Malformed:   lib.Malformed,
		SSRCResets:  lib.SSRCResets,
		SenderClock: lib.SenderClock,
	}

	if denom := stats.Packets + stats.Lost; denom > 0 {
		stats.LossRatio = float64(stats.Lost) / float64(denom)
	}
	if elapsed > 0 {
		stats.Bitrate = float64(stats.Bytes) * 8 / elapsed.Seconds()
		stats.WireBitrate = float64(stats.WireBytes) * 8 / elapsed.Seconds()
	}
	for _, f := range frames {
		if f.SeqGap > stats.MaxGap {
			stats.MaxGap = f.SeqGap
		}
	}

	// Last-frame age: CapturedAt minus LastFrameAt, only once a frame has
	// arrived. The library documents the subtraction as wall-clock, so a
	// backward NTP step could make it negative; clamp at zero.
	if !lib.LastFrameAt.IsZero() {
		stats.HaveLastFrame = true
		if age := capturedAt.Sub(lib.LastFrameAt); age > 0 {
			stats.LastFrameAge = age
		}
	}

	// Sender clock: extrapolate the last captured frame's RTP timestamp to
	// the sender's wall clock and record its offset from the local receive
	// time. With no frames but a valid report, fall back to the report's own
	// NTP time.
	if lib.SenderClock.Valid {
		switch {
		case len(frames) > 0:
			last := frames[len(frames)-1]
			stats.SenderWall = lib.SenderClock.WallClock(last.RTPTime)
			stats.SenderOffset = stats.SenderWall.Sub(last.ReceivedAt)
		default:
			// No media frame to anchor against: fall back to the sender
			// report's own pair, so the offset reflects the report's wall
			// clock against the time it was received rather than a misleading
			// zero (which would read as perfectly synchronized clocks).
			stats.SenderWall = lib.SenderClock.NTPTime
			stats.SenderOffset = lib.SenderClock.NTPTime.Sub(lib.SenderClock.ReceivedAt)
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
