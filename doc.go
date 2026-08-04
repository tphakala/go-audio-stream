// Package audiostream holds the shared data types of the go-audio-stream
// library: frames, codec identities, media kinds, and session statistics.
// It contains no I/O and no protocol logic. Protocol clients live in
// subpackages (rtsp) and deliver these types; RTP payload depacketizers
// live under depacket. It also defines Source, the source-agnostic capture
// contract every protocol client satisfies, and SourceInfo, its identity
// snapshot. Source covers lifecycle (Wait, Close), statistics (Stats) and
// identity (Info); frame delivery is not part of it, each concrete source
// configures delivery through its own Config.OnFrame.
package audiostream
