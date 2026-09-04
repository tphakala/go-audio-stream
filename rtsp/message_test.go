package rtsp_test

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

const (
	testURL        = "rtsp://cam/s"
	methodDescribe = "DESCRIBE"
)

// --- Header ---------------------------------------------------------------

func TestHeaderGetValuesCaseInsensitive(t *testing.T) {
	t.Parallel()
	h := rtsp.Header{}
	h.Add("CSeq", "1")
	h.Add("www-authenticate", "Basic realm=\"a\"")
	h.Add("WWW-Authenticate", "Digest realm=\"b\"")

	if got := h.Get("cseq"); got != "1" {
		t.Errorf("Get(cseq) = %q, want %q", got, "1")
	}
	vals := h.Values("Www-Authenticate")
	if len(vals) != 2 || vals[0] != "Basic realm=\"a\"" || vals[1] != "Digest realm=\"b\"" {
		t.Errorf("Values(Www-Authenticate) = %#v, want two challenges in order", vals)
	}
	if got := h.Get("absent"); got != "" {
		t.Errorf("Get(absent) = %q, want empty", got)
	}
	if got := h.Values("absent"); got != nil {
		t.Errorf("Values(absent) = %#v, want nil", got)
	}
}

func TestHeaderSetReplaces(t *testing.T) {
	t.Parallel()
	h := rtsp.Header{}
	h.Add("Session", "a")
	h.Add("Session", "b")
	h.Set("session", "c")
	if got := h.Values("Session"); len(got) != 1 || got[0] != "c" {
		t.Errorf("after Set, Values(Session) = %#v, want [c]", got)
	}
}

func TestHeaderCanonicalFallbackTitleCasing(t *testing.T) {
	t.Parallel()
	h := rtsp.Header{}
	h.Add("x-custom-header", "v")
	if _, ok := h["X-Custom-Header"]; !ok {
		t.Errorf("Add did not canonicalize unknown name to title case; keys = %v", keysOf(h))
	}
}

func keysOf(h rtsp.Header) []string {
	ks := make([]string, 0, len(h))
	for k := range h {
		ks = append(ks, k)
	}
	return ks
}

// --- ParseResponse --------------------------------------------------------

func TestParseResponseMinimal(t *testing.T) {
	t.Parallel()
	buf := []byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\n")
	resp, n, err := rtsp.ParseResponse(buf)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.Reason != "OK" || resp.CSeq != 1 {
		t.Errorf("got code=%d reason=%q cseq=%d", resp.StatusCode, resp.Reason, resp.CSeq)
	}
	if resp.Body != nil {
		t.Errorf("Body = %#v, want nil", resp.Body)
	}
	if n != len(buf) {
		t.Errorf("n = %d, want %d", n, len(buf))
	}
}

func TestParseResponseWithSDPBody(t *testing.T) {
	t.Parallel()
	body := "v=0\r\n"
	buf := []byte("RTSP/1.0 200 OK\r\nContent-Type: application/sdp\r\nContent-Length: 5\r\n\r\n" + body)
	resp, n, err := rtsp.ParseResponse(buf)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if !bytes.Equal(resp.Body, []byte(body)) {
		t.Errorf("Body = %q, want %q", resp.Body, body)
	}
	if n != len(buf) {
		t.Errorf("n = %d, want %d", n, len(buf))
	}
	if got := resp.Header.Get("Content-Type"); got != "application/sdp" {
		t.Errorf("Content-Type = %q, want application/sdp", got)
	}
	// One byte short must report ErrIncomplete.
	if _, _, err := rtsp.ParseResponse(buf[:len(buf)-1]); !errors.Is(err, rtsp.ErrIncomplete) {
		t.Errorf("short buffer err = %v, want ErrIncomplete", err)
	}
}

func TestParseResponseTwoWWWAuthenticate(t *testing.T) {
	t.Parallel()
	buf := []byte("RTSP/1.0 401 Unauthorized\r\nCSeq: 2\r\n" +
		"WWW-Authenticate: Basic realm=\"cam\"\r\n" +
		"WWW-Authenticate: Digest realm=\"cam\", nonce=\"abc\"\r\n\r\n")
	resp, _, err := rtsp.ParseResponse(buf)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	vals := resp.Header.Values("WWW-Authenticate")
	if len(vals) != 2 {
		t.Fatalf("got %d WWW-Authenticate values, want 2", len(vals))
	}
	if !strings.HasPrefix(vals[0], "Basic") || !strings.HasPrefix(vals[1], "Digest") {
		t.Errorf("challenges out of order: %#v", vals)
	}
}

