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
// negotiate their interleaved channels; PLAY and frame delivery arrive in
// subsequent changes, so a set-up session does not yet produce frames.
package rtsp
