# AGENTS.md

Orientation for coding agents and automated reviewers working on this repository. README.md is the user-facing reference (usage examples, codec and transport coverage); this file is the map for changing and reviewing the code.

## What this is

`github.com/tphakala/go-audio-stream` (root package `audiostream`) is a pure-Go library that pulls audio off the network and hands the consumer timestamped frames. It aims to:

1. **Build anywhere Go builds.** No cgo, no runtime dependencies in the library module, static cross-compiles for every target the CI and release workflows build.
2. **Depacketize and report, never decode.** Compressed audio (AAC, Opus, MP3, FLAC) is delivered as coded frames for the consumer to decode with its own codec library (go-aac, go-opus). Only the companded or raw PCM codecs (G.711, G.726, L16) are expanded to little-endian s16le, because that is part of depacketization. No resampling, mixing or decoding of complex codecs here.
3. **Survive hostile input and flaky peers.** Every wire parser is total and size-capped, and a quiet or misbehaving peer ends in a typed error rather than a hang or a panic. Consumers are long-running, unattended services (BirdNET-Go is the main one).

Sources, all satisfying one `audiostream.Source` interface:

| Package | Source |
|---|---|
| `rtsp` | RTSP 1.0 client: DESCRIBE/SETUP/PLAY, TCP-interleaved or opt-in UDP RTP, Basic/Digest auth, `rtsps`, keepalive, RTCP Receiver Reports |
| `httpsource` | HTTP(S) progressive: WAV, raw L16/PCM, MP3 and ADTS AAC (Icecast/SHOUTcast) |
| `udpsource` | Raw `udp://` / `rtp://` with no RTSP session (`ModeRTP`, `ModePCM`); caller describes the codec |
| `hlssource` | HLS (m3u8) live or VOD, AAC out of MPEG-TS or fMP4/CMAF |
| `supervisor` | Optional wrapper that turns any single-session `Source` into a reconnecting one with capped exponential backoff |

Non-goals: decoding, resampling, playback, video, DRM, a general-purpose RTSP server. The `packet/` packetizers and the RTP/RTCP marshal and SDP writer code are send-side primitives, not a server.

The public API is not stable before v1.0.0. Prefer the better design over compatibility: no deprecated aliases or shims. A changed signature needs every caller in this repo updated (including `cmd/stream-doctor`), and a changed error or `Source` contract needs the package doc and README updated with it.

## Design rules you must not break

A change that violates one is wrong even if tests pass.

