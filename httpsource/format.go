package httpsource

import (
	"fmt"
	"math"
	"mime"
	"net/http"
	"strconv"
)

// maxChannels bounds the channel count this source accepts, for both WAV and
// raw audio. It caps the sample-frame width (2*channels bytes) well under the
// reader buffer, so a delivery is always a whole number of frames and the
// per-read byte-swap scratch is large enough. Eight channels covers the audio
// this library ingests.
const maxChannels = 8

// resolveFormat dispatches on the response Content-Type and fills the reader's
// immutable format fields: for a PCM source the rate, channels, frameBytes,
// swap, and data budget; for a compressed source (MP3 or AAC) the codec and the
// frame scanner. It runs during Open, before the reader goroutine spawns, so those
// fields need no synchronization afterward.
//
// Precedence for rate and channels is WAV header > Content-Type parameters >
// Config.Format; for byte order it is an explicit Config.Format.Endian > the
// raw default, which is little-endian for all raw PCM this source carries. An
// unresolvable rate or channel count is an open-phase error, never a guess.
func (c *Client) resolveFormat(resp *http.Response) error {
	mediaType, params, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mediaType {
	case "audio/wav", "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return c.setupWAV()
	case "audio/l16":
		return c.setupL16(params)
	case "audio/mpeg", "audio/mp3":
		// A raw MP3 (MPEG-1/2/2.5 Layer I/II/III) byte stream, as Icecast and
		// SHOUTcast radio endpoints and progressive MP3 responses serve it. The
		// reader frames it into coded frames; it is delivered compressed, never
		// decoded.
		return c.setupMP3()
	case "audio/aac", "audio/aacp":
		// A raw ADTS AAC byte stream, as Icecast and SHOUTcast AAC endpoints and
		// progressive .aac responses serve it. The reader frames it into raw
		// access units and synthesizes the AudioSpecificConfig from the ADTS
		// header, so a consumer decodes it exactly like an RTSP AAC track; it is
		// delivered compressed, never decoded. audio/aacp (HE-AAC) is framed the
		// same way: its base-layer ADTS is AAC-LC with implicit SBR a decoder
		// detects from the bitstream.
		return c.setupAAC()
	case "application/octet-stream", "audio/pcm", "":
		// Empty covers both an absent Content-Type and one mime.ParseMediaType
		// could not parse (it returns "" and an error, ignored here): sniff the
		// body for a RIFF/WAVE header, else fall back to Config.Format. An
		// unlabeled body is never sniffed as MP3: raw PCM can contain a byte
		// pair that mimics a frame sync, so MP3 requires an explicit media type.
		return c.setupSniff()
	default:
		// A named type this source does not carry (audio/ogg, audio/flac, ...).
		// Failing fast here is the no-transcode contract: this source delivers
		// PCM or a framed compressed bitstream it recognizes, never an
		// unrecognized codec's bytes mislabeled as something it is not.
		return fmt.Errorf("%w: %s", ErrUnsupportedFormat, mediaType)
	}
}

// setupWAV parses the RIFF header from the buffered body and adopts its rate and
// channels. WAV samples are little-endian by definition, so no byte swap.
func (c *Client) setupWAV() error {
	info, err := c.parseWAVHeader(c.br)
	if err != nil {
		return err
	}
	c.rate = info.rate
	c.channels = info.channels
	c.frameBytes = 2 * info.channels
	c.swap = false
	c.bounded = info.bounded
	c.remaining = int64(info.dataSize)
	return nil
}

// setupL16 configures a raw audio/L16 source. RFC 3551 defines audio/L16 as
// big-endian, but real HTTP embedded microphones (for example
// esp32-audio-streamer's /stream.pcm) send native little-endian while labeling
// the stream audio/L16, so this source defaults audio/L16 to little-endian; set
// Config.Format.Endian = EndianBig for a spec-strict big-endian source. The rate
// and channels MIME parameters win over Config.Format; a missing channel count
// defaults to one (the audio/L16 registration default), a missing rate is
// unresolvable and fails Open.
func (c *Client) setupL16(params map[string]string) error {
	rate := paramInt(params, "rate", c.cfg.Format.SampleRate)
	channels := paramInt(params, "channels", c.cfg.Format.Channels)
	if channels <= 0 {
		channels = 1
	}
	return c.setupRaw(rate, channels)
}

