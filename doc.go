// Package audiostream holds the shared data types of the go-audio-stream
// library: frames, codec identities, media kinds, and session statistics.
// It contains no I/O and no protocol logic. Protocol clients live in
// subpackages (rtsp) and deliver these types; RTP payload depacketizers
// live under depacket.
package audiostream
