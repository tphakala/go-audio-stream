// Package latm depacketizes AAC access units from RTP MP4A-LATM payloads
// (RFC 3016 / RFC 6416, ISO/IEC 14496-3 LATM): the out-of-band and in-band
// StreamMuxConfig cases, and single or multiple subframes per
// AudioMuxElement.
//
// The package produces access units (raw AAC bytes) and per-AU relative
// RTP tick offsets, plus the AudioSpecificConfig extracted from the
// StreamMuxConfig. It never constructs an audiostream.Frame and never
// consults a clock rate or presentation timestamp: the caller owns those
// and adds the packet's rtpTime to the relative offsets this package
// reports. A single Depacketizer carries the retained StreamMuxConfig for
// one RTP stream and is not safe for concurrent use.
package latm