func TestParseResponseCaseInsensitiveCSeq(t *testing.T) {
	t.Parallel()
	buf := []byte("RTSP/1.0 200 OK\r\ncseq: 7\r\n\r\n")
	resp, _, err := rtsp.ParseResponse(buf)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if resp.CSeq != 7 {
		t.Errorf("CSeq = %d, want 7", resp.CSeq)
	}
}

func TestParseResponseBareLF(t *testing.T) {
	t.Parallel()
	buf := []byte("RTSP/1.0 200 OK\nCSeq: 1\n\n")
	resp, n, err := rtsp.ParseResponse(buf)
	if err != nil {
		t.Fatalf("ParseResponse bare-LF: %v", err)
	}
	if resp.StatusCode != http.StatusOK || resp.CSeq != 1 {
		t.Errorf("got code=%d cseq=%d", resp.StatusCode, resp.CSeq)
	}
	if n != len(buf) {
		t.Errorf("n = %d, want %d", n, len(buf))
	}
}

func TestParseResponseTrailingBytes(t *testing.T) {
	t.Parallel()
	msg := "RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\n"
	buf := []byte(msg + "EXTRA")
	_, n, err := rtsp.ParseResponse(buf)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if n != len(msg) {
		t.Errorf("n = %d, want %d", n, len(msg))
	}
	if got := string(buf[n:]); got != "EXTRA" {
		t.Errorf("remainder = %q, want EXTRA", got)
	}
}

