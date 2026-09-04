package rtsp

import (
	"encoding/binary"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Size caps for the RTSP message parsers. Every cap is a compile-time
// constant applied before allocation, so a hostile or broken peer cannot
// drive unbounded work or memory. Values are calibrated from the protocol
// research (section 5).
const (
	// MaxHeaderBytes bounds the status/request line plus all header lines
	// plus the terminating blank line that ParseResponse/ParseRequest will
	// scan before giving up. 64 KiB is far above any real RTSP head; a peer
	// that never terminates its header block is rejected, not buffered
	// unboundedly.
	MaxHeaderBytes = 64 * 1024
	// MaxHeaderLines is the largest number of header lines accepted
	// (research: 255).
	MaxHeaderLines = 255
	// MaxHeaderNameLen caps a header field name (research: ~512).
	MaxHeaderNameLen = 512
	// MaxHeaderValueLen caps a header field value (research: ~2048).
	MaxHeaderValueLen = 2048
	// MaxMethodLen caps a method token (research: 64).
	MaxMethodLen = 64
	// MaxVersionLen caps the RTSP-version token (research: 64).
	MaxVersionLen = 64
	// MaxRequestURILen caps a request-URI (research: 2048).
	MaxRequestURILen = 2048
	// MaxReasonLen caps a response reason phrase.
	MaxReasonLen = 512
	// MaxBodySize caps a message body declared by Content-Length
	// (research: 128 KiB).
	MaxBodySize = 128 * 1024
	// MaxInterleavedFrame is the largest interleaved payload the 2-byte
	// length field can express (research: inherently 65535).
	MaxInterleavedFrame = 65535
	// interleavedHeaderLen is the fixed interleaved framing header: '$', a
	// one-byte channel, and a two-byte big-endian payload length. It is the
	// per-frame wire overhead the payload byte count does not include.
	interleavedHeaderLen = 4
)

// Sentinel errors returned by the message parsers and serializers. A parser
// returns one of these (wrapped or bare) and never any other error value,
// and never panics on any input.
var (
	// ErrIncomplete means buf does not yet hold a whole message; read more.
	ErrIncomplete = errors.New("rtsp: incomplete message")
	// ErrHeadersTooLarge means the header block was not terminated within
	// MaxHeaderBytes.
	ErrHeadersTooLarge = errors.New("rtsp: header block exceeds maximum size")
	// ErrTooManyHeaders means the head carried more than MaxHeaderLines
	// header lines.
	ErrTooManyHeaders = errors.New("rtsp: too many header lines")
	// ErrHeaderNameTooLong means a header field name exceeded
	// MaxHeaderNameLen.
	ErrHeaderNameTooLong = errors.New("rtsp: header name too long")
	// ErrHeaderValueTooLong means a header field value exceeded
	// MaxHeaderValueLen.
	ErrHeaderValueTooLong = errors.New("rtsp: header value too long")
	// ErrBodyTooLarge means the Content-Length exceeded MaxBodySize.
	ErrBodyTooLarge = errors.New("rtsp: body exceeds maximum size")
	// ErrMalformedStatusLine means the response status line did not parse.
	ErrMalformedStatusLine = errors.New("rtsp: malformed status line")
	// ErrMalformedRequestLine means the request line did not parse.
	ErrMalformedRequestLine = errors.New("rtsp: malformed request line")
	// ErrMalformedHeader means a header line had no colon, or was an
	// obsolete folded continuation line.
	ErrMalformedHeader = errors.New("rtsp: malformed header line")
	// ErrBadContentLength means the Content-Length value was non-numeric or
	// negative.
	ErrBadContentLength = errors.New("rtsp: invalid Content-Length")
	// ErrNotInterleaved means ParseInterleaved was given bytes not starting
	// with '$'.
	ErrNotInterleaved = errors.New("rtsp: not an interleaved frame")
	// ErrInterleavedTooLarge means the channel or payload was out of range
	// for an interleaved frame.
	ErrInterleavedTooLarge = errors.New("rtsp: interleaved frame too large")
	// ErrInvalidRequest means a Request could not be serialized (empty or
	// oversize Method/URL, or oversize Body).
	ErrInvalidRequest = errors.New("rtsp: invalid request")
	// ErrInvalidResponse is returned by MarshalResponse when the status
	// code is out of range, a field carries a CR or LF, or the body
	// exceeds MaxBodySize.
	ErrInvalidResponse = errors.New("rtsp: invalid response")
)

// canonicalHeaderOverrides maps a lower-cased field name to the canonical
// spelling this client uses for the RTSP headers it cares about. Names not
// listed here fall back to hyphen-token title casing.
var canonicalHeaderOverrides = map[string]string{
	"cseq":             "CSeq",
	"content-length":   "Content-Length",
	"content-type":     "Content-Type",
	"content-base":     "Content-Base",
	"content-location": "Content-Location",
	"www-authenticate": "WWW-Authenticate",
	"authorization":    "Authorization",
	"session":          "Session",
	"transport":        "Transport",
	"public":           "Public",
	"rtp-info":         "RTP-Info",
	"location":         "Location",
	"range":            "Range",
	"user-agent":       "User-Agent",
	"server":           "Server",
	"date":             "Date",
}

// canonicalHeaderName maps a field name to a stable canonical spelling. It
// consults the override table for the RTSP headers this client uses, then
// falls back to hyphen-token title casing (each token capitalized, rest
// lower-cased) for anything else, so lookups are case-insensitive.
func canonicalHeaderName(name string) string {
	lower := strings.ToLower(name)
	if c, ok := canonicalHeaderOverrides[lower]; ok {
		return c
	}
	tokens := strings.Split(lower, "-")
	for i, tok := range tokens {
		if tok == "" {
			continue
		}
		tokens[i] = strings.ToUpper(tok[:1]) + tok[1:]
	}
	return strings.Join(tokens, "-")
}

// Header holds RTSP header fields keyed by canonical name. A field may
// repeat (for example multiple WWW-Authenticate lines); all values are
// kept in arrival order. Lookups are case-insensitive.
type Header map[string][]string

// Get returns the first value stored under name (matched case-insensitively),
// or "" when the field is absent.
func (h Header) Get(name string) string {
	vs := h[canonicalHeaderName(name)]
	if len(vs) == 0 {
		return ""
	}
	return vs[0]
}

// Values returns all values stored under name (matched case-insensitively)
// in order, or nil when the field is absent.
func (h Header) Values(name string) []string {
	return h[canonicalHeaderName(name)]
}

// Set replaces any existing values for name with the single value v.
func (h Header) Set(name, v string) {
	h[canonicalHeaderName(name)] = []string{v}
}

// Add appends v to the values stored under name.
func (h Header) Add(name, v string) {
	c := canonicalHeaderName(name)
	h[c] = append(h[c], v)
}

// Del removes every value stored under name. Deleting an absent field is a
// no-op. It completes the Get/Values/Set/Add set, and exists because a request
// that is retried needs a way to take a header back off rather than only to
// overwrite it: an Authorization computed for one attempt must not survive into
// the next when the recomputation fails.
func (h Header) Del(name string) {
	delete(h, canonicalHeaderName(name))
}

// Request is an outbound RTSP request (client to server) or a parsed
// server-initiated request.
type Request struct {
	// Method is the request method token, for example "DESCRIBE".
	Method string
	// URL is the request-URI as it appears on the request line.
	URL string
	// CSeq is the request sequence number. On marshal it is written as the
	// CSeq header; on parse it is read from the CSeq header (0 if absent or
	// unparseable). It is the caller's job to allocate CSeq values.
	CSeq int
	// Header holds additional fields. On marshal, any CSeq or Content-Length
	// entry here is ignored in favor of the computed values.
	Header Header
	// Body is the optional message body. Content-Length is computed from it.
	Body []byte
}

// Response is an outbound RTSP response (answering a server-initiated
// request) or a parsed response from the server.
type Response struct {
	// StatusCode is the numeric status (for example 200, 401).
	StatusCode int
	// Reason is the reason phrase.
	Reason string
	// CSeq is read from (parse) or written to (marshal) the CSeq header.
	CSeq int
	// Header holds all header fields. On parse it carries every field
	// verbatim, including the raw CSeq line.
	Header Header
	// Body is the message body, exactly Content-Length bytes on parse, nil
	// when there is none.
	Body []byte
}

// MarshalRequest serializes req to RTSP/1.0 wire format: the request line
// "<Method> <URL> RTSP/1.0", a CSeq header from req.CSeq, a Content-Length
// header when Body is non-empty, then req.Header fields in sorted order,
// the blank line, and the body. It returns ErrInvalidRequest when Method or
// URL is empty or exceeds MaxMethodLen/MaxRequestURILen, or when Body
// exceeds MaxBodySize. It never panics.
func MarshalRequest(req *Request) ([]byte, error) {
	if req == nil {
		return nil, ErrInvalidRequest
	}
	if req.Method == "" || req.URL == "" {
		return nil, ErrInvalidRequest
	}
	if len(req.Method) > MaxMethodLen || len(req.URL) > MaxRequestURILen {
		return nil, ErrInvalidRequest
	}
	if len(req.Body) > MaxBodySize {
		return nil, ErrInvalidRequest
	}
	// A CR or LF anywhere in the start line or the header fields would end
	// the line early on the wire and let everything after it be read as
	// further headers. The request-URI is held to a stricter rule still: a
	// raw space or tab truncates the request line at the server's tokenizer,
	// so a control URL carrying one would make the server act on a Request-URI
	// this client never resolved. These strings are not all locally authored:
	// a control URL comes from the session description, which is remote input,
	// so refuse them here rather than trusting every caller.
	if hasCRLF(req.Method) || hasForbiddenURIByte(req.URL) || headerHasCRLF(req.Header) {
		return nil, ErrInvalidRequest
	}

	var b strings.Builder
	b.WriteString(req.Method)
	b.WriteByte(' ')
	b.WriteString(req.URL)
	b.WriteString(" RTSP/1.0\r\n")
	writeControlHeaders(&b, req.CSeq, len(req.Body))
	writeSortedHeaders(&b, req.Header)
	b.WriteString("\r\n")

	out := make([]byte, 0, b.Len()+len(req.Body))
	out = append(out, b.String()...)
	out = append(out, req.Body...)
	return out, nil
}

// MarshalResponse serializes resp: the status line "RTSP/1.0 <code>
// <reason>", a CSeq header from resp.CSeq, a Content-Length header when Body
// is non-empty, then resp.Header fields, the blank line, and the body. Used
// to answer server-initiated requests. It returns ErrInvalidResponse when
// the status code is outside 100 to 999, when the reason or a header field
// carries a CR or LF, or when Body exceeds MaxBodySize. It never panics.
func MarshalResponse(resp *Response) ([]byte, error) {
	if resp == nil {
		return nil, ErrInvalidResponse
	}
	if resp.StatusCode < 100 || resp.StatusCode > 999 {
		return nil, ErrInvalidResponse
	}
	if len(resp.Reason) > MaxReasonLen || len(resp.Body) > MaxBodySize {
		return nil, ErrInvalidResponse
	}
	if hasCRLF(resp.Reason) || headerHasCRLF(resp.Header) {
		return nil, ErrInvalidResponse
	}

	var b strings.Builder
	b.WriteString("RTSP/1.0 ")
	b.WriteString(strconv.Itoa(resp.StatusCode))
	b.WriteByte(' ')
	b.WriteString(resp.Reason)
	b.WriteString("\r\n")
	writeControlHeaders(&b, resp.CSeq, len(resp.Body))
	writeSortedHeaders(&b, resp.Header)
	b.WriteString("\r\n")

	out := make([]byte, 0, b.Len()+len(resp.Body))
	out = append(out, b.String()...)
	out = append(out, resp.Body...)
	return out, nil
}

// hasCRLF reports whether s contains a carriage return or line feed, either
// of which would terminate a line early in the serialized message.
func hasCRLF(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

// hasForbiddenURIByte reports whether s contains a byte that must never appear
// raw in an RTSP request-URI: any control byte (0x00-0x1F, which includes CR,
// LF, and tab), a space (0x20), or DEL (0x7F). A raw space or tab truncates the
// request line at the server's tokenizer, so a control URL from the session
// description (remote input) carrying one would make the server act on a
// Request-URI this client never resolved; a control byte would split the line
// outright. RFC 3986 admits none of these unescaped, so a legitimate URI
// carries them percent-encoded and is unaffected.
func hasForbiddenURIByte(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// headerHasCRLF reports whether any field name or value in h contains a
// carriage return or line feed.
func headerHasCRLF(h Header) bool {
	for name, vals := range h {
		if hasCRLF(name) {
			return true
		}
		if slices.ContainsFunc(vals, hasCRLF) {
			return true
		}
	}
	return false
}

// writeControlHeaders writes the computed CSeq line and, when bodyLen is
// positive, the computed Content-Length line.
func writeControlHeaders(b *strings.Builder, cseq, bodyLen int) {
	b.WriteString("CSeq: ")
	b.WriteString(strconv.Itoa(cseq))
	b.WriteString("\r\n")
	if bodyLen > 0 {
		b.WriteString("Content-Length: ")
		b.WriteString(strconv.Itoa(bodyLen))
		b.WriteString("\r\n")
	}
}

// writeSortedHeaders writes every field in h except CSeq and Content-Length
// (which are computed), canonicalized and ordered by canonical name for a
// deterministic wire form. Repeated values are written on separate lines.
func writeSortedHeaders(b *strings.Builder, h Header) {
	if len(h) == 0 {
		return
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		c := canonicalHeaderName(k)
		if c == "CSeq" || c == "Content-Length" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return canonicalHeaderName(keys[i]) < canonicalHeaderName(keys[j])
	})
	for _, k := range keys {
		name := canonicalHeaderName(k)
		for _, v := range h[k] {
			b.WriteString(name)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
	}
}

// ParseResponse parses one complete RTSP response from the front of buf. It
// returns the response and n, the number of bytes the message occupies, so
// a caller draining a byte stream can advance by n. When buf does not yet
// contain a whole message it returns ErrIncomplete. It enforces every cap
// and returns a typed error on any violation. It never panics.
func ParseResponse(buf []byte) (resp *Response, n int, err error) {
	firstLine, header, bodyStart, err := parseHead(buf)
	if err != nil {
		return nil, 0, err
	}
	code, reason, err := parseStatusLine(firstLine)
	if err != nil {
		return nil, 0, err
	}
	body, n, err := extractBody(buf, header, bodyStart)
	if err != nil {
		return nil, 0, err
	}
	return &Response{
		StatusCode: code,
		Reason:     reason,
		CSeq:       parseCSeq(header),
		Header:     header,
		Body:       body,
	}, n, nil
}

// ParseRequest parses one complete server-initiated RTSP request from the
// front of buf, with the same n and ErrIncomplete semantics and the same
// caps as ParseResponse. It never panics.
func ParseRequest(buf []byte) (req *Request, n int, err error) {
	firstLine, header, bodyStart, err := parseHead(buf)
	if err != nil {
		return nil, 0, err
	}
	method, url, err := parseRequestLine(firstLine)
	if err != nil {
		return nil, 0, err
	}
	body, n, err := extractBody(buf, header, bodyStart)
	if err != nil {
		return nil, 0, err
	}
	return &Request{
		Method: method,
		URL:    url,
		CSeq:   parseCSeq(header),
		Header: header,
		Body:   body,
	}, n, nil
}

// parseHead locates and parses the message head shared by requests and
// responses: it returns the first line, the parsed header fields, and the
// index in buf where the body begins.
func parseHead(buf []byte) (firstLine string, header Header, bodyStart int, err error) {
	headEnd, bodyStart, err := findHead(buf)
	if err != nil {
		return "", nil, 0, err
	}
	lines := splitHeadLines(buf[:headEnd])
	if len(lines)-1 > MaxHeaderLines {
		return "", nil, 0, ErrTooManyHeaders
	}
	header = make(Header)
	if err := parseHeaderLines(lines[1:], header); err != nil {
		return "", nil, 0, err
	}
	return lines[0], header, bodyStart, nil
}

// findHead scans buf for the blank line that ends the head. It accepts both
// CRLFCRLF and a bare LFLF (some cheap firmware omits the CR). headEnd is
// the exclusive end of the header text (before the terminator); bodyStart
// is the index of the first body byte (after the terminator). It scans at
// most MaxHeaderBytes bytes: no terminator with buf under the cap is
// ErrIncomplete; at or over the cap is ErrHeadersTooLarge.
func findHead(buf []byte) (headEnd, bodyStart int, err error) {
	limit := min(len(buf), MaxHeaderBytes)
	for i := range limit {
		if buf[i] != '\n' {
			continue
		}
		if i >= 1 && buf[i-1] == '\n' {
			return i - 1, i + 1, nil // bare LF LF
		}
		if i >= 3 && buf[i-1] == '\r' && buf[i-2] == '\n' && buf[i-3] == '\r' {
			return i - 3, i + 1, nil // CRLF CRLF
		}
	}
	if len(buf) >= MaxHeaderBytes {
		return 0, 0, ErrHeadersTooLarge
	}
	return 0, 0, ErrIncomplete
}

// splitHeadLines splits head text on LF and trims one trailing CR from each
// line, so callers see logical lines regardless of CRLF or bare-LF framing.
func splitHeadLines(head []byte) []string {
	lines := strings.Split(string(head), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// parseStatusLine parses a response first line "RTSP/1.0 <3-digit code>
// <reason>". A malformed line is ErrMalformedStatusLine.
func parseStatusLine(line string) (code int, reason string, err error) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return 0, "", ErrMalformedStatusLine
	}
	if !strings.HasPrefix(parts[0], "RTSP/") || len(parts[0]) > MaxVersionLen {
		return 0, "", ErrMalformedStatusLine
	}
	if len(parts[1]) != 3 {
		return 0, "", ErrMalformedStatusLine
	}
	code, err = strconv.Atoi(parts[1])
	if err != nil || code < 100 || code > 999 {
		return 0, "", ErrMalformedStatusLine
	}
	if len(parts) == 3 {
		reason = parts[2]
	}
	if len(reason) > MaxReasonLen {
		return 0, "", ErrMalformedStatusLine
	}
	return code, reason, nil
}

// parseRequestLine parses a request first line "<method> <request-URI>
// RTSP/1.0". A malformed line is ErrMalformedRequestLine.
func parseRequestLine(line string) (method, url string, err error) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 3 {
		return "", "", ErrMalformedRequestLine
	}
	method, url = parts[0], parts[1]
	version := parts[2]
	if method == "" || len(method) > MaxMethodLen {
		return "", "", ErrMalformedRequestLine
	}
	if url == "" || len(url) > MaxRequestURILen {
		return "", "", ErrMalformedRequestLine
	}
	if !strings.HasPrefix(version, "RTSP/") || len(version) > MaxVersionLen {
		return "", "", ErrMalformedRequestLine
	}
	return method, url, nil
}

// parseHeaderLines parses each header line into h. It splits on the first
// colon, trims one optional leading space from the value, and enforces the
// name and value caps. A line with no colon, or an obsolete folded
// continuation line (one beginning with a space or tab), is ErrMalformedHeader.
func parseHeaderLines(lines []string, h Header) error {
	for _, line := range lines {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			return ErrMalformedHeader
		}
		before, after, ok := strings.Cut(line, ":")
		if !ok {
			return ErrMalformedHeader
		}
		name := before
		value := strings.TrimPrefix(after, " ")
		if len(name) > MaxHeaderNameLen {
			return ErrHeaderNameTooLong
		}
		if len(value) > MaxHeaderValueLen {
			return ErrHeaderValueTooLong
		}
		h.Add(name, value)
	}
	return nil
}

// parseCSeq reads the CSeq header as an int, returning 0 on absence or a
// parse failure (not an error, per the wire contract).
func parseCSeq(h Header) int {
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("CSeq")))
	if err != nil {
		return 0
	}
	return n
}

