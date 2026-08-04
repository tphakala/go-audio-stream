package doctor

import (
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// TestComputeStatsJitter exercises the RFC 3550 section 6.4.1 worked vector:
// four packets, one frame each, clock rate 16000. The expected JitterTicks
// of 9.375 is exact because every intermediate term is dyadic in float64.
func TestComputeStatsJitter(t *testing.T) {
	t.Parallel()
	const clockRate = 16000
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	frames := []CapturedFrame{
		{RTPTime: 0, PTS: 0, ReceivedAt: base},
		{RTPTime: 1024, PTS: 64 * time.Millisecond, ReceivedAt: base.Add(64 * time.Millisecond)},
		{RTPTime: 2048, PTS: 128 * time.Millisecond, ReceivedAt: base.Add(138 * time.Millisecond)},
		{RTPTime: 3072, PTS: 192 * time.Millisecond, ReceivedAt: base.Add(202 * time.Millisecond)},
	}

	stats := computeStats(frames, &audiostream.TrackStats{}, clockRate, 0, time.Time{})
	if stats.JitterTicks != 9.375 {
		t.Fatalf("JitterTicks = %v, want 9.375", stats.JitterTicks)
	}
	wantMS := 9.375 / clockRate * 1000
	if diff := stats.JitterMS - wantMS; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("JitterMS = %v, want %v (within 1e-9)", stats.JitterMS, wantMS)
	}
}

func TestComputeStatsJitterSinglePacket(t *testing.T) {
	t.Parallel()
	frames := []CapturedFrame{{RTPTime: 0, PTS: 0, ReceivedAt: time.Now()}}
	stats := computeStats(frames, &audiostream.TrackStats{}, 16000, time.Second, time.Time{})
	if stats.JitterTicks != 0 {
		t.Errorf("JitterTicks = %v, want 0", stats.JitterTicks)
	}
}

// TestComputeStatsJitterMultiAUPacket proves that several access-unit
// frames sharing one RTP timestamp are grouped into a single packet, so
// they do not inflate jitter beyond the single-packet (zero) case.
func TestComputeStatsJitterMultiAUPacket(t *testing.T) {
	t.Parallel()
	base := time.Now()
	frames := []CapturedFrame{
		{RTPTime: 100, PTS: 0, ReceivedAt: base},
		{RTPTime: 100, PTS: 10 * time.Millisecond, ReceivedAt: base.Add(1 * time.Millisecond)},
		{RTPTime: 100, PTS: 20 * time.Millisecond, ReceivedAt: base.Add(2 * time.Millisecond)},
	}
	stats := computeStats(frames, &audiostream.TrackStats{}, 16000, time.Second, time.Time{})
	if stats.JitterTicks != 0 {
		t.Errorf("JitterTicks = %v, want 0 (frames sharing one RTPTime must count as a single packet)", stats.JitterTicks)
	}
}

// TestComputeStatsDuplicates checks that the library's Duplicates counter is
// surfaced into CaptureStats unchanged.
func TestComputeStatsDuplicates(t *testing.T) {
	t.Parallel()
	lib := audiostream.TrackStats{Packets: 500, PayloadBytes: 64000, SeqGaps: 3, Duplicates: 2, Malformed: 1}
	stats := computeStats(nil, &lib, 48000, time.Second, time.Time{})
	if stats.Duplicates != 2 {
		t.Errorf("Duplicates = %d, want 2", stats.Duplicates)
	}
}

func TestComputeStatsLossAndGap(t *testing.T) {
	t.Parallel()
	lib := audiostream.TrackStats{Packets: 490, PayloadBytes: 62720, SeqGaps: 10, Malformed: 4, SSRCResets: 2}
	frames := []CapturedFrame{
		{RTPTime: 0, SeqGap: 0},
		{RTPTime: 1, SeqGap: 3},
		{RTPTime: 2, SeqGap: 1},
	}
	stats := computeStats(frames, &lib, 16000, time.Second, time.Time{})
	if stats.Lost != 10 {
		t.Errorf("Lost = %d, want 10", stats.Lost)
	}
	if stats.Malformed != 4 {
		t.Errorf("Malformed = %d, want 4", stats.Malformed)
	}
	if stats.SSRCResets != 2 {
		t.Errorf("SSRCResets = %d, want 2", stats.SSRCResets)
	}
	wantRatio := 10.0 / 500
	if stats.LossRatio != wantRatio {
		t.Errorf("LossRatio = %v, want %v", stats.LossRatio, wantRatio)
	}
	if stats.MaxGap != 3 {
		t.Errorf("MaxGap = %d, want 3", stats.MaxGap)
	}
}

