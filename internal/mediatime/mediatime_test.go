package mediatime

import (
	"testing"
	"time"
)

func TestPTSFromSamples(t *testing.T) {
	cases := []struct {
		name      string
		samples   uint64
		clockRate int
		want      time.Duration
	}{
		{"zero clock rate guards div-by-zero", 1000, 0, 0},
		{"negative clock rate guards div-by-zero", 1000, -1, 0},
		{"zero samples is PTS 0", 0, 48000, 0},
		{"one full second", 48000, 48000, time.Second},
		{"one opus frame 960@48000", 960, 48000, 20 * time.Millisecond},
		{"fractional 1152@44100", 1152, 44100, time.Duration(1152) * time.Second / 44100},
		{"g711 8000@8000", 8000, 8000, time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PTSFromSamples(tc.samples, tc.clockRate); got != tc.want {
				t.Errorf("PTSFromSamples(%d, %d) = %v, want %v", tc.samples, tc.clockRate, got, tc.want)
			}
		})
	}
}

// TestPTSFromSamplesOverflowClamp drives the seconds term past what a
// time.Duration can hold and asserts the result clamps to the maximum rather
// than wrapping negative.
func TestPTSFromSamplesOverflowClamp(t *testing.T) {
	got := PTSFromSamples(^uint64(0), 48000)
	if got < 0 {
		t.Fatalf("PTSFromSamples clamped to a negative duration: %v", got)
	}
	if want := time.Duration(maxPTSSeconds) * time.Second; got != want {
		t.Errorf("clamp = %v, want %v", got, want)
	}
}
