// Package mediatime holds the overflow-safe presentation-time math for the audio
// sources. Both an RTP timestamp (a sample count at the media clock rate) and a
// running PCM sample count map to a presentation time the same way, through one
// split-seconds helper. udpsource computes its PTS through it; the older rtsp
// and httpsource clients still carry their own equivalent copies of this
// arithmetic and can be migrated onto this helper incrementally.
package mediatime

import (
	"math"
	"time"
)

// maxPTSSeconds is the largest whole-second count a time.Duration holds. A
// duration is an int64 nanosecond count, so the seconds term of a PTS must stay
// below this or the multiply that scales it wraps negative.
const maxPTSSeconds = math.MaxInt64 / int64(time.Second)

// PTSFromSamples returns the presentation time of sample index samples played
// at clockRate Hz. The division is split into whole seconds and a remainder so
// the nanosecond scaling cannot overflow on a long stream, and the seconds term
// is clamped to what a time.Duration can express. It returns 0 for a
// non-positive clockRate, so a caller that has not resolved a rate yields PTS 0
// rather than dividing by zero.
func PTSFromSamples(samples uint64, clockRate int) time.Duration {
	if clockRate <= 0 {
		return 0
	}
	rate := uint64(clockRate)
	sec := samples / rate
	if sec >= uint64(maxPTSSeconds) {
		return time.Duration(maxPTSSeconds) * time.Second
	}
	frac := (samples % rate) * uint64(time.Second) / rate
	return time.Duration(sec)*time.Second + time.Duration(frac)
}