func TestComputeStatsBitrate(t *testing.T) {
	t.Parallel()
	lib := audiostream.TrackStats{PayloadBytes: 64000}
	stats := computeStats(nil, &lib, 16000, 10*time.Second, time.Time{})
	if stats.Bitrate != 51200 {
		t.Errorf("Bitrate = %v, want 51200", stats.Bitrate)
	}
}

func TestComputeStatsZeroGuards(t *testing.T) {
	t.Parallel()
	stats := computeStats(nil, &audiostream.TrackStats{}, 16000, 0, time.Time{})
	if stats.Bitrate != 0 {
		t.Errorf("Bitrate = %v, want 0", stats.Bitrate)
	}
	if stats.LossRatio != 0 {
		t.Errorf("LossRatio = %v, want 0", stats.LossRatio)
	}
	if stats.JitterTicks != 0 {
		t.Errorf("JitterTicks = %v, want 0", stats.JitterTicks)
	}
}

// TestComputeStatsJitterEarlyArrival exercises the absolute-value step of the
// RFC 3550 estimator: the third packet arrives earlier than its timestamp
// spacing implies (a 32 ms receive gap against a 64 ms timestamp gap), so the
// transit difference D is negative. The jitter must use |D|, giving a positive
// 32 ticks; without the abs step it would be -32.
func TestComputeStatsJitterEarlyArrival(t *testing.T) {
	t.Parallel()
	const clockRate = 16000
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	frames := []CapturedFrame{
		{RTPTime: 0, PTS: 0, ReceivedAt: base},
		{RTPTime: 1024, PTS: 64 * time.Millisecond, ReceivedAt: base.Add(64 * time.Millisecond)},
		{RTPTime: 2048, PTS: 128 * time.Millisecond, ReceivedAt: base.Add(96 * time.Millisecond)},
	}

	stats := computeStats(frames, &audiostream.TrackStats{}, clockRate, 0, time.Time{})
	if diff := stats.JitterTicks - 32; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("JitterTicks = %v, want 32 within 1e-9 (abs step likely missing)", stats.JitterTicks)
	}
}

// TestComputeStatsWireBitrate checks the on-the-wire bitrate math and its
// zero-elapsed guard: 70000 bytes over 10 s is 56000 bit/s, the same shape as
// the payload bitrate.
func TestComputeStatsWireBitrate(t *testing.T) {
	t.Parallel()
	lib := audiostream.TrackStats{PayloadBytes: 64000, WireBytes: 70000}
	stats := computeStats(nil, &lib, 16000, 10*time.Second, time.Time{})
	if stats.WireBytes != 70000 {
		t.Errorf("WireBytes = %d, want 70000", stats.WireBytes)
	}
	if stats.WireBitrate != 56000 {
		t.Errorf("WireBitrate = %v, want 56000", stats.WireBitrate)
	}
	zero := computeStats(nil, &lib, 16000, 0, time.Time{})
	if zero.WireBitrate != 0 {
		t.Errorf("WireBitrate = %v, want 0 for zero elapsed", zero.WireBitrate)
	}
}