// extractBody reads Content-Length and returns the body plus n, the total
// message length. Absent Content-Length means no body (n ends at the head).
// A non-numeric or negative value is ErrBadContentLength; a value over
// MaxBodySize is ErrBodyTooLarge; a buffer shorter than head+length is
// ErrIncomplete. The returned body is copied, not aliased to buf.
func extractBody(buf []byte, h Header, bodyStart int) (body []byte, n int, err error) {
	cls := h.Values("Content-Length")
	if len(cls) == 0 {
		return nil, bodyStart, nil
	}
	// Two Content-Length fields that disagree leave the body length
	// ambiguous, and resolving it by picking one is how request smuggling
	// gets in: this parser and the next hop can choose differently. Repeats
	// that agree are harmless and tolerated, since some devices emit them.
	clStr := cls[0]
	for _, other := range cls[1:] {
		if strings.TrimSpace(other) != strings.TrimSpace(clStr) {
			return nil, 0, ErrBadContentLength
		}
	}
	cl, convErr := strconv.Atoi(strings.TrimSpace(clStr))
	if convErr != nil || cl < 0 {
		return nil, 0, ErrBadContentLength
	}
	if cl > MaxBodySize {
		return nil, 0, ErrBodyTooLarge
	}
	end := bodyStart + cl
	if end > len(buf) {
		return nil, 0, ErrIncomplete
	}
	if cl == 0 {
		return nil, bodyStart, nil
	}
	body = make([]byte, cl)
	copy(body, buf[bodyStart:end])
	return body, end, nil
}

