// Package rtsp implements an RTSP 1.0 client for audio ingestion from
// IP cameras and restreamers.
//
// The package is being assembled in pieces. What is present so far is
// the wire layer: serializing requests, parsing responses and
// server-initiated requests, framing interleaved binary data, and
// classifying response status codes into typed errors. The session
// client that drives DESCRIBE, SETUP and PLAY over these primitives,
// along with authentication, TLS and frame delivery, arrives in
// subsequent changes.
//
// Everything here is pure: no sockets, no goroutines, and no state
// beyond what the caller passes in. The input arrives from the network
// and is treated as hostile, so sizes are capped and every parser is
// total.
package rtsp
