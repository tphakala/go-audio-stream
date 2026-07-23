# go-audio-stream

[![CI](https://github.com/tphakala/go-audio-stream/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-audio-stream/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-audio-stream.svg)](https://pkg.go.dev/github.com/tphakala/go-audio-stream)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-audio-stream)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor)](https://github.com/sponsors/tphakala)

A pure-Go RTSP client for pulling audio off IP cameras and restreamers. It runs
the DESCRIBE / SETUP / PLAY handshake over TCP-interleaved RTP, depacketizes AAC,
Opus and G.711, and hands the consumer timestamped codec frames. No cgo, no
runtime dependencies.

Where the rest of the family reads and writes files, this library brings audio
in off the network, so it sits one step upstream of them: the frames it delivers
are what [go-aac](https://github.com/tphakala/go-aac) and
[go-opus](https://github.com/tphakala/go-opus) decode. It shares their
conventions and their audio vocabulary, alongside
[go-flac](https://github.com/tphakala/go-flac),
[go-m4a](https://github.com/tphakala/go-m4a) and
[go-wav](https://github.com/tphakala/go-wav).

## Install

```bash
go get github.com/tphakala/go-audio-stream
```

Requires Go 1.26 or newer.

## Status

**Phase 1 shipped.** The RTSP client is complete and delivers frames; what
remains is broad live-camera interop across vendors and long-running soak
testing, not core functionality.

- **RTSP 1.0 client**: the full DESCRIBE / SETUP / PLAY lifecycle over the
  TCP-interleaved RTP profile, per-track setup with optional payload discard,
  Basic and Digest authentication (answered and retried automatically, including
  a stale-nonce rotation), `rtsps` TLS, session keepalive, and RTCP Receiver
  Reports.
- **Depacketizers**: AAC (RFC 3640 AAC-hbr), Opus (RFC 7587), and G.711 mu-law
  and A-law. An unrecognized codec, a non-audio track, or an AAC mode this
  milestone does not decode degrades to raw payload delivery rather than
  failing the session.
- **Frame delivery**: each frame carries its track ID, a presentation time
  unwrapped from the RTP clock, the raw RTP timestamp, the local receive time,
  and the count of packets lost immediately before it. Per-track receive
  statistics (packets, bytes, sequence gaps, malformed, SSRC resets) are
  available through `Stats`.
- **Untrusted input**: every wire parser (SDP, RTP, RTCP, the RTSP message
  layer, the interleaved framing) is total and size-capped, and the fuzz suite
  covers each one. The read-idle watchdog ends a session whose peer goes quiet.
- **Resilience**: a mid-stream SSRC change is tolerated and re-baselined, a
  desynchronized framing stream is resynchronized within a bounded budget, and
  a payload type the camera's SDP did not declare is adopted from the wire so a
  mis-declaring camera still delivers audio.

Not yet on this branch: `stream-doctor`, a diagnostic CLI that connects to an
RTSP URL and prints the handshake, the negotiated track and codec, and packet
statistics. It lands with the tooling PRs.

The transport is TCP-interleaved only; there is no UDP transport. Within that
profile a few details are deliberately coarse and documented where they are
implemented: Receiver Reports carry no jitter or fraction-lost estimate, only
AAC-hbr among RFC 3640's modes is depacketized, and AAC PTS interpolation
assumes 1024 samples per frame.

## Usage

Register an `OnFrame` callback, then drive the lifecycle from a single
goroutine:

```go
import (
    "context"

    audiostream "github.com/tphakala/go-audio-stream"
    "github.com/tphakala/go-audio-stream/rtsp"
)

ctx := context.Background()
c, err := rtsp.Dial(ctx, rtsp.Config{
    URL: "rtsp://user:pass@camera.local/stream",
    OnFrame: func(f audiostream.Frame) {
        // f.Data is the codec frame (AAC AU, Opus packet, or s16le PCM for
        // G.711). It aliases library memory and is valid only for this call;
        // copy it to keep it. Hand it to a codec decoder from the family.
        _ = f
    },
})
if err != nil {
    // handle
}
defer c.Close()

tracks, err := c.Describe(ctx)
// ...
if err := c.Setup(ctx, tracks[0], rtsp.SetupOptions{}); err != nil {
    // ...
}
if err := c.Play(ctx); err != nil {
    // ...
}

// Play returns once the stream is running; frames arrive on OnFrame from the
// reader goroutine. Wait blocks until the session ends and returns the cause.
err = c.Wait(ctx)
```

`Close`, `Wait`, `Stats` and `SessionInfo` are safe from any goroutine;
`Describe`, `Setup` and `Play` must be called in order from one goroutine.
`Frame.Data` is owned by the library and valid only for the duration of the
callback, so copy it before retaining it.

## Layout

- `audiostream` (root): the shared types a consumer sees (`Frame`, `Codec`,
  `MediaKind`, `Stats`).
- `rtsp/`: the RTSP client, with the `rtsp/sdp/` and `rtsp/rtp/` wire parsers.
- `depacket/`: the RTP payload depacketizers (`aac`, `opus`, `g711`).
- `cmd/stream-doctor/`: the diagnostic CLI (a separate module; see Status).

## License

MIT. See [LICENSE](LICENSE).

## Sponsor

If this is useful to you, [sponsorship](https://github.com/sponsors/tphakala) is
welcome.
