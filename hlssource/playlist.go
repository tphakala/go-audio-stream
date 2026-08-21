package hlssource

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// mediaSegment is one entry in a media playlist: a resolved absolute URI, its
// declared duration, its absolute media sequence number, and the boundary flags
// the reload state machine and the demuxer key off.
type mediaSegment struct {
	// seq is the absolute media sequence number: EXT-X-MEDIA-SEQUENCE plus the
	// segment's index in the playlist. It identifies a segment across reloads so
	// an already-delivered one is not fetched twice.
	seq uint64
	// uri is the segment's absolute URL, resolved against the playlist URL.
	uri string
	// duration is the EXTINF duration, used to advance the media clock across a
	// skipped (gap) or missed segment.
	duration time.Duration
	// discontinuity is true when EXT-X-DISCONTINUITY preceded this segment: the
	// demuxer resets its continuity domain before this segment.
	discontinuity bool
	// gap is true when EXT-X-GAP marked this segment: it is intentionally absent
	// and must not be fetched. The media clock advances by duration and the loss
	// is signalled to the consumer.
	gap bool
}

// mediaPlaylist is a parsed media playlist (a list of segments).
type mediaPlaylist struct {
	version               int
	targetDuration        time.Duration
	mediaSequence         uint64
	discontinuitySequence uint64
	endList               bool
	segments              []mediaSegment
}

// variant is one EXT-X-STREAM-INF entry in a master playlist.
type variant struct {
	uri        string
	bandwidth  int
	audioGroup string // AUDIO="group" attribute; "" when the audio is muxed in the variant
}

// rendition is one EXT-X-MEDIA TYPE=AUDIO entry in a master playlist. A rendition
// with an empty uri means the audio is muxed into the variant that references the
// group, not served separately.
type rendition struct {
	uri       string
	groupID   string
	isDefault bool
}

// masterPlaylist is a parsed master (multivariant) playlist.
type masterPlaylist struct {
	variants []variant
	audio    []rendition
}

// parsePlaylist parses an m3u8 body into either a media or a master playlist, with
// all URIs resolved to absolute form against base. Exactly one of the returned
// pointers is non-nil on success. It returns ErrMalformedPlaylist for a body that
// is not a valid playlist and ErrUnsupportedPlaylist for a valid playlist this
// source will not play (encryption, fMP4 init, byte range).
func parsePlaylist(body []byte, base *url.URL) (*mediaPlaylist, *masterPlaylist, error) {
	lines := splitLines(string(body))
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "#EXTM3U" {
		return nil, nil, fmt.Errorf("%w: missing #EXTM3U header", ErrMalformedPlaylist)
	}
	p := playlistParser{base: base}
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if err := p.handleURI(line); err != nil {
				return nil, nil, err
			}
			continue
		}
		tag, attr, _ := strings.Cut(line, ":")
		if err := p.handleTag(tag, attr); err != nil {
			return nil, nil, err
		}
	}
	return p.result()
}

// playlistParser accumulates parse state across the lines of one playlist. It
// splits the tag dispatch out of parsePlaylist so neither function carries the
// whole grammar's branching.
type playlistParser struct {
	base     *url.URL
	media    mediaPlaylist
	master   masterPlaylist
	isMedia  bool
	isMaster bool
	haveTgt  bool

	// Pending state carried onto the next segment or variant URI line.
	pendExtinf  bool
	pendDur     time.Duration
	pendDisc    bool
	pendGap     bool
	pendVariant *variant
}

// handleURI resolves a URI line against the pending variant (master) or the
// pending EXTINF (media). A URI with neither is malformed.
func (p *playlistParser) handleURI(line string) error {
	switch {
	case p.pendVariant != nil:
		p.pendVariant.uri = resolveURI(p.base, line)
		p.master.variants = append(p.master.variants, *p.pendVariant)
		p.pendVariant = nil
		p.isMaster = true
	case p.pendExtinf:
		p.media.segments = append(p.media.segments, mediaSegment{
			seq:           p.media.mediaSequence + uint64(len(p.media.segments)),
			uri:           resolveURI(p.base, line),
			duration:      p.pendDur,
			discontinuity: p.pendDisc,
			gap:           p.pendGap,
		})
		p.pendExtinf, p.pendDisc, p.pendGap = false, false, false
	default:
		return fmt.Errorf("%w: URI %q with no preceding EXTINF or STREAM-INF", ErrMalformedPlaylist, line)
	}
	return nil
}