1. **No cgo, no runtime dependencies in the library module.** The root `go.mod` requires only `go-ruleguard/dsl`, behind a build tag for lint. Third-party code (go-aac, go-opus, go-wav) lives only in `cmd/stream-doctor`, a separate module.
2. **Depacketize, never decode.** `AudioFormat.SampleRate` and `Channels` are set only for `KindPCMS16LE`, where the library produced the PCM; for every other kind they are 0 because the advertised rate is untrustworthy (an AAC RTP clock is often 90000). `PayloadKindFor` is the single source of truth for codec to kind. Adding a codec means classifying it there.
3. **Frame data is borrowed.** `Frame.Data` aliases library memory and is valid only for the `OnFrame` call. Never hand a consumer a slice you will reuse without saying so, and never retain one past the callback in library code.
4. **Delivery is configured at construction.** `OnFrame` and `OnCodecUpdate` are `Config` fields, not methods on `Source`, so delivery is race-free however early the peer sends. Do not add a registration method.
5. **Concurrency contract of `Source`.** `Close`, `Stats` and `Info` are safe from any goroutine, including from inside `OnFrame`. `Wait` must never be called from inside `OnFrame` (it deadlocks the reader it waits on). `Close` is idempotent. After `Wait` returns, `OnFrame` is never called again. A `Client` owns a socket and a goroutine and must be released with `Close`.
6. **Typed terminal causes.** `Wait` returns `audiostream.ErrClosed` after `Close`, the caller's `ctx.Err()` if it cancels first, `audiostream.ErrReadTimeout` when the read-idle watchdog expires, and protocol causes as typed errors matchable with `errors.Is`/`errors.As`. The first cause wins. Redirects surface as `*audiostream.RedirectError` (matches `ErrRedirect`). Never return an opaque string error where a sentinel applies.
7. **Parsers are total and capped.** Anything parsed from the network (SDP, RTP, RTCP, RTSP messages, interleaved framing, Transport and Session headers, WAV/ADTS/MP3/TS/fMP4 framing, m3u8) never panics, never allocates unbounded memory from a peer-controlled length, and returns a typed error on bad input. New parsers get a fuzz target and an entry in `task fuzz`.
8. **Bounded waits.** Every wait, retry and resync loop is bounded or wakes on `Close`. Open-phase `ctx` bounds only the open (`httpsource.Open`, `udpsource.Open`, `hlssource.Open` detach the stream from it); `Close` ends the stream.
9. **Credentials never leak.** `SourceInfo.URL` and every error, log line and `stream-doctor` report strip userinfo. URL parsing is shared through `internal/urltarget`; use it rather than re-parsing. Basic credentials over plaintext http are refused unless explicitly allowed.
10. **Honest degradation.** An unrecognized codec or non-audio track is delivered as `KindOpaque` raw payload or skipped, not failed or mis-decoded. An unsupported container or compressed format is rejected at `Open` with a typed error rather than guessed (Ogg, RF64/BW64, encrypted HLS, and so on).
11. **Low allocation on the delivery path.** Depacketize-and-deliver is allocation-light; `AllocsPerRun` tests and benchmarks (`rtsp/pipeline_test.go`, `udpsource/deliver_bench_test.go`, the depacketizer and `rtp` marshal tests) guard it. Error paths may allocate.

## Layout

```
doc.go, source.go     Package doc; Source and SourceInfo (the source-agnostic contract)
frame.go, format.go   Frame; AudioFormat, PayloadKind, PayloadKindFor
codec.go, media.go    Sealed Codec sum type (CodecAAC, CodecOpus, ...); Law, G726BitRate, G726Packing
stats.go, errors.go   TrackStats, SenderClock, Stats; ErrClosed, ErrReadTimeout, ErrRedirect, RedirectError

rtsp/                 RTSP client. Wire layer (message, request, transport, status, auth, controlurl)
                      is pure; session layer (client, describe, setup, play, reader, keepalive, udp,
                      udprecv, pipeline, state) owns the socket and reader goroutine
rtsp/sdp/             SDP parser and writer (RFC 8866 subset RTSP needs)
rtsp/rtp/             RTP/RTCP parse and marshal, Stream (sequence tracking, timestamp unwrap, SSRC
                      re-baseline), Reorderer, NTP and SenderClock helpers
httpsource/           HTTP progressive client; WAV, L16, MP3, ADTS AAC framers
udpsource/            Raw UDP source (ModeRTP / ModePCM), opt-in Reorder and RTCP
hlssource/            HLS client, playlist parser, TS and fMP4 demuxers
supervisor/           Reconnecting wrapper (Factory, backoff with jitter, State machine)

depacket/{aac,latm,opus,g711,g726,mp3,flac}   RTP payload to frames, one Depacketizer per RTP stream
packet/{l16,opus,flac}                        Send-side inverses of the above (no RTP header or clock)

internal/             adts, adtsframe, mp3, mp4 (framers/parsers); httpauth (Basic/Digest);
                      mediatime (overflow-safe PTS math); urltarget (credential-safe URL handling);
                      testserver (scripted in-process RTSP and UDP servers, tests only)
cmd/stream-doctor/    Diagnostic CLI, its own module (own go.mod, replace => ../..); release binaries
rules/rules.go        gocritic ruleguard matchers (build tag `ruleguard`, lint-only)
tools/tools.go        Anchors lint-only deps for `go mod tidy`
testdata/fixtures/    Scrubbed, committed protocol fixtures
```

