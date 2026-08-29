// Package packet and its subpackages hold the send-side RTP payload
// packetizers, each the inverse of a depacket counterpart. They carry no codec
// dependency and never import the rtsp layer: they turn raw media (little-endian
// PCM, or a validated Opus packet) into the bytes that go in an RTP payload,
// leaving the RTP header, sequencing, and timestamps to the caller.
package packet