// setupSniff handles an unlabeled body. It peeks the leading signature; a Peek
// error (a body shorter than the signature) is treated as "not WAV" and falls
// through to Config.Format rather than failing Open, so a short raw stream
// still opens. A RIFF/WAVE body is parsed as WAV, and a 64-bit RIFF body
// (RF64/BW64/WAVE) is rejected as unsupported here rather than slipping through
// to the raw fallback, which would otherwise deliver container bytes as PCM.
// Absent any WAVE signature the shape comes entirely from Config.Format, and
// unlabeled embedded PCM is native little-endian.
func (c *Client) setupSniff() error {
	head, err := c.br.Peek(len(riffMagic) + 4 + len(waveMagic))
	// A stall that tripped the open deadline, a caller cancellation, or a
	// transport failure during the sniff read fails Open through the open-phase
	// taxonomy rather than being swallowed into a spurious raw-PCM success that
	// then dies on the first body read (issue #92). A clean short read (a
	// genuinely short unlabeled stream) classifies as nil and falls through to
	// the Config.Format raw fallback, the pre-#92 behavior.
	if oe := c.classifyOpenRead(err); oe != nil {
		return oe
	}
	if err == nil {
		if isRIFFWAVE(head) {
			return c.setupWAV()
		}
		if magic, is64 := is64BitRIFFWAVE(head); is64 {
			return fmt.Errorf("%w: %s (64-bit RIFF) is not supported", ErrUnsupportedFormat, magic)
		}
	}
	return c.setupRaw(c.cfg.Format.SampleRate, c.cfg.Format.Channels)
}

// setupRaw validates a raw shape and records it, resolving the byte order from
// the explicit Config override or the little-endian default that every raw PCM
// stream this source carries now takes. A raw source has no self-described
// length, so it streams unbounded until EOF, Close, or the watchdog.
func (c *Client) setupRaw(rate, channels int) error {
	if err := validateRawShape(rate, channels); err != nil {
		return err
	}
	c.rate = rate
	c.channels = channels
	c.frameBytes = 2 * channels
	c.swap = resolveBigEndian(c.cfg.Format.Endian)
	c.bounded = false
	return nil
}

// validateRawShape rejects a rate or channel count outside the supported range.
// The rate is checked against the 32-bit ceiling that a real audio clock stays
// under, and the channel count against maxChannels; either failure is
// ErrFormatUnknown, since it leaves the sample geometry undetermined.
func validateRawShape(rate, channels int) error {
	if rate < 1 || int64(rate) > math.MaxUint32 {
		return fmt.Errorf("%w: sample rate %d out of range", ErrFormatUnknown, rate)
	}
	if channels < 1 || channels > maxChannels {
		return fmt.Errorf("%w: channel count %d out of range", ErrFormatUnknown, channels)
	}
	return nil
}

// resolveBigEndian reports whether the source samples are big-endian (and so
// need a swap to little-endian s16le on delivery). Only an explicit EndianBig
// override is big-endian; EndianLittle and the EndianUnspecified default are
// little-endian, so raw PCM defaults to being delivered verbatim.
func resolveBigEndian(override Endianness) bool {
	return override == EndianBig
}

// paramInt returns a positive integer MIME parameter, or fallback when the
// parameter is absent, non-numeric, or not positive.
func paramInt(params map[string]string, key string, fallback int) int {
	if v, ok := params[key]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// isRIFFWAVE reports whether head begins with a RIFF container whose form type
// is WAVE. head must be at least len(riffMagic)+4+len(waveMagic) bytes; callers
// pass exactly that.
func isRIFFWAVE(head []byte) bool {
	return string(head[0:4]) == riffMagic && string(head[8:12]) == waveMagic
}

// is64BitRIFFWAVE reports whether head begins with a 64-bit RIFF WAVE container
// (RF64 or BW64), returning the magic for the error message. These carry PCM
// this source cannot size or bound, so a sniffed body must reject them rather
// than fall through to the raw fallback. head must be at least
// len(riffMagic)+4+len(waveMagic) bytes.
func is64BitRIFFWAVE(head []byte) (string, bool) {
	magic := string(head[0:4])
	if (magic == rf64Magic || magic == bw64Magic) && string(head[8:12]) == waveMagic {
		return magic, true
	}
	return "", false
}
