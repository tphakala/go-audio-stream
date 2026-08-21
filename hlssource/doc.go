// Package hlssource pulls audio off an HLS (HTTP Live Streaming) endpoint and
// delivers it as timestamped AAC access units, satisfying the same
// audiostream.Source contract as the rtsp, httpsource and udpsource clients.
//
// Open fetches an m3u8 playlist, resolves a master (multivariant) playlist to a
// media playlist, downloads segments in media-sequence order, demuxes the AAC
// elementary stream out of the MPEG-TS segments, and delivers each access unit
// to Config.OnFrame on a single reader goroutine. A VOD playlist (one carrying
// EXT-X-ENDLIST) is played to its end, after which Wait returns ErrStreamEnded; a
// live playlist is reloaded on the RFC 8216 cadence (one target duration after a
// reload that produced new segments, half that after one that did not) until
// Close or the read-idle watchdog.
//
// Like the other sources, this package depacketizes and reports; it never decodes.
// A delivered frame carries one AAC access unit (KindCompressed) plus the
// AudioSpecificConfig on Format().Codec, so a consumer decodes an HLS AAC track
// exactly as it would an RTSP or httpsource AAC track. Frame.PTS is accumulated
// from the ADTS frame duration, so it is monotonic from zero; the container PES
// timestamp is not used. A segment skipped by EXT-X-GAP, or segments dropped when
// the client falls behind the live window, advance the clock by their declared
// duration and are reported as Frame.SeqGap on the next delivered frame.
//
// Scope. This source demuxes AAC in an MPEG-TS segment, the dominant
// audio-on-the-wire form for live HLS. It follows CDN redirects and supports
// media and master playlists, live and VOD. Out of scope: fMP4/CMAF segments
// (EXT-X-MAP), encrypted or DRM content (EXT-X-KEY), byte-range segments
// (EXT-X-BYTERANGE), video, non-AAC audio (MP3 or LATM in TS), adaptive bitrate
// switching, and Digest authentication. A playlist that requires any of these
// fails Open with ErrUnsupportedPlaylist or ErrUnsupportedCodec.
//
// The read-idle watchdog (Config.ReadIdle) answers "is new audio still
// arriving": it is stamped on every successful playlist or segment body read.
// For a live stream it must exceed the playlist target duration, since the
// client is intentionally idle between reloads. Playlist and segment bodies are
// bounded by Config.MaxPlaylistBytes and Config.MaxSegmentBytes so an untrusted
// endpoint cannot force an unbounded read.
package hlssource
