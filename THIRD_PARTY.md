# Third-party notices

go-audio-stream is an original implementation written from the public
protocol specifications:

- RFC 2326 (RTSP 1.0), RFC 3550 (RTP), RFC 3551 (RTP A/V profile),
  RFC 3640 (RTP payload for MPEG-4 streams), RFC 7587 (RTP payload for
  Opus), RFC 7616 (HTTP Digest), RFC 8866 (SDP).
- ITU-T Recommendation G.711 (mu-law and A-law companding) and ITU-T
  Recommendation G.726 (40, 32, 24, 16 kbit/s ADPCM), for the G.711 and
  G.726 decoders.

No code has been copied, adapted, or translated from any existing RTSP
implementation. MediaMTX and gortsplib (MIT) were consulted as behavioral
references for real-world interoperability quirks only; where observing
their behavior shaped a workaround, that is credited here as an idea, not
as code.

The G.711 and G.726 decoders are written from the ITU-T recommendations. The
Sun Microsystems CCITT g72x reference and FFmpeg's G.726 codec were consulted
as behavioral references to verify bit-exactness only; no code from either was
copied, adapted, or translated.
