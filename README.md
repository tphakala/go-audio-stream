# go-audio-stream

Pure-Go audio stream ingestion. Part of the tphakala/go-* native audio
series (go-aac, go-opus, go-flac, go-m4a, go-wav).

**Status: phase 1 shipped.** An RTSP 1.0 client that pulls audio from IP
cameras and restreamers over TCP-interleaved RTP, with the SDP and RTP
parsers and the AAC / Opus / G.711 depacketizers behind it. The library
delivers timestamped codec frames; decoding stays with the consumer
(go-aac, go-opus). Still pending: broad live-camera interop across vendors
and long-running soak testing.

## Features

- RTSP 1.0 client: TCP-interleaved RTP transport, Basic and Digest auth,
  rtsps TLS, per-track setup with optional payload discard, session
  keepalive, and RTCP receiver reports.
- An SDP parser for DESCRIBE responses and an RTP packet parser, both with
  strict size caps because the input is untrusted.
- Depacketizers for AAC (RFC 3640 AAC-hbr), Opus, and G.711 (mu-law and
  A-law).
- `stream-doctor`: a diagnostic CLI that connects to an RTSP URL and
  reports the handshake, negotiated track, codec, and packet statistics.

## Layout

- `audiostream` (root): shared types (Frame, Codec, MediaKind, Stats).
- `rtsp/`: RTSP client, with `rtsp/sdp/` and `rtsp/rtp/`.
- `depacket/`: RTP payload depacketizers (aac, opus, g711).
- `cmd/stream-doctor/`: the diagnostic CLI (a separate module).

## License

MIT
