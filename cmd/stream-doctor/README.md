# stream-doctor

`stream-doctor` is a diagnostic CLI for network audio streams. Point it at an
RTSP camera or NVR, or at an HTTP progressive audio source, and it reports
whether [go-audio-stream](https://github.com/tphakala/go-audio-stream) (and
therefore any consumer such as BirdNET-Go) can connect to the stream, what
audio track and codec it negotiates, and how the packets flow. It is a single
static binary with no runtime dependencies.

If you are troubleshooting why a camera's audio does not work as a BirdNET-Go
source, run `stream-doctor` against the same URL and share the report.

## Supported inputs

`stream-doctor` probes two kinds of URL:

- `rtsp://` and `rtsps://`: an RTSP 1.0 camera or NVR stream.
- `http://` and `https://`: an HTTP progressive source. The response must be
  WAV or raw PCM/L16. A compressed HTTP stream (MP3 or AAC) is detected and its
  codec is reported, but it is not decoded to WAV.

HLS (`.m3u8`) and raw `udp://` / `rtp://` sources are supported by the
go-audio-stream library but are not wired into this CLI, so `stream-doctor`
rejects those URLs with a usage error.

## Install

Download the prebuilt binary for your platform from the
[latest release](https://github.com/tphakala/go-audio-stream/releases), make it
executable, and (optionally) rename it to `stream-doctor`.

| Platform | Artifact |
|---|---|
| Linux x86-64 | `stream-doctor-vX.Y.Z-linux-amd64` |
| Linux Raspberry Pi 4/5, arm64 | `stream-doctor-vX.Y.Z-linux-arm64` |
| Linux Raspberry Pi 2/3/Zero 2, 32-bit (armv7) | `stream-doctor-vX.Y.Z-linux-arm` |
| Windows x86-64 | `stream-doctor-vX.Y.Z-windows-amd64.exe` |
| macOS Apple Silicon | `stream-doctor-vX.Y.Z-darwin-arm64` |
| macOS Intel | `stream-doctor-vX.Y.Z-darwin-amd64` |

The 32-bit Linux build targets armv7 (hard-float) and newer; the original Pi
Zero/1 (armv6) is not built.

Verify the download against the published checksums:

```sh
sha256sum -c checksums.txt          # Linux
shasum -a 256 -c checksums.txt      # macOS
```

On Linux and macOS, mark the binary executable and confirm the version:

```sh
chmod +x stream-doctor-*
./stream-doctor-* -version       # prints: stream-doctor X.Y.Z
```

The macOS binaries are signed with an Apple Developer ID and notarized, so
Gatekeeper allows them (an online check on first run). If Gatekeeper still
quarantines a copy, for example when the machine is offline, clear the flag:

```sh
xattr -d com.apple.quarantine stream-doctor-*
```

You can also build from source (Go 1.26+): from `cmd/stream-doctor/`, run
`go build .`. A source build reports its version as `dev`.

## Quickstart

Probe an RTSP camera and print the full report:

```sh
stream-doctor -report rtsp://user:pass@camera.local/stream
```

More examples:

```sh
# Capture 10 seconds of audio to a WAV file as well as reporting.
stream-doctor -report -wav out.wav rtsp://user:pass@camera.local/stream

# Force UDP transport (or try UDP then fall back to TCP).
stream-doctor -report -transport udp rtsp://camera.local/stream
stream-doctor -report -transport udp-then-tcp rtsp://camera.local/stream

# Cameras that reject an audio-only SETUP: set up all tracks.
stream-doctor -report -full-stream rtsp://camera.local/stream

# Self-signed rtsps, or credentials passed as flags instead of in the URL.
stream-doctor -report -insecure-tls rtsps://camera.local/stream
stream-doctor -report -user U -password P rtsp://camera.local/stream

# An HTTP progressive WAV source.
stream-doctor -report http://host:8000/stream.wav
```

Credentials can be given in the URL userinfo (`rtsp://user:pass@host/...`) or
with `-user` / `-password`; the URL userinfo wins if both are present.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-duration` | `10s` | capture window |
| `-timeout` | `10s` | dial and request timeout |
| `-read-idle` | `15s` | watchdog: no frames within this window ends capture |
| `-wav path` | off | also write the captured audio to a WAV file |
| `-report` | off | print a full diagnostic report |
| `-insecure-tls` | off | skip certificate verification for `rtsps` and `https` |
| `-insecure-auth` | off | permit HTTP Basic credentials over a plaintext `http` connection |
| `-full-stream` | off | set up all tracks, not just audio, for cameras that reject audio-only SETUP (RTSP only) |
| `-transport mode` | `tcp` | media transport: `tcp`, `udp`, or `udp-then-tcp` (RTSP only) |
| `-user username` | "" | stream username (overridden by URL userinfo) |
| `-password password` | "" | stream password (overridden by URL userinfo) |
| `-version` | | print the version and exit |

## Exit codes

`stream-doctor` returns a specific exit code so it can be scripted:

| Code | Name | Meaning |
|---|---|---|
| 0 | OK | an audio track was found, its codec is supported, and frames were captured |
| 1 | Usage | bad flags, URL, or scheme, or plaintext credentials refused (see `-insecure-auth`) |
| 2 | Connection | dial, DESCRIBE, SETUP, or PLAY failed before capture (includes HTTP non-auth error responses) |
| 3 | Auth | authentication failed (RTSP 401 or give-up, HTTP 401) |
| 4 | NoAudioTrack | connected, but the stream has no audio track |
| 5 | Unsupported | the audio track/codec, or the HTTP media format, cannot be turned into WAV |
| 6 | Capture | capture started but produced zero frames, or failed mid-capture |

## Sharing a report

The `-report` output is safe to paste into a public issue. The target URL is
reduced to its scheme and path (`rtsp://[redacted]/stream`), so embedded
credentials never appear, and error strings are scrubbed of hostnames, IP
addresses, and other identifying detail before they are printed.

## Reporting a stream problem

When a camera's audio is not working as a BirdNET-Go source, run:

```sh
stream-doctor -report rtsp://user:pass@camera.local/stream
```

and include, in the issue:

- the full `-report` output (already redacted),
- the exit code (`echo $?` on Linux/macOS, `echo %ERRORLEVEL%` on Windows), and
- your OS and CPU architecture.

Quick reading of the result:

- exit 3 (Auth): check the username and password.
- exit 4 (NoAudioTrack) or 5 (Unsupported): the stream carries no audio, or an
  audio codec go-audio-stream does not handle.
- exit 2 (Connection) with `-transport udp`: retry with `-transport tcp` or
  `-transport udp-then-tcp`; some networks block UDP.
- the camera rejects an audio-only stream: add `-full-stream`.
