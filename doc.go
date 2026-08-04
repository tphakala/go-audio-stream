// Package audiostream holds the shared data types of the go-audio-stream
// library: frames, codec identities, media kinds, and session statistics.
// It contains no I/O and no protocol logic. Protocol clients live in
// subpackages (rtsp) and deliver these types; RTP payload depacketizers
// live under depacket. It also defines Source, the source-agnostic capture
// contract every protocol client satisfies, and SourceInfo, its identity
// snapshot, so a supervisor can drive any source through one lifecycle,
// delivery and introspection interface.
package audiostream
