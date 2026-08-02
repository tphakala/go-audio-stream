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

	stats := computeStats(frames, audiostream.TrackStats{}, clockRate, 0)
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
	stats := computeStats(frames, audiostream.TrackStats{}, 16000, time.Second)
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
	stats := computeStats(frames, audiostream.TrackStats{}, 16000, time.Second)
	if stats.JitterTicks != 0 {
		t.Errorf("JitterTicks = %v, want 0 (frames sharing one RTPTime must count as a single packet)", stats.JitterTicks)
	}
}

// TestComputeStatsDuplicates checks that the library's Duplicates counter is
// surfaced into CaptureStats unchanged.
func TestComputeStatsDuplicates(t *testing.T) {
	t.Parallel()
	lib := audiostream.TrackStats{Packets: 500, Bytes: 64000, SeqGaps: 3, Duplicates: 2, Malformed: 1}
	stats := computeStats(nil, lib, 48000, time.Second)
	if stats.Duplicates != 2 {
		t.Errorf("Duplicates = %d, want 2", stats.Duplicates)
	}
}

func TestComputeStatsLossAndGap(t *testing.T) {
	t.Parallel()
	lib := audiostream.TrackStats{Packets: 490, Bytes: 62720, SeqGaps: 10, Malformed: 4, SSRCResets: 2}
	frames := []CapturedFrame{
		{RTPTime: 0, SeqGap: 0},
		{RTPTime: 1, SeqGap: 3},
		{RTPTime: 2, SeqGap: 1},
	}
	stats := computeStats(frames, lib, 16000, time.Second)
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
	lib := audiostream.TrackStats{Bytes: 64000}
	stats := computeStats(nil, lib, 16000, 10*time.Second)
	if stats.Bitrate != 51200 {
		t.Errorf("Bitrate = %v, want 51200", stats.Bitrate)
	}
}

func TestComputeStatsZeroGuards(t *testing.T) {
	t.Parallel()
	stats := computeStats(nil, audiostream.TrackStats{}, 16000, 0)
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

	stats := computeStats(frames, audiostream.TrackStats{}, clockRate, 0)
	if diff := stats.JitterTicks - 32; diff < -1e-9 || diff > 1e-9 {
		t.Errorf("JitterTicks = %v, want 32 within 1e-9 (abs step likely missing)", stats.JitterTicks)
	}
}