Frame production differs by source and consumers rely on it: RTSP and `udpsource` `ModeRTP` take PTS from the unwrapped RTP clock and report `SeqGap` from sequence tracking; `httpsource` and `ModePCM` derive PTS from the delivered-sample count and report RTPTime and SeqGap as 0; `hlssource` accumulates PTS from access-unit durations and reports dropped or `EXT-X-GAP` segments as `SeqGap`. Keep that consistent when touching a source.

A depacketizer carries reassembly state for one RTP stream and is not safe for concurrent use. Depacketizers return raw access units plus relative tick offsets; the caller (pipeline) owns clocks, PTS and `Frame` construction.

## Testing approach

Tests run offline with no real peers.

- **RTSP and UDP**: `internal/testserver` is a scripted in-process server that reproduces interop quirks (keepalive expectations, control URL variants, mid-stream requests, renumbered interleaved channels, abrupt disconnects). Add a quirk to it rather than a one-off fake in a test.
- **HTTP and HLS**: `httptest` servers and in-repo fixtures (`hlssource/fixture_test.go`, `fixture_fmp4_test.go`, `testdata/fixtures/`). Fixtures derived from real captures must be scrubbed of LAN details and credentials.
- **Fuzzing**: every wire parser has a `Fuzz*` target; the full list is in `task fuzz`. Seed corpora live under `testdata/fuzz`. Add new targets to the Taskfile.
- **Supervisor**: driven with an injected clock (`supervisor/clock.go`) and a fake `Factory`; never real sleeps.
- **Races**: CI runs `go test -race` on Linux and Windows. Join every goroutine a test starts. The library is also used on 32-bit ARM (the release ships a `linux/arm` stream-doctor), so avoid 64-bit-only assumptions.
- Dependent module: `cmd/stream-doctor` has byte-exact golden test data under `internal/doctor/testdata`; `.gitattributes` forces LF so Windows runners match.

New behaviour gets a test through the relevant seam. A bug fix gets a regression test that fails before the fix. A test must be able to fail: check the assertion would break if the fixed line were reverted, and avoid fixtures where every path yields the same result. Benchmarks and `AllocsPerRun` tests back any performance or allocation claim.

## Commands

Go 1.27 and golangci-lint v2.13.2 (pinned in `ci.yml`). Tasks are in `Taskfile.yml`.

```
task check        # library gate: build, vet (amd64 and arm64), lint, gofmt, race tests
task doctor:check # same gate for the cmd/stream-doctor module
task check:all    # both; run this before pushing
task fuzz         # every fuzz target, 2m each
task tidy         # go mod tidy and verify
```

The library and `cmd/stream-doctor` are separate modules, so `go build ./...` at the root does not cover the CLI; use the `doctor:*` tasks or `check:all`. CI also builds and tests on `windows-latest`, so avoid Unix-only assumptions (path separators, signals, file modes) in code and tests.

The project tracks the latest stable Go and tooling. When a new Go minor ships, bump `go.mod`, `cmd/stream-doctor/go.mod` and `go-version` in both workflows together; the golangci-lint version string in `ci.yml` is bumped by hand. Use current language and standard library features; do not add shims for older Go.

Releases are tag-driven (`v*.*.*` triggers `release.yml`, which cross-builds stream-doctor with `CGO_ENABLED=0` and the version injected through `-ldflags`, then drafts a release).

## Conventions