// handleTag applies one #EXT tag to the parser state. Unknown tags are ignored.
func (p *playlistParser) handleTag(tag, attr string) error {
	switch tag {
	case "#EXT-X-VERSION":
		p.media.version, _ = strconv.Atoi(strings.TrimSpace(attr))
	case "#EXT-X-TARGETDURATION":
		p.isMedia = true
		secs, err := strconv.Atoi(strings.TrimSpace(attr))
		if err != nil || secs <= 0 || secs > maxPlaylistSeconds {
			return fmt.Errorf("%w: bad EXT-X-TARGETDURATION %q", ErrMalformedPlaylist, attr)
		}
		p.media.targetDuration = time.Duration(secs) * time.Second
		p.haveTgt = true
	case "#EXT-X-MEDIA-SEQUENCE":
		p.media.mediaSequence, _ = strconv.ParseUint(strings.TrimSpace(attr), 10, 64)
	case "#EXT-X-DISCONTINUITY-SEQUENCE":
		p.media.discontinuitySequence, _ = strconv.ParseUint(strings.TrimSpace(attr), 10, 64)
	case "#EXT-X-ENDLIST":
		p.isMedia = true
		p.media.endList = true
	case "#EXTINF":
		p.isMedia = true
		dur, err := parseExtinf(attr)
		if err != nil {
			return err
		}
		p.pendExtinf, p.pendDur = true, dur
	case "#EXT-X-DISCONTINUITY":
		p.pendDisc = true
	case "#EXT-X-GAP":
		p.pendGap = true
	case "#EXT-X-KEY":
		// Encryption is out of scope; only an explicit METHOD=NONE is allowed.
		if m := attrValue(attr, "METHOD"); !strings.EqualFold(m, "NONE") {
			return fmt.Errorf("%w: encrypted content (EXT-X-KEY METHOD=%s)", ErrUnsupportedPlaylist, m)
		}
	case "#EXT-X-MAP":
		return fmt.Errorf("%w: EXT-X-MAP (fMP4 initialization) is not supported", ErrUnsupportedPlaylist)
	case "#EXT-X-BYTERANGE":
		return fmt.Errorf("%w: EXT-X-BYTERANGE segments are not supported", ErrUnsupportedPlaylist)
	case "#EXT-X-STREAM-INF":
		p.isMaster = true
		bw, _ := strconv.Atoi(attrValue(attr, "BANDWIDTH"))
		p.pendVariant = &variant{bandwidth: bw, audioGroup: attrValue(attr, "AUDIO")}
	case "#EXT-X-MEDIA":
		p.isMaster = true
		if strings.EqualFold(attrValue(attr, "TYPE"), "AUDIO") {
			uri := attrValue(attr, "URI")
			if uri != "" {
				uri = resolveURI(p.base, uri)
			}
			p.master.audio = append(p.master.audio, rendition{
				uri:       uri,
				groupID:   attrValue(attr, "GROUP-ID"),
				isDefault: strings.EqualFold(attrValue(attr, "DEFAULT"), "YES"),
			})
		}
	default:
		// Unknown or unhandled tags (EXT-X-PROGRAM-DATE-TIME,
		// EXT-X-INDEPENDENT-SEGMENTS, plain comments) are ignored per RFC 8216.
	}
	return nil
}

// result decides whether the parsed lines form a media or a master playlist, or
// neither, applying the media-playlist target-duration requirement.
func (p *playlistParser) result() (*mediaPlaylist, *masterPlaylist, error) {
	if p.isMaster && p.isMedia {
		return nil, nil, fmt.Errorf("%w: body mixes master and media playlist tags", ErrMalformedPlaylist)
	}
	if p.isMaster {
		return nil, &p.master, nil
	}
	if p.isMedia {
		if !p.haveTgt {
			return nil, nil, fmt.Errorf("%w: media playlist has no EXT-X-TARGETDURATION", ErrMalformedPlaylist)
		}
		return &p.media, nil, nil
	}
	return nil, nil, fmt.Errorf("%w: body is neither a media nor a master playlist", ErrMalformedPlaylist)
}

