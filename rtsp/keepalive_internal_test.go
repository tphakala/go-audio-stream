package rtsp

import (
	"math"
	"testing"
	"time"
)

// dlsrUnits is the DLSR conversion the Receiver Report builder uses. The field
// is 32 bits in units of 1/65536 s, so it saturates rather than wrapping: a
// single Sender Report followed by a long silence used to read back as a much
// shorter delay.
func TestDLSRUnits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		elapsed time.Duration
		want    uint32
	}{
		{name: "zero", elapsed: 0, want: 0},
		{name: "negative clock step", elapsed: -time.Second, want: 0},
		{name: "one second", elapsed: time.Second, want: 1 << 16},
		{name: "half second", elapsed: 500 * time.Millisecond, want: 1 << 15},
		{name: "ten seconds", elapsed: 10 * time.Second, want: 10 << 16},
		{name: "just under the field width", elapsed: (maxDLSRSeconds - 1) * time.Second, want: (maxDLSRSeconds - 1) << 16},
		{name: "at the field width", elapsed: maxDLSRSeconds * time.Second, want: math.MaxUint32},
		{name: "a day", elapsed: 24 * time.Hour, want: math.MaxUint32},
		{name: "past the shift overflow", elapsed: 100 * time.Hour, want: math.MaxUint32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := dlsrUnits(tc.elapsed); got != tc.want {
				t.Errorf("dlsrUnits(%v) = %d, want %d", tc.elapsed, got, tc.want)
			}
		})
	}
	// Monotonic within the representable range: a longer wait must never read
	// back as a shorter delay, which is exactly what the old shift did.
	prev := uint32(0)
	for _, d := range []time.Duration{time.Second, time.Minute, time.Hour, 5 * time.Hour, 18 * time.Hour} {
		got := dlsrUnits(d)
		if got < prev {
			t.Errorf("dlsrUnits(%v) = %d, lower than the previous %d", d, got, prev)
		}
		prev = got
	}
}
