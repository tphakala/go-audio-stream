// Package rtp parses RTP (RFC 3550) packet headers and maintains
// per-stream reception state: sequence tracking with gap counting,
// 64-bit timestamp unwrapping, SSRC change tolerance, and minimal RTCP
// (sender report parsing, receiver report construction).
//
// Parsers are total: they never panic and return a typed error on any
// truncated or malformed input.
package rtp
