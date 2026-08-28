# go-audio-stream

[![CI](https://github.com/tphakala/go-audio-stream/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-audio-stream/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-audio-stream.svg)](https://pkg.go.dev/github.com/tphakala/go-audio-stream)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-audio-stream)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor)](https://github.com/sponsors/tphakala)

A pure-Go library for pulling audio off the network and handing the consumer
timestamped frames. Its primary source is an RTSP client for IP cameras and
restreamers: it runs the DESCRIBE / SETUP / PLAY handshake and pulls RTP over
interleaved TCP or, opt-in, unicast UDP, depacketizes AAC (including MP4A-LATM),
Opus, G.711, G.726 ADPCM, and L16 PCM, and delivers codec frames. A second source, the
`httpsource` package, pulls audio off an HTTP(S) progressive endpoint: a WAV
response or raw L16/PCM delivered as the same s16le frames, or a compressed MP3
or ADTS AAC (Icecast/SHOUTcast) stream framed and delivered as coded frames. A
third source, `udpsource`, receives raw `udp://` / `rtp://` audio that no RTSP
session negotiated, framing either RTP packets or interleaved s16 PCM datagrams
the caller describes. A fourth source, `hlssource`, follows an HLS (m3u8)
playlist, live or VOD, and demuxes AAC access units out of MPEG-TS or fMP4/CMAF
(EXT-X-MAP init segment plus `.m4s` fragments) segments, delivering them with the
same AudioSpecificConfig an RTSP AAC track carries. All four satisfy one `Source`
interface, so a consumer can drive any of them uniformly, and an optional
`supervisor` wraps any single-session source into a transparently reconnecting
one with capped exponential backoff. No cgo, no runtime dependencies.

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

**Core shipped.** The RTSP client, the HTTP progressive source, and the raw
UDP source all deliver frames, with RTP/AVP unicast UDP transport and MP4A-LATM
in the RTSP path. What remains is broad live-camera interop across vendors and
long-running soak testing, not core functionality.

- **RTSP 1.0 client**: the full DESCRIBE / SETUP / PLAY lifecycle over the
  TCP-interleaved RTP profile by default, with opt-in RTP/AVP unicast UDP
  transport (and a UDP-then-TCP fallback), per-track setup with optional payload
  discard, Basic and Digest authentication (answered and retried automatically,
  including a stale-nonce rotation), `rtsps` TLS, session keepalive, and RTCP
  Receiver Reports.
- **HTTP progressive source** (`httpsource`): a single GET against an audio
  endpoint. A WAV response (its `fmt` chunk authoritative) or a raw response
  (`audio/L16`, or an unlabeled `application/octet-stream` or `audio/pcm`) is
  resolved to a rate and channel count and delivered as little-endian s16le, the
  same frame shape the RTSP client delivers; big-endian `audio/L16` is
  byte-swapped on the way out. A compressed MP3 response (`audio/mpeg` or
  `audio/mp3`, as Icecast and SHOUTcast serve it) is framed on MPEG frame
  boundaries, and an ADTS AAC response (`audio/aac` or `audio/aacp`) is framed
  on ADTS frame boundaries with a synthesized AudioSpecificConfig, matching an
  RTSP AAC track; both are delivered as `KindCompressed` coded frames for the
  consumer to decode, never decoded here. Other compressed and container formats
  (Ogg, RF64/BW64) are rejected at `Open` rather than mis-decoded, and Basic
  credentials over plaintext http are refused unless explicitly allowed.
- **Raw UDP source** (`udpsource`): one bound UDP socket for audio pushed
  directly over UDP with no RTSP session. `ModeRTP` parses each datagram as an
  RTP packet (sequence tracking, SSRC re-baseline, timestamp unwrap) and
  depacketizes it by the caller-supplied payload type; `ModePCM` reads each
  datagram as interleaved s16 PCM. Because there is no SDP, the caller supplies
  the codec, clock rate, and channel count. G.711, G.726, L16, and Opus are framed
  today; AAC over raw RTP, RTP reordering, and RTCP are deferred, so an
  out-of-order datagram is dropped and surfaces as a sequence gap. An optional
  source-IP filter and a read-idle watchdog bound what it accepts and how long a
  silent sender keeps the session open.
- **Depacketizers**: AAC (RFC 3640 AAC-hbr) and MP4A-LATM (RFC 3016, out-of-band
  and in-band StreamMuxConfig), Opus (RFC 7587), G.711 mu-law and A-law, G.726
  ADPCM (16/24/32/40 kbps, from a `G726-NN` rtpmap (RFC 3551), an
  `AAL2-G726-NN` one (ITU-T I.366.2 Annex E; the same codewords in the
  opposite bit order), or the static payload type 2), and L16 linear PCM
  (RFC 3551, from an `L16` rtpmap or the static payload types 10 and 11).
  G.711, G.726, and L16 are delivered as little-endian s16le PCM, so a consumer
  gets PCM in one byte order regardless of which of them it received. An
  unrecognized codec, a non-audio track, or an AAC mode this milestone does not
  decode degrades to raw payload delivery rather than failing the session.
- **Source interface**: `rtsp.Client`, `httpsource.Client` and
  `udpsource.Client` all implement the root package's `audiostream.Source`
  (`Wait`, `Close`, `Stats`, `Info`), so a supervisor can hold an
  `audiostream.Source` and drive any source's lifecycle, read its statistics,
  and read its source-neutral identity (`SourceInfo`) without importing the
  concrete package. Frame delivery is not part of the
  interface: each source registers `OnFrame` at construction, so delivery is
  race-free no matter how early the peer starts sending.
- **Frame delivery**: each frame carries its track ID, a presentation time, the
  raw RTP timestamp, the local receive time, and the count of packets lost
  immediately before it. From the RTSP source the presentation time is unwrapped
  from the RTP clock, the RTP timestamp is the packet's, and the lost-packet
  count comes from sequence-number tracking. The HTTP progressive source has no
  RTP clock: it derives the presentation time from the running delivered-sample
  count and reports the RTP timestamp and the lost-packet count as 0. The raw
  UDP source mirrors whichever it resembles: `ModeRTP` carries the packet's RTP
  timestamp and sequence-derived loss like the RTSP source, `ModePCM` derives
  the presentation time from the delivered-sample count like the HTTP source.
  Per-track receive statistics are available through `Stats`: accepted packets,
  payload bytes (compressed audio) and wire bytes (network bandwidth), sequence
  gaps, duplicates, malformed drops, SSRC resets, the wall-clock time of the last
  frame, and, on an RTCP-bearing source, the RTP-to-wall-clock `SenderClock`
  mapping from the most recent Sender Report. The snapshot is stamped with
  `CapturedAt`.
- **Untrusted input**: every wire parser (SDP, RTP, RTCP, the RTSP message
  layer, the interleaved framing) is total and size-capped, and the fuzz suite
  covers each one. The read-idle watchdog ends a session whose peer goes quiet.
- **Resilience**: a mid-stream SSRC change is tolerated and re-baselined, a
  desynchronized framing stream is resynchronized within a bounded budget, and
  a payload type the camera's SDP did not declare is adopted from the wire so a
  mis-declaring camera still delivers audio.

`stream-doctor`, a diagnostic CLI (in `cmd/stream-doctor/`, a separate module
built and tested in CI), connects to an RTSP or HTTP progressive URL and prints
the handshake, the negotiated track and codec, and packet statistics. A
`-transport` flag selects the RTSP media transport (`tcp`, `udp`, or
`udp-then-tcp`), and the walkthrough renders the negotiated UDP endpoints.
Prebuilt binaries and usage are documented in
[`cmd/stream-doctor/README.md`](cmd/stream-doctor/README.md).

A few transport details are deliberately coarse and documented where they are
implemented: Receiver Reports carry no jitter or fraction-lost estimate, only
AAC-hbr among RFC 3640's modes is depacketized, and AAC PTS interpolation
assumes 1024 samples per frame.

## Usage

### RTSP

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
        // G.711 and L16). It aliases library memory and is valid only for this call;
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

### HTTP

`httpsource.Open` performs the whole handshake and returns an already-delivering
source, so there is no separate describe/setup/play step:

```go
import (
    "context"

    audiostream "github.com/tphakala/go-audio-stream"
    "github.com/tphakala/go-audio-stream/httpsource"
)

ctx := context.Background()
src, err := httpsource.Open(ctx, httpsource.Config{
    URL: "http://device.local/audio.wav",
    OnFrame: func(f audiostream.Frame) {
        // f.Data is little-endian s16le PCM. It aliases library memory and is
        // valid only for this call; copy it to keep it.
        _ = f
    },
})
if err != nil {
    // handle
}
defer src.Close()

// Open returns an already-delivering source; frames arrive on OnFrame from the
// reader goroutine. Wait blocks until the stream ends and returns the cause.
err = src.Wait(ctx)
```

`Open`'s `ctx` bounds only the open phase; cancelling it after `Open` returns
does not end the stream, `Close` does. For a raw L16/PCM endpoint whose media
type does not carry a rate and channel count, supply them through
`Config.Format`.

### UDP

`udpsource.Open` binds one UDP socket and returns an already-receiving source.
There is no SDP, so for `ModeRTP` the caller supplies the payload type and its
codec, clock rate, and channel count; `ModePCM` instead reads each datagram as
interleaved s16 PCM described by `Config.Format`:

```go
import (
    "context"

    audiostream "github.com/tphakala/go-audio-stream"
    "github.com/tphakala/go-audio-stream/udpsource"
)

ctx := context.Background()
src, err := udpsource.Open(ctx, udpsource.Config{
    ListenAddr:  ":5004",
    Mode:        udpsource.ModeRTP,
    PayloadType: 0, // PCMU
    Codec:       audiostream.CodecG711{Law: audiostream.MuLaw},
    ClockRate:   8000,
    Channels:    1,
    OnFrame: func(f audiostream.Frame) {
        // Same frame shape as the other sources; f.Data aliases library memory
        // and is valid only for this call, so copy it to keep it.
        _ = f
    },
})
if err != nil {
    // handle
}
defer src.Close()

err = src.Wait(ctx)
```

### One interface over all three

`rtsp.Dial`, `httpsource.Open` and `udpsource.Open` each return a value that
satisfies `audiostream.Source`, so code that does not care which kind it holds
can take the interface and call `Wait`, `Close`, `Stats` and `Info` on it:

```go
func drain(ctx context.Context, s audiostream.Source) error {
    defer s.Close()
    return s.Wait(ctx)
}
```

## Layout

- `audiostream` (root): the shared types a consumer sees (`Frame`, `Codec`,
  `MediaKind`, `Stats`) and the source-agnostic `Source` interface with its
  `SourceInfo`.
- `rtsp/`: the RTSP client, with the `rtsp/sdp/` and `rtsp/rtp/` wire parsers.
- `httpsource/`: the HTTP(S) progressive source (WAV, raw L16/PCM, MP3, ADTS AAC).
- `udpsource/`: the raw `udp://` / `rtp://` source (RTP or interleaved-PCM datagrams).
- `depacket/`: the RTP payload depacketizers (`aac`, `latm`, `opus`, `g711`).
- `cmd/stream-doctor/`: the diagnostic CLI (a separate module; see Status).

## License

MIT. See [LICENSE](LICENSE).

## Sponsor

If this is useful to you, [sponsorship](https://github.com/sponsors/tphakala) is
welcome.
