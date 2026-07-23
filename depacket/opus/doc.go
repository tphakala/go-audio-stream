// Package opus depacketizes Opus from RTP payloads (RFC 7587), where
// each RTP packet carries exactly one Opus packet.
//
// Depacketize returns the raw Opus packet bytes; it never constructs
// audiostream.Frame. The caller owns frame assembly, timestamps, and
// clock rate.
package opus