// FrameKind classifies what begins at the front of an RTSP/1.0 TCP stream.
type FrameKind int

const (
	// FrameNeedMore means there are not yet enough bytes to decide.
	FrameNeedMore FrameKind = iota
	// FrameInterleaved means the bytes begin with '$' (0x24): a binary
	// interleaved frame (RFC 2326 section 10.12).
	FrameInterleaved
	// FrameResponse means the bytes begin with "RT" (the start of
	// "RTSP/1.0 ..."): an RTSP response.
	FrameResponse
	// FrameRequest means the bytes begin with a known RTSP method token: a
	// server-initiated request.
	FrameRequest
	// FrameUnknown means the leading bytes match nothing recognized; the
	// caller should discard one byte and reclassify (resynchronize).
	FrameUnknown
)

// ClassifyStream inspects the leading bytes of buf without consuming them
// and reports what begins there. A leading '$' is decisive from one byte;
// the text cases need two bytes. It never panics.
func ClassifyStream(buf []byte) FrameKind {
	if len(buf) == 0 {
		return FrameNeedMore
	}
	if buf[0] == '$' {
		return FrameInterleaved
	}
	if len(buf) < 2 {
		return FrameNeedMore
	}
	switch string(buf[:2]) {
	case "RT":
		return FrameResponse
	// Two-byte prefixes of the RTSP methods a server may send: OPTIONS,
	// GET_PARAMETER, TEARDOWN, ANNOUNCE, SETUP/SET_PARAMETER,
	// RECORD/REDIRECT, PLAY, PAUSE, DESCRIBE.
	case "OP", "GE", "TE", "AN", "SE", "RE", "PL", "PA", "DE":
		return FrameRequest
	default:
		return FrameUnknown
	}
}

