package audiostream_test

import (
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

// WallClock maps an RTP timestamp back to the sender's wall clock from the
// Sender Report pair. The 32-bit difference is interpreted signed, so a
// timestamp on the far side of the 32-bit wrap from the report still resolves
// to a nearby instant rather than to a value half the clock's range away.
func TestSenderClockWallClock(t *testing.T) {
	base := time.Unix(1_600_000_000, 0).UTC()
	const rate = 48000

	// dur is WallClock's own tick-to-duration arithmetic, so the wrap cases
	// assert the signed interpretation of the difference rather than a
	// hand-rounded nanosecond count.
	dur := func(ticks int64) time.Duration {
		return time.Duration(ticks * int64(time.Second) / int64(rate))
	}

	cases := []struct {
		name     string
		sc       audiostream.SenderClock
		rtpTime  uint32
		wantZero bool
		want     time.Time
	}{
		{
			name:    "same tick returns the report time",
			sc:      audiostream.SenderClock{RTPTime: 100000, NTPTime: base, ClockRate: rate, Valid: true},
			rtpTime: 100000,
			want:    base,
		},
		{
			name:    "one clock rate ahead is one second later",
			sc:      audiostream.SenderClock{RTPTime: 100000, NTPTime: base, ClockRate: rate, Valid: true},
			rtpTime: 100000 + rate,
			want:    base.Add(time.Second),
		},
		{
			name:    "one clock rate behind is one second earlier",
			sc:      audiostream.SenderClock{RTPTime: 100000, NTPTime: base, ClockRate: rate, Valid: true},
			rtpTime: 100000 - rate,
			want:    base.Add(-time.Second),
		},
		{
			// Report near the top of the 32-bit range, frame past the wrap:
			// 0x00002000 - 0xFFFFE000 is +16384 as a signed 32-bit difference.
			name:    "forward across the 32-bit wrap",
			sc:      audiostream.SenderClock{RTPTime: 0xFFFFE000, NTPTime: base, ClockRate: rate, Valid: true},
			rtpTime: 0x00002000,
			want:    base.Add(dur(16384)),
		},
		{
			// Mirror: report just past zero, frame near the top wraps back.
			name:    "backward across the 32-bit wrap",
			sc:      audiostream.SenderClock{RTPTime: 0x00002000, NTPTime: base, ClockRate: rate, Valid: true},
			rtpTime: 0xFFFFE000,
			want:    base.Add(dur(-16384)),
		},
		{
			name:     "invalid mapping returns zero time",
			sc:       audiostream.SenderClock{RTPTime: 1000, NTPTime: base, ClockRate: rate, Valid: false},
			rtpTime:  1000,
			wantZero: true,
		},
		{
			name:     "zero clock rate returns zero time",
			sc:       audiostream.SenderClock{RTPTime: 1000, NTPTime: base, ClockRate: 0, Valid: true},
			rtpTime:  1000,
			wantZero: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.sc.WallClock(tc.rtpTime)
			if tc.wantZero {
				if !got.IsZero() {
					t.Errorf("WallClock(%d) = %v, want the zero time", tc.rtpTime, got)
				}
				return
			}
			if !got.Equal(tc.want) {
				t.Errorf("WallClock(%d) = %v, want %v", tc.rtpTime, got, tc.want)
			}
		})
	}
}
