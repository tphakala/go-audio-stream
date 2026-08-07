// Package udpsource is a raw UDP audio source for go-audio-stream. It binds one
// UDP socket and delivers frames to Config.OnFrame on a reader goroutine, the
// same frame shape the rtsp and httpsource clients deliver. A Client satisfies
// audiostream.Source, so a supervisor can drive its lifecycle and read its
// statistics and identity without importing this package. It fills the gap where
// a sender pushes RTP or raw audio directly over UDP with no RTSP session.
//
// Two ingest modes are supported. ModeRTP treats each datagram as one RTP
// packet: it is parsed and its sequence continuity tracked (gaps, duplicates,
// SSRC changes, timestamp unwrap) through the shared rtsp/rtp primitives, then
// its payload is depacketized according to the configured payload type. Because
// there is no SDP, the caller supplies the payload type and its codec, clock
// rate, and channel count in Config. G.711 (companded to s16le), L16
// (byte-swapped from big-endian to s16le), and Opus (delivered as a compressed
// packet) are framed by reusing the library's existing depacketizers; an
// unrecognized codec is passed through opaquely. ModePCM treats each datagram as
// interleaved 16-bit PCM at the configured rate and channel count, delivering
// the whole-sample-frame prefix and byte-swapping a big-endian source to s16le.
//
// The frame's PTS comes from the RTP timestamp (ModeRTP) or a running sample
// count (ModePCM), through the same overflow-safe math the other sources use.
// The presentation-time-per-frame and payload kind (from AudioFormat, via
// PayloadKindFor) let a consumer treat frames uniformly across sources.
//
// A read-idle watchdog (Config.ReadIdle) ends the stream with
// audiostream.ErrReadTimeout when no datagram arrives within the window; it is
// implemented with a per-read socket deadline, so there is no separate watchdog
// goroutine. An optional Config.SourceIP restricts accepted datagrams to one
// sender IP; by default any sender on the bound port is accepted.
//
// Scope: this first version frames G.711, L16, and Opus over raw RTP and raw
// s16 PCM datagrams. AAC over raw RTP (which needs the fmtp depacketizer widths)
// and RTP reordering (rtsp/rtp exposes a Reorderer this source does not yet
// drive) are deliberate follow-ups: an out-of-order datagram is dropped and
// surfaces only as a sequence gap, not delivered late for the consumer to
// reorder. Because each supported codec here carries a self-contained payload
// with its own timestamp, that loss is non-corrupting (a dropped frame, not a
// desynchronized stream). There is no RTCP handling, so TrackStats.SenderClock
// stays invalid.
//
// Open binds the socket and returns an already-receiving source. Its ctx bounds
// only the bind and does not end the stream afterward; Close does. A Client
// holds a socket and a goroutine and must be released with Close; Wait reports
// the terminal cause. Close, Stats, Info and Format are safe from any goroutine,
// including from inside OnFrame; Wait is safe from other goroutines but must not
// be called from inside OnFrame, which would deadlock the reader it waits on.
//
// Errors are matched with errors.Is against the package sentinels
// (ErrInvalidConfig, ErrUnsupportedCodec, ErrBind, ErrConnectionClosed) and the
// root package's audiostream.ErrClosed and audiostream.ErrReadTimeout. UDP is
// connectionless, so there is no orderly end-of-stream: a healthy source runs
// until Close or the watchdog.
package udpsource
