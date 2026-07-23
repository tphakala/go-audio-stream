// Package aac depacketizes AAC from RTP MPEG4-GENERIC payloads
// (RFC 3640, mode AAC-hbr): AU-header parsing, multiple AUs per packet
// with interpolated timestamps, and fragmented AU reassembly.
//
// The package produces access units (raw codec bytes) and per-AU relative
// RTP tick offsets. It never constructs an audiostream.Frame and never
// consults a clock rate or presentation timestamp: the caller owns those
// and adds the packet's rtpTime to the relative offsets this package
// reports. A single Depacketizer carries the reassembly state for one RTP
// stream and is not safe for concurrent use.
package aac