- Lint config is `.golangci.yaml` (adapted from BirdNET-Go: gocritic with ruleguard, revive, errorlint, exhaustive with `default` counting as exhaustive, gocognit 50, modernize, perfsprint). Use `errors.New` for constant messages, not `fmt.Errorf` (enforced by `rules/rules.go`). Fix new findings rather than disabling a linter.
- Error strings are prefixed with the package name (`audiostream:`, `supervisor:`, and so on). Wrap with `%w`. Sentinels are matched with `errors.Is`; typed errors expose an `Is` method for their category and are recovered with `errors.As`.
- Package docs are real documentation and are kept in sync with behaviour: each source's `doc.go` states its scope, terminal causes and what is out of scope. Update it, and the README Status section, whenever scope changes. Doc claims that are false for some transport or source are defects.
- Deliberately coarse behaviour is documented where it is implemented (Receiver Reports carry no jitter or fraction-lost, only AAC-hbr among RFC 3640 modes, AAC PTS interpolation assumes 1024 samples per frame). Do not paper over these; either fix them or keep the note accurate.
- Comments explain why (the protocol quirk, the failure being avoided), cite the RFC or spec section where one applies, and match the surrounding density.
- Adding a codec touches: a `Codec*` variant in `codec.go`, `PayloadKindFor` in `format.go`, SDP mapping in `rtsp/sdp/codec.go`, a depacketizer under `depacket/`, delivery in `rtsp/pipeline.go` and `udpsource`, tests and a fuzz target, `stream-doctor` reporting, and the README coverage list.
- Adding a source means implementing `audiostream.Source`, a compile-time assertion for it (see `httpsource/source_assert_test.go`), the read-idle watchdog returning `ErrReadTimeout`, credential-stripped `Info`, and a note in `supervisor` docs if it has per-session state a consumer must know about.
- Commits follow Conventional Commits with a scope: `feat(rtsp): ...`, `fix(hlssource): ...`, `chore: ...`.

## Review guidance

For CodeRabbit, Copilot and human reviewers. Rate findings on what a consumer of the library or a hostile peer would see.

Flag these, they are real defects here:

- Any break of the design rules above.
- A parser that can panic, loop forever, or allocate proportionally to an unvalidated peer-supplied length.
- A path where `Open`, `Dial`, `Wait` or a reader can block forever, or a retry or resync loop with no bound and no `Close` wake-up.
- A callback or `Source` method that can deadlock when called from `OnFrame` (anything but `Wait`), or `OnFrame` invoked after `Wait` returned.
- Retaining `Frame.Data` (or a codec slice from `OnCodecUpdate`) past the callback inside the library, or handing out a buffer that is later overwritten.
- A terminal cause mapped to the wrong sentinel (a `Close` reported as a read timeout, a server teardown reported as `ErrClosed`), or a raw `fmt.Errorf` string where a typed error applies.
- Credentials (URL userinfo, Authorization values, digest responses) reaching an error, log line, `SourceInfo` or `stream-doctor` output.
- Timestamp bugs: PTS or RTP unwrap overflow, a discontinuity (SSRC change, reconnect, dropped segment) not reflected in `SeqGap`, PTS that goes backwards.
- A new allocation on the steady-state deliver path without a benchmark justifying it.
- A new wire parser without a fuzz target, or a test that cannot fail on the behaviour it names.
- Windows-hostile code or tests, or 64-bit-only assumptions that break 32-bit targets.
- A doc sentence (package doc, README, comment) that is false for some source or transport.

Intentional, do not flag:

- Per-source differences in how PTS, `RTPTime` and `SeqGap` are produced (documented above and in each `doc.go`).
- `KindOpaque` passthrough and skipped tracks for unrecognized codecs.
- The absence of decoding, resampling, jitter estimation, or Receiver Report fraction-lost.
- Out-of-order datagrams dropped (and surfacing as a sequence gap) in `udpsource` when `Config.Reorder` is off.
- `Stats` and `Info` resetting on every supervisor reconnect (current-session semantics, documented in `supervisor/doc.go`).
- Ignored errors from best-effort cleanup (`_ = conn.Close()` on an error path).
- Breaking changes to the exported API before v1.0.0, and the absence of deprecated aliases or shims.
- Code duplicated between sources where the shared logic would need a cross-package dependency the import boundaries forbid (`supervisor` imports only the root package).

## Things that do not belong in the repo

- Raw protocol captures (`/testdata/captures/`, gitignored): they can carry LAN addresses and credentials. Commit only scrubbed fixtures under `testdata/fixtures/`.
- Build outputs (`/bin/`, `*.exe`, `*.test`, coverage and profile files).