// selectMediaURL picks the media-playlist URL to open from a master playlist. It
// chooses the lowest-bandwidth variant, then resolves its audio: when that
// variant references an AUDIO group whose renditions carry their own URI, the
// group's default (or first) rendition URI is used, since the variant itself may
// be video-only; otherwise the variant URL is used (the audio is muxed in). A
// master with no variant and no audio rendition URL has nothing to play.
func (m *masterPlaylist) selectMediaURL() (string, error) {
	if len(m.variants) == 0 {
		if r, ok := pickAudio(m.audio, ""); ok && r.uri != "" {
			return r.uri, nil
		}
		return "", fmt.Errorf("%w: master playlist has no audio-bearing variant", ErrUnsupportedPlaylist)
	}
	lowest := m.variants[0]
	for _, v := range m.variants[1:] {
		if v.bandwidth < lowest.bandwidth {
			lowest = v
		}
	}
	if lowest.audioGroup != "" {
		if r, ok := pickAudio(m.audio, lowest.audioGroup); ok && r.uri != "" {
			return r.uri, nil
		}
	}
	if lowest.uri == "" {
		return "", fmt.Errorf("%w: selected variant has no URI", ErrUnsupportedPlaylist)
	}
	return lowest.uri, nil
}

// pickAudio returns the default (or first) audio rendition in group, or, when
// group is empty, across all groups. ok is false when there is no candidate.
func pickAudio(renditions []rendition, group string) (rendition, bool) {
	var first *rendition
	for i := range renditions {
		r := &renditions[i]
		if group != "" && r.groupID != group {
			continue
		}
		if r.isDefault {
			return *r, true
		}
		if first == nil {
			first = r
		}
	}
	if first != nil {
		return *first, true
	}
	return rendition{}, false
}

// maxPlaylistSeconds bounds a parsed EXTINF or target duration. A real segment
// or target duration is seconds to tens of seconds; a value past a day is a
// malformed or hostile playlist, and rejecting it keeps the remote number from
// overflowing the int64 nanosecond range when scaled to a time.Duration.
const maxPlaylistSeconds = 24 * 60 * 60

// parseExtinf parses an EXTINF attribute ("9.9,title" or "10") into a duration.
func parseExtinf(attr string) (time.Duration, error) {
	field, _, _ := strings.Cut(attr, ",")
	secs, err := strconv.ParseFloat(strings.TrimSpace(field), 64)
	if err != nil || secs < 0 || secs > maxPlaylistSeconds {
		return 0, fmt.Errorf("%w: bad EXTINF duration %q", ErrMalformedPlaylist, field)
	}
	return time.Duration(secs * float64(time.Second)), nil
}

// splitLines splits an m3u8 body into lines, tolerating both LF and CRLF and a
// leading UTF-8 BOM.
func splitLines(s string) []string {
	s = strings.TrimPrefix(s, "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}

// resolveURI resolves a possibly-relative playlist/segment URI against base,
// returning an absolute URL string. It uses url.ResolveReference, which handles
// relative paths, "../" traversal, and absolute URIs safely. An unparseable URI
// is returned unchanged, to be surfaced later by the fetch.
func resolveURI(base *url.URL, ref string) string {
	u, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	if base == nil {
		return u.String()
	}
	return base.ResolveReference(u).String()
}

// attrValue returns the value of key in an attribute list, unquoting a quoted
// value. Keys are matched case-insensitively; a missing key yields "".
func attrValue(attrs, key string) string {
	for _, kv := range splitAttrs(attrs) {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), key) {
			return strings.Trim(strings.TrimSpace(v), "\"")
		}
	}
	return ""
}

// splitAttrs splits a comma-separated attribute list, honoring double quotes so a
// comma inside a quoted value (a CODECS list, a NAME) does not split the entry.
func splitAttrs(attrs string) []string {
	var out []string
	var b strings.Builder
	inQuote := false
	for _, r := range attrs {
		switch {
		case r == '"':
			inQuote = !inQuote
			b.WriteRune(r)
		case r == ',' && !inQuote:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
