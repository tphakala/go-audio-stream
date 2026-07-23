// Package testserver is a scripted in-process RTSP server used by the
// test suite to simulate real-server behavior, including interop quirks
// (keepalive expectations, control URL variants, mid-stream requests,
// renumbered interleaved channels, abrupt disconnects). It is not a
// general-purpose RTSP server and is never exported.
package testserver
