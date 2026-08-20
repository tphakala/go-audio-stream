package rtp

import (
	"testing"
	"time"
)

// NTPTime decodes a 64-bit RTCP NTP timestamp into a time.Time. The seconds
// field is read through the RFC 5905 era-1 pivot so a sender clock in the
// post-2036 era decodes correctly, and the fraction is the binary fraction of
// a second.
func TestNTPTime(t *testing.T) {
	// The NTP-epoch seconds value that lands exactly on the Unix epoch; its
	// high bit is set, so it decodes in era 0 with no pivot.
	const unixEpochNTPSeconds = uint64(ntpUnixOffset)

	cases := []struct {
		name string
		ts   uint64
		want time.Time
	}{
		{
			name: "era-0 Unix epoch, zero fraction",
			ts:   unixEpochNTPSeconds << 32,
			want: time.Unix(0, 0),
		},
		{
			// The high fraction bit is exactly one half second.
			name: "half-second fraction",
			ts:   unixEpochNTPSeconds<<32 | 0x80000000,
			want: time.Unix(0, int64(time.Second)/2),
		},
		{
			// Seconds field 0x1 has its high bit clear, so the pivot reads it as
			// era 1: 2^32 + 1 seconds after 1900.
			name: "era-1 pivot",
			ts:   uint64(0x1) << 32,
			want: time.Unix((int64(1)<<32)+1-ntpUnixOffset, 0),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NTPTime(tc.ts)
			if !got.Equal(tc.want) {
				t.Errorf("NTPTime(%#016x) = %v, want %v", tc.ts, got.UTC(), tc.want.UTC())
			}
		})
	}
}
