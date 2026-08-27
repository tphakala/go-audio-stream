// Package hlssource pulls audio off an HLS (HTTP Live Streaming) endpoint and
// delivers it as timestamped AAC access units, satisfying the same
// audiostream.Source contract as the rtsp, httpsource and udpsource clients.
//
// Open fetches an m3u8 playlist, resolves a master (multivariant) playlist to a
// media playlist, downloads segments in media-sequence order, demuxes the AAC
// access units out of the segments (from an MPEG-TS elementary stream, or from
// fMP4/CMAF fragments announced by EXT-X-MAP), and delivers each access unit
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
// from each access unit's duration (the ADTS frame duration for MPEG-TS, the
// timescale-derived fMP4 sample duration for CMAF), so it is monotonic from zero;
// the container timestamp is not used (the MPEG-TS PES timestamp and the fMP4 tfdt
// are both ignored). A segment skipped by EXT-X-GAP, or segments dropped when
// the client falls behind the live window, advance the clock by their declared
// duration and are reported as Frame.SeqGap on the next delivered frame.
//
// Scope. This source demuxes AAC in an MPEG-TS segment and AAC in an fMP4/CMAF
// segment (an EXT-X-MAP initialization segment plus .m4s fragments), the two
// dominant audio-on-the-wire forms for HLS. It follows CDN redirects and supports
// media and master playlists, live and VOD. The fMP4 path selects the audio track
// by its 'soun' handler and mp4a sample entry, so a multiplexed audio+video
// fragment feeds only the audio samples. Out of scope: encrypted or DRM content
// (EXT-X-KEY, or an encrypted fMP4 sample entry), byte-range segments and
// byte-range EXT-X-MAP (EXT-X-BYTERANGE), an EXT-X-MAP that changes mid-stream,
// video, non-AAC audio (MP3 or LATM in TS, a non-AAC fMP4 sample entry), adaptive
// bitrate switching, and Digest authentication. An unsupported container or
// encrypted content fails Open with ErrUnsupportedPlaylist; a segment whose audio
// is not AAC fails with ErrUnsupportedCodec; a segment carrying no audio at all
// (video-only) fails with ErrMalformedSegment.
//
// The read-idle watchdog (Config.ReadIdle) answers "is new audio still
// arriving": it is stamped on every successful playlist or segment body read.
// For a live stream it must exceed the playlist target duration, since the
// client is intentionally idle between reloads. Playlist and segment bodies are
// bounded by Config.MaxPlaylistBytes and Config.MaxSegmentBytes so an untrusted
// endpoint cannot force an unbounded read.
package hlssource