func TestParseResponseCapViolations(t *testing.T) {
	t.Parallel()

	var manyLines strings.Builder
	manyLines.WriteString("RTSP/1.0 200 OK\r\n")
	for range 300 {
		manyLines.WriteString("X-Pad: v\r\n")
	}
	manyLines.WriteString("\r\n")

	tests := []struct {
		name string
		buf  []byte
		want error
	}{
		{
			name: "too many headers",
			buf:  []byte(manyLines.String()),
			want: rtsp.ErrTooManyHeaders,
		},
		{
			name: "body too large",
			buf:  []byte("RTSP/1.0 200 OK\r\nContent-Length: 999999\r\n\r\n"),
			want: rtsp.ErrBodyTooLarge,
		},
		{
			name: "header value too long",
			buf:  []byte("RTSP/1.0 200 OK\r\nX-Big: " + strings.Repeat("a", 3000) + "\r\n\r\n"),
			want: rtsp.ErrHeaderValueTooLong,
		},
		{
			name: "header name too long",
			buf:  []byte("RTSP/1.0 200 OK\r\n" + strings.Repeat("N", 600) + ": v\r\n\r\n"),
			want: rtsp.ErrHeaderNameTooLong,
		},
		{
			name: "unterminated head too large",
			buf:  []byte("RTSP/1.0 200 OK\r\n" + strings.Repeat("X", 70*1024)),
			want: rtsp.ErrHeadersTooLarge,
		},
		{
			name: "bad content length",
			buf:  []byte("RTSP/1.0 200 OK\r\nContent-Length: abc\r\n\r\n"),
			want: rtsp.ErrBadContentLength,
		},
		{
			name: "negative content length",
			buf:  []byte("RTSP/1.0 200 OK\r\nContent-Length: -1\r\n\r\n"),
			want: rtsp.ErrBadContentLength,
		},
		{
			name: "malformed status line",
			buf:  []byte("GARBAGE LINE\r\n\r\n"),
			want: rtsp.ErrMalformedStatusLine,
		},
		{
			name: "malformed header no colon",
			buf:  []byte("RTSP/1.0 200 OK\r\nNoColonHere\r\n\r\n"),
			want: rtsp.ErrMalformedHeader,
		},
		{
			name: "obsolete line folding rejected",
			buf:  []byte("RTSP/1.0 200 OK\r\nX-A: one\r\n continued\r\n\r\n"),
			want: rtsp.ErrMalformedHeader,
		},
		{
			name: "empty header name rejected",
			buf:  []byte("RTSP/1.0 200 OK\r\n: value\r\n\r\n"),
			want: rtsp.ErrMalformedHeader,
		},
		{
			name: "header name with space before colon rejected",
			buf:  []byte("RTSP/1.0 200 OK\r\nContent-Length : 5\r\n\r\n"),
			want: rtsp.ErrMalformedHeader,
		},
		{
			name: "header name with control byte rejected",
			buf:  []byte("RTSP/1.0 200 OK\r\n\x01X: v\r\n\r\n"),
			want: rtsp.ErrMalformedHeader,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := rtsp.ParseResponse(tt.buf)
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParseResponseIncompleteHead(t *testing.T) {
	t.Parallel()
	// No terminating blank line, and buffer well under the cap.
	buf := []byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\n")
	if _, _, err := rtsp.ParseResponse(buf); !errors.Is(err, rtsp.ErrIncomplete) {
		t.Errorf("err = %v, want ErrIncomplete", err)
	}
}

// --- ParseRequest ---------------------------------------------------------

func TestParseRequestOptions(t *testing.T) {
	t.Parallel()
	buf := []byte("OPTIONS rtsp://cam/s RTSP/1.0\r\nCSeq: 3\r\n\r\n")
	req, n, err := rtsp.ParseRequest(buf)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.Method != http.MethodOptions || req.URL != testURL || req.CSeq != 3 {
		t.Errorf("got method=%q url=%q cseq=%d", req.Method, req.URL, req.CSeq)
	}
	if n != len(buf) {
		t.Errorf("n = %d, want %d", n, len(buf))
	}
}

func TestParseRequestMalformedLine(t *testing.T) {
	t.Parallel()
	buf := []byte("OPTIONS rtsp://cam/s\r\nCSeq: 3\r\n\r\n") // missing version token
	if _, _, err := rtsp.ParseRequest(buf); !errors.Is(err, rtsp.ErrMalformedRequestLine) {
		t.Errorf("err = %v, want ErrMalformedRequestLine", err)
	}
}

func TestParseRequestInvalidHeaderName(t *testing.T) {
	t.Parallel()
	// The header-name validation lives in parseHeaderLines, which both
	// ParseRequest and ParseResponse funnel through; assert the request path
	// rejects both the empty-name and the non-token (space before colon) cases.
	tests := []struct {
		name string
		buf  []byte
	}{
		{
			name: "empty name",
			buf:  []byte("OPTIONS rtsp://cam/s RTSP/1.0\r\n: value\r\n\r\n"),
		},
		{
			name: "space before colon",
			buf:  []byte("OPTIONS rtsp://cam/s RTSP/1.0\r\nContent-Length : 5\r\n\r\n"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := rtsp.ParseRequest(tt.buf); !errors.Is(err, rtsp.ErrMalformedHeader) {
				t.Errorf("err = %v, want ErrMalformedHeader", err)
			}
		})
	}
}

// --- MarshalRequest / MarshalResponse -------------------------------------

func TestMarshalRequestRoundTrip(t *testing.T) {
	t.Parallel()
	req := &rtsp.Request{Method: methodDescribe, URL: testURL, CSeq: 2}
	out, err := rtsp.MarshalRequest(req)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	if !strings.Contains(string(out), "CSeq: 2\r\n") {
		t.Errorf("marshaled request missing CSeq: 2 header:\n%s", out)
	}
	if strings.Contains(string(out), "Content-Length") {
		t.Errorf("marshaled request should have no Content-Length:\n%s", out)
	}
	got, n, err := rtsp.ParseRequest(out)
	if err != nil {
		t.Fatalf("ParseRequest(marshaled): %v", err)
	}
	if got.Method != methodDescribe || got.URL != testURL || got.CSeq != 2 {
		t.Errorf("round-trip got method=%q url=%q cseq=%d", got.Method, got.URL, got.CSeq)
	}
	if n != len(out) {
		t.Errorf("n = %d, want %d", n, len(out))
	}
}

func TestMarshalRequestWithBody(t *testing.T) {
	t.Parallel()
	req := &rtsp.Request{Method: "ANNOUNCE", URL: testURL, CSeq: 4, Body: []byte("v=0\r\n")}
	out, err := rtsp.MarshalRequest(req)
	if err != nil {
		t.Fatalf("MarshalRequest: %v", err)
	}
	if !strings.Contains(string(out), "Content-Length: 5\r\n") {
		t.Errorf("marshaled request missing Content-Length: 5:\n%s", out)
	}
	got, _, err := rtsp.ParseRequest(out)
	if err != nil {
		t.Fatalf("ParseRequest(marshaled): %v", err)
	}
	if !bytes.Equal(got.Body, []byte("v=0\r\n")) {
		t.Errorf("round-trip body = %q, want v=0", got.Body)
	}
}

func TestMarshalRequestInvalid(t *testing.T) {
	t.Parallel()
	if _, err := rtsp.MarshalRequest(&rtsp.Request{Method: "", URL: testURL}); !errors.Is(err, rtsp.ErrInvalidRequest) {
		t.Errorf("empty method err = %v, want ErrInvalidRequest", err)
	}
	if _, err := rtsp.MarshalRequest(&rtsp.Request{Method: methodDescribe, URL: ""}); !errors.Is(err, rtsp.ErrInvalidRequest) {
		t.Errorf("empty url err = %v, want ErrInvalidRequest", err)
	}
}

func TestMarshalResponse(t *testing.T) {
	t.Parallel()
	out, err := rtsp.MarshalResponse(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: 5})
	if err != nil {
		t.Fatalf("MarshalResponse: %v", err)
	}
	want := "RTSP/1.0 200 OK\r\nCSeq: 5\r\n\r\n"
	if string(out) != want {
		t.Errorf("MarshalResponse = %q, want %q", out, want)
	}
}

// --- ClassifyStream -------------------------------------------------------

func TestClassifyStream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want rtsp.FrameKind
	}{
		{"$", rtsp.FrameInterleaved},
		{"RT", rtsp.FrameResponse},
		{"OP", rtsp.FrameRequest},
		{"GE", rtsp.FrameRequest},
		{"TE", rtsp.FrameRequest},
		{"AN", rtsp.FrameRequest},
		{"SE", rtsp.FrameRequest},
		{"RE", rtsp.FrameRequest},
		{"PL", rtsp.FrameRequest},
		{"PA", rtsp.FrameRequest},
		{"DE", rtsp.FrameRequest},
		{"ZZ", rtsp.FrameUnknown},
		{"", rtsp.FrameNeedMore},
		{"O", rtsp.FrameNeedMore},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			if got := rtsp.ClassifyStream([]byte(tt.in)); got != tt.want {
				t.Errorf("ClassifyStream(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// --- Interleaved ----------------------------------------------------------

func TestParseInterleaved(t *testing.T) {
	t.Parallel()
	buf := []byte{'$', 0x00, 0x00, 0x02, 0xAA, 0xBB}
	fr, n, err := rtsp.ParseInterleaved(buf)
	if err != nil {
		t.Fatalf("ParseInterleaved: %v", err)
	}
	if fr.Channel != 0 || !bytes.Equal(fr.Payload, []byte{0xAA, 0xBB}) || n != 6 {
		t.Errorf("got channel=%d payload=%v n=%d", fr.Channel, fr.Payload, n)
	}

	if _, _, err := rtsp.ParseInterleaved(buf[:5]); !errors.Is(err, rtsp.ErrIncomplete) {
		t.Errorf("truncated err = %v, want ErrIncomplete", err)
	}
	if _, _, err := rtsp.ParseInterleaved([]byte{'R', 0x00}); !errors.Is(err, rtsp.ErrNotInterleaved) {
		t.Errorf("non-'$' err = %v, want ErrNotInterleaved", err)
	}
}

func TestMarshalInterleaved(t *testing.T) {
	t.Parallel()
	out, err := rtsp.MarshalInterleaved(0, []byte{0xAA, 0xBB})
	if err != nil {
		t.Fatalf("MarshalInterleaved: %v", err)
	}
	fr, n, err := rtsp.ParseInterleaved(out)
	if err != nil {
		t.Fatalf("ParseInterleaved(marshaled): %v", err)
	}
	if fr.Channel != 0 || !bytes.Equal(fr.Payload, []byte{0xAA, 0xBB}) || n != len(out) {
		t.Errorf("round-trip got channel=%d payload=%v n=%d", fr.Channel, fr.Payload, n)
	}

	if _, err := rtsp.MarshalInterleaved(300, []byte{0x00}); !errors.Is(err, rtsp.ErrInterleavedTooLarge) {
		t.Errorf("channel 300 err = %v, want ErrInterleavedTooLarge", err)
	}
	if _, err := rtsp.MarshalInterleaved(0, make([]byte, 70000)); !errors.Is(err, rtsp.ErrInterleavedTooLarge) {
		t.Errorf("oversize payload err = %v, want ErrInterleavedTooLarge", err)
	}
}

func TestMarshalRequestRejectsCRLFInjection(t *testing.T) {
	t.Parallel()
	// A CR or LF in the start line or a header field would end the line
	// early on the wire, letting whatever follows be read as more headers.
	// Not every one of these strings is locally authored: a control URL
	// comes from the session description, which is remote input.
	const (
		method = "SETUP"
		url    = "rtsp://h/s"
	)
	cases := map[string]rtsp.Request{
		"lf in url":            {Method: method, URL: url + "\nX-Evil: 1"},
		"cr in url":            {Method: method, URL: url + "\rX-Evil: 1"},
		"crlf in url":          {Method: method, URL: url + "\r\nX-Evil: 1"},
		"crlf in method":       {Method: method + "\r\nX-Evil: 1", URL: url},
		"crlf in header name":  {Method: method, URL: url, Header: rtsp.Header{"X\r\nEvil": {"1"}}},
		"crlf in header value": {Method: method, URL: url, Header: rtsp.Header{"Session": {"12345\r\nX-Evil: 1"}}},
	}
	for name, req := range cases {
		if _, err := rtsp.MarshalRequest(&req); !errors.Is(err, rtsp.ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	if _, err := rtsp.MarshalRequest(nil); !errors.Is(err, rtsp.ErrInvalidRequest) {
		t.Errorf("nil request: err = %v, want ErrInvalidRequest", err)
	}
}

func TestMarshalRequestRejectsWhitespaceInURI(t *testing.T) {
	t.Parallel()
	// A raw space or tab in the request-URI truncates the request line at the
	// server's tokenizer, so a control URL from the session description (remote
	// input) that carried one would make the server act on a Request-URI this
	// client never resolved. RFC 3986 admits no space, tab, control byte, or
	// DEL unescaped, so a legitimate URI carries them percent-encoded.
	const method = "SETUP"
	cases := map[string]string{
		"space in uri":   "rtsp://cam/stream/trk 1",
		"tab in uri":     "rtsp://cam/stream/trk\t1",
		"trailing space": "rtsp://cam/stream ",
		"nul in uri":     "rtsp://cam/stream\x00",
		"del in uri":     "rtsp://cam/stream\x7f",
	}
	for name, uri := range cases {
		if _, err := rtsp.MarshalRequest(&rtsp.Request{Method: method, URL: uri}); !errors.Is(err, rtsp.ErrInvalidRequest) {
			t.Errorf("%s (%q): err = %v, want ErrInvalidRequest", name, uri, err)
		}
	}
	// Bytes just outside the forbidden set stay legal: a percent-encoded space,
	// "~" (0x7E, immediately below DEL), and a raw high byte (0x80, non-ASCII)
	// none truncate a request line, so a URI carrying them must still marshal.
	for _, uri := range []string{"rtsp://cam/stream/trk%201", "rtsp://cam/str~am", "rtsp://cam/stream/\x80"} {
		if _, err := rtsp.MarshalRequest(&rtsp.Request{Method: method, URL: uri}); err != nil {
			t.Errorf("legal uri %q: err = %v, want nil", uri, err)
		}
	}
}

func TestMarshalResponseValidates(t *testing.T) {
	t.Parallel()
	cases := map[string]*rtsp.Response{
		"nil":                  nil,
		"code too low":         {StatusCode: 99, Reason: "OK"},
		"code too high":        {StatusCode: 1000, Reason: "OK"},
		"crlf in reason":       {StatusCode: 200, Reason: "OK\r\nX-Evil: 1"},
		"crlf in header value": {StatusCode: 200, Reason: "OK", Header: rtsp.Header{"Session": {"a\r\nX-Evil: 1"}}},
		"reason too long":      {StatusCode: 200, Reason: strings.Repeat("x", rtsp.MaxReasonLen+1)},
		"body too large":       {StatusCode: 200, Reason: "OK", Body: make([]byte, rtsp.MaxBodySize+1)},
	}
	for name, resp := range cases {
		if _, err := rtsp.MarshalResponse(resp); !errors.Is(err, rtsp.ErrInvalidResponse) {
			t.Errorf("%s: err = %v, want ErrInvalidResponse", name, err)
		}
	}
	// The ordinary case the client actually sends must still marshal.
	if _, err := rtsp.MarshalResponse(&rtsp.Response{StatusCode: 200, Reason: "OK", CSeq: 4}); err != nil {
		t.Errorf("valid response: err = %v, want nil", err)
	}
}

func TestParseResponseDuplicateContentLength(t *testing.T) {
	t.Parallel()
	// Two Content-Length fields that disagree leave the body boundary
	// ambiguous, which is how smuggling gets in when two parsers resolve it
	// differently. Repeats that agree are harmless and stay accepted.
	conflicting := []byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: 4\r\nContent-Length: 8\r\n\r\nabcdefgh")
	if _, _, err := rtsp.ParseResponse(conflicting); !errors.Is(err, rtsp.ErrBadContentLength) {
		t.Errorf("conflicting Content-Length: err = %v, want ErrBadContentLength", err)
	}

	agreeing := []byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\nContent-Length: 4\r\nContent-Length: 4\r\n\r\nabcd")
	resp, _, err := rtsp.ParseResponse(agreeing)
	if err != nil {
		t.Fatalf("agreeing duplicate Content-Length: err = %v, want nil", err)
	}
	if string(resp.Body) != "abcd" {
		t.Errorf("Body = %q, want %q", resp.Body, "abcd")
	}
}