// TestComputeStatsLastFrameAge covers the last-frame age: a normal positive
// age, the no-frame case (HaveLastFrame false), and the clamp when a backward
// wall-clock step would make CapturedAt precede LastFrameAt.
func TestComputeStatsLastFrameAge(t *testing.T) {
	t.Parallel()
	last := time.Unix(1000, 0)
	captured := last.Add(400 * time.Millisecond)
	lib := audiostream.TrackStats{LastFrameAt: last}

	stats := computeStats(nil, &lib, 16000, time.Second, captured)
	if !stats.HaveLastFrame {
		t.Fatal("HaveLastFrame = false, want true")
	}
	if stats.LastFrameAge != 400*time.Millisecond {
		t.Errorf("LastFrameAge = %v, want 400ms", stats.LastFrameAge)
	}

	none := computeStats(nil, &audiostream.TrackStats{}, 16000, time.Second, captured)
	if none.HaveLastFrame {
		t.Error("HaveLastFrame = true, want false when LastFrameAt is zero")
	}
	if none.LastFrameAge != 0 {
		t.Errorf("LastFrameAge = %v, want 0", none.LastFrameAge)
	}

	stepped := computeStats(nil, &lib, 16000, time.Second, last.Add(-time.Second))
	if !stepped.HaveLastFrame {
		t.Error("HaveLastFrame = false, want true")
	}
	if stepped.LastFrameAge != 0 {
		t.Errorf("LastFrameAge = %v, want 0 (clamped)", stepped.LastFrameAge)
	}
}

// TestComputeStatsSenderClock covers the sender-clock derivation: a valid
// report extrapolates the last frame's RTP timestamp to the sender wall clock
// and records its offset from the local receive time; an invalid report leaves
// the derived fields zero.
func TestComputeStatsSenderClock(t *testing.T) {
	t.Parallel()
	const clockRate = 16000
	anchor := time.Date(2026, 8, 4, 9, 12, 33, 0, time.UTC)
	// The last captured frame is one second (clockRate ticks) past the report
	// anchor, so its extrapolated wall clock is anchor+1s. Its local receive
	// time is 880 ms past the anchor, leaving a 120 ms offset.
	recv := anchor.Add(880 * time.Millisecond)
	frames := []CapturedFrame{
		{RTPTime: 0, ReceivedAt: anchor},
		{RTPTime: clockRate, ReceivedAt: recv},
	}
	lib := audiostream.TrackStats{SenderClock: audiostream.SenderClock{RTPTime: 0, NTPTime: anchor, ClockRate: clockRate, Valid: true}}

	stats := computeStats(frames, &lib, clockRate, time.Second, time.Time{})
	wantWall := anchor.Add(time.Second)
	if !stats.SenderWall.Equal(wantWall) {
		t.Errorf("SenderWall = %v, want %v", stats.SenderWall, wantWall)
	}
	if stats.SenderOffset != 120*time.Millisecond {
		t.Errorf("SenderOffset = %v, want 120ms", stats.SenderOffset)
	}
	if !stats.SenderClock.Valid {
		t.Error("SenderClock.Valid = false, want the raw clock copied through")
	}

	inv := computeStats(frames, &audiostream.TrackStats{}, clockRate, time.Second, time.Time{})
	if !inv.SenderWall.IsZero() {
		t.Errorf("SenderWall = %v, want zero for an invalid clock", inv.SenderWall)
	}
	if inv.SenderOffset != 0 {
		t.Errorf("SenderOffset = %v, want 0 for an invalid clock", inv.SenderOffset)
	}
	if inv.SenderClock.Valid {
		t.Error("SenderClock.Valid = true, want false")
	}

	// With a valid report but no captured frames, fall back to the report's
	// own pair: SenderWall is its NTP time and the offset is that time against
	// its local receive time, never a misleading zero.
	srRecv := anchor.Add(-40 * time.Millisecond)
	noFrames := computeStats(nil, &audiostream.TrackStats{SenderClock: audiostream.SenderClock{RTPTime: 0, NTPTime: anchor, ReceivedAt: srRecv, ClockRate: clockRate, Valid: true}}, clockRate, time.Second, time.Time{})
	if !noFrames.SenderWall.Equal(anchor) {
		t.Errorf("SenderWall = %v, want the report NTP time %v with no frames", noFrames.SenderWall, anchor)
	}
	if noFrames.SenderOffset != 40*time.Millisecond {
		t.Errorf("SenderOffset = %v, want 40ms (NTP time minus report receive time)", noFrames.SenderOffset)
	}
}
