// Package supervisor turns a single-session audiostream.Source into a
// transparently reconnecting one. It is an optional, opt-in wrapper: the core
// transport contract is unchanged, and a consumer that wants a session to end
// when its source ends simply does not use this package.
//
// # What it does
//
// A Supervisor drives exactly one supervising goroutine that connects, delivers,
// and reconnects. On construction it calls a user-supplied Factory to build the
// first fully negotiated, already delivering source; when that source's Wait
// returns a retryable cause it waits out a capped exponential backoff (with
// jitter) and calls the Factory again, and so on until the caller Closes it or a
// non-retryable cause ends it. Because the Factory closes over the consumer's
// OnFrame and OnCodecUpdate callbacks, frame delivery resumes on every reconnect
// with no rewiring, and the Supervisor itself satisfies audiostream.Source, so
// an existing consumer swaps a concrete client for a Supervisor and keeps its
// Wait, Close, Stats, and Info call sites.
//
// # Import boundary
//
// This package imports only the root audiostream package. It knows nothing of
// rtsp, httpsource, udpsource, or hlssource: the concrete-source knowledge lives
// entirely in the caller's Factory closure. That keeps the wrapper free of any
// protocol dependency and free of an import cycle.
//
// # Statistics reset on reconnect
//
// Stats and Info are current-session, not cumulative. Each reconnect builds a
// fresh source with its own zeroed counters, so Stats reflects only the live
// session while one is connected, and the final counters of the most recent
// session during a reconnect gap (and the zero Stats before the first connect).
// A consumer that wants totals across reconnects must accumulate them itself,
// for example by summing each session's final counters observed at a
// StateReconnecting transition.
//
// # Discontinuities across sessions
//
// A reconnect is a new transport session, so cross-session continuity is not
// preserved and a consumer must not assume it:
//
//   - OnCodecUpdate may fire again on a new session. An in-band configuration
//     (for example an MP4A-LATM AudioSpecificConfig learned from the first
//     packet) is relearned per session, so a consumer that caches codec
//     configuration should refresh it when a new session begins.
//   - RTP timestamps and sequence numbers restart. Each session has its own
//     SSRC, timestamp base, and sequence origin, so Frame.RTPTime and the
//     per-track sequence are discontinuous across a reconnect. Frame.PTS from a
//     source that resets its presentation clock likewise restarts. Treat a
//     reconnect (observable via OnState) as a hard boundary for any timestamp or
//     sequence tracking.
//
// # Ownership
//
// The caller owns the Supervisor's lifetime and MUST Close it to release the
// supervising goroutine and any live source; a Supervisor that is dropped
// without Close leaks its goroutine. Close is idempotent and safe from any
// goroutine, including from inside OnFrame. Wait blocks until the supervisor has
// fully stopped and returns the terminal cause.
package supervisor
