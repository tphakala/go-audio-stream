package rtsp_test

import (
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// responseSeeds are wire fixtures exercising the response parser's paths:
// minimal, body, repeated fields, bare-LF framing, and trailing bytes.
var responseSeeds = [][]byte{
	[]byte(""),
	[]byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\n"),
	[]byte("RTSP/1.0 200 OK\r\nContent-Type: application/sdp\r\nContent-Length: 5\r\n\r\nv=0\r\n"),
	[]byte("RTSP/1.0 401 Unauthorized\r\nCSeq: 2\r\nWWW-Authenticate: Basic realm=\"a\"\r\nWWW-Authenticate: Digest realm=\"b\"\r\n\r\n"),
	[]byte("RTSP/1.0 200 OK\ncseq: 7\n\n"),
	[]byte("RTSP/1.0 200 OK\r\nCSeq: 1\r\n\r\nEXTRA"),
	[]byte("RTSP/1.0 200 OK\r\nContent-Length: abc\r\n\r\n"),
}

// requestSeeds are wire fixtures exercising the request parser's paths.
var requestSeeds = [][]byte{
	[]byte(""),
	[]byte("OPTIONS rtsp://cam/s RTSP/1.0\r\nCSeq: 3\r\n\r\n"),
	[]byte("ANNOUNCE rtsp://cam/s RTSP/1.0\r\nCSeq: 4\r\nContent-Length: 5\r\n\r\nv=0\r\n"),
	[]byte("GET_PARAMETER rtsp://cam/s RTSP/1.0\r\nCSeq: 5\r\nSession: 12345\r\n\r\n"),
}

func FuzzParseResponse(f *testing.F) {
	for _, s := range responseSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, buf []byte) {
		resp, n, err := rtsp.ParseResponse(buf)
		if err != nil {
			return // a typed error is a pass; the contract is no panic
		}
		if n < 0 || n > len(buf) {
			t.Fatalf("ParseResponse n = %d out of range for %d-byte buf", n, len(buf))
		}
		_ = resp.Header.Get("CSeq")
	})
}

func FuzzParseRequest(f *testing.F) {
	for _, s := range requestSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, buf []byte) {
		req, n, err := rtsp.ParseRequest(buf)
		if err != nil {
			return
		}
		if n < 0 || n > len(buf) {
			t.Fatalf("ParseRequest n = %d out of range for %d-byte buf", n, len(buf))
		}
		_ = req.Header.Get("CSeq")
	})
}

func FuzzParseInterleaved(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte{'$', 0x00, 0x00, 0x02, 0xAA, 0xBB})
	f.Add([]byte{'$', 0x01, 0xFF, 0xFF})
	f.Add([]byte("RTSP/1.0 200 OK\r\n"))
	f.Add([]byte(strings.Repeat("$", 8)))
	f.Fuzz(func(t *testing.T, buf []byte) {
		fr, n, err := rtsp.ParseInterleaved(buf)
		if err != nil {
			return
		}
		if n < 0 || n > len(buf) {
			t.Fatalf("ParseInterleaved n = %d out of range for %d-byte buf", n, len(buf))
		}
		if len(fr.Payload) != n-4 {
			t.Fatalf("payload len %d inconsistent with n %d", len(fr.Payload), n)
		}
	})
}