// InterleavedFrame is a parsed TCP-interleaved binary frame.
type InterleavedFrame struct {
	// Channel is the interleaved channel number (0..255).
	Channel int
	// Payload aliases buf; copy it to retain beyond the call.
	Payload []byte
}

// ParseInterleaved parses one interleaved frame from buf, which must begin
// with '$'. It returns the frame and n = 4 + payload length. It returns
// ErrNotInterleaved when buf does not begin with '$', and ErrIncomplete
// when buf does not yet hold the whole frame. It never panics.
func ParseInterleaved(buf []byte) (InterleavedFrame, int, error) {
	if len(buf) == 0 || buf[0] != '$' {
		return InterleavedFrame{}, 0, ErrNotInterleaved
	}
	if len(buf) < interleavedHeaderLen {
		return InterleavedFrame{}, 0, ErrIncomplete
	}
	channel := int(buf[1])
	length := int(binary.BigEndian.Uint16(buf[2:4]))
	end := interleavedHeaderLen + length
	if len(buf) < end {
		return InterleavedFrame{}, 0, ErrIncomplete
	}
	return InterleavedFrame{Channel: channel, Payload: buf[interleavedHeaderLen:end]}, end, nil
}

// MarshalInterleaved builds an interleaved frame: '$', a one-byte channel,
// a two-byte big-endian length, then payload. It returns
// ErrInterleavedTooLarge when channel is outside 0..255 or len(payload)
// exceeds MaxInterleavedFrame. It never panics.
func MarshalInterleaved(channel int, payload []byte) ([]byte, error) {
	if channel < 0 || channel > 255 || len(payload) > MaxInterleavedFrame {
		return nil, ErrInterleavedTooLarge
	}
	out := make([]byte, interleavedHeaderLen+len(payload))
	out[0] = '$'
	out[1] = byte(channel)
	binary.BigEndian.PutUint16(out[2:4], uint16(len(payload)))
	copy(out[interleavedHeaderLen:], payload)
	return out, nil
}
