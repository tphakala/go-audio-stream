// Package rtsp implements an RTSP 1.0 client for audio ingestion from
// IP cameras and restreamers.
//
// The package is being assembled in pieces. Two layers are present.
//
// The wire layer is pure: serializing requests, parsing responses and
// server-initiated requests, framing interleaved binary data, resolving
// control URLs, answering authentication challenges, and classifying response
// status codes into typed errors. It holds no state beyond what the caller
// passes in. Its input arrives from the network and is treated as hostile, so
// sizes are capped and every parser is total.
//
// The session client is built on that layer. Dial opens a TCP (or, for rtsps,
// TLS) connection, starts a reader goroutine that owns every socket read for
// the connection's life, and probes OPTIONS to learn the keepalive method. A
// Client therefore holds a socket and a goroutine, and must be released with
// Close; Wait reports the terminal cause. Close, Wait, Stats and SessionInfo
// are safe from any goroutine. Describe and Setup discover the tracks and
// negotiate their interleaved channels. Routing begins with the first
// successful Setup, not with Play: from then on the reader routes each
// interleaved frame to its track, depacketizes it, and calls Config.OnFrame on
// the reader goroutine. Play is what asks the server to start sending, and it
// also starts a timer goroutine that emits RTSP keepalives and RTCP Receiver
// Reports. A 401 on Describe, Setup or Play is answered and retried
// automatically; Dial's OPTIONS probe tolerates one instead, since it only reads
// the Public header.
//
// This client speaks only the TCP-interleaved profile; there is no UDP
// transport. Several details within that profile are deliberately coarse for
// now, and each says so where it is implemented: Receiver Reports carry no
// jitter or fraction-lost estimate, only AAC-hbr among RFC 3640's modes is
// depacketized, and AAC PTS interpolation assumes 1024 samples per frame.
//
// Errors are matched two ways. Test a category with errors.Is against a
// sentinel: the wire-layer and lifecycle sentinels (ErrMalformedTransport,
// ErrNotSDP, ErrAuthFailed and the rest) match directly, while the three typed
// errors this package defines match one through an Is method, ResponseError
// against ErrResponseStatus, UnauthorizedError against ErrUnauthorized, and
// StateError against ErrInvalidState. Recover a typed error's fields with
// errors.As: *ResponseError for the status Code and Reason, *UnauthorizedError
// for the raw challenges, *StateError for the rejected Method and State, and
// the root package's *audiostream.RedirectError for a 3xx Location, which has
// no sentinel and is matched with errors.As only.
package rtsp
