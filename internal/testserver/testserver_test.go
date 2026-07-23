package testserver

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// methodOptions is the OPTIONS token, hoisted to a constant so the several
// scripted OPTIONS exchanges do not trip goconst.
const methodOptions = "OPTIONS"

// aacSDP is a single-track AAC-hbr SDP fixture. Handshake only counts the
// m= sections to know how many SETUP exchanges to script, so one m=audio
// line yields exactly one track.
const aacSDP = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=Stream\r\n" +
	"t=0 0\r\n" +
	"m=audio 0 RTP/AVP 96\r\n" +
	"a=rtpmap:96 mpeg4-generic/44100/2\r\n" +
	"a=fmtp:96 streamtype=5;profile-level-id=1;mode=AAC-hbr;sizelength=13;indexlength=3;indexdeltalength=3;config=1210\r\n" +
	"a=control:trackID=0\r\n"

// testClient is the raw client side of an exchange: a net.Conn plus an
// accumulation buffer parsed with the M4a wire helpers. It plays the role
// the real M4b client will later fill, so the server is exercised end to
// end before any client code exists.
type testClient struct {
	t    *testing.T
	conn net.Conn
	buf  []byte
	off  int
	cseq int
}

// dialPlain connects a raw TCP client to a non-TLS server.
func dialPlain(t *testing.T, s *Server, path string) *testClient {
	t.Helper()
	conn, err := net.Dial("tcp", hostPort(t, s.URL(path)))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &testClient{t: t, conn: conn}
}

// dialTLS connects a raw TLS client that verifies the server certificate
// against a RootCAs pool built from CertPEM.
func dialTLS(t *testing.T, s *Server, path string) *testClient {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(s.CertPEM()) {
		t.Fatal("AppendCertsFromPEM: no cert added")
	}
	conn, err := tls.Dial("tcp", hostPort(t, s.URL(path)), &tls.Config{
		RootCAs:    pool,
		ServerName: "127.0.0.1",
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &testClient{t: t, conn: conn}
}

// hostPort extracts the host:port authority from an rtsp:// or rtsps:// URL.
func hostPort(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url %q: %v", raw, err)
	}
	return u.Host
}

// fill compacts the consumed prefix and reads one chunk from the socket.
func (c *testClient) fill() error {
	if c.off > 0 {
		c.buf = c.buf[:copy(c.buf, c.buf[c.off:])]
		c.off = 0
	}
	tmp := make([]byte, 4096)
	n, err := c.conn.Read(tmp)
	if n > 0 {
		// Matches ServerConn.fill and the production reader: a short read that
		// carried a full unit must be parsed before the error surfaces on the
		// next call, or a (n>0, io.EOF) from a server that responds and closes
		// discards a response that is already buffered.
		c.buf = append(c.buf, tmp[:n]...)
		return nil
	}
	return err
}

// send marshals and writes a client request with a fresh client CSeq and
// returns that CSeq.
func (c *testClient) send(method, reqURL string, h rtsp.Header, body []byte) int {
	c.t.Helper()
	c.cseq++
	raw, err := rtsp.MarshalRequest(&rtsp.Request{
		Method: method,
		URL:    reqURL,
		CSeq:   c.cseq,
		Header: h,
		Body:   body,
	})
	if err != nil {
		c.t.Fatalf("marshal %s: %v", method, err)
	}
	if _, err := c.conn.Write(raw); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
	return c.cseq
}

// respond marshals and writes a response answering a server-initiated
// request, echoing req.CSeq. It is the client side of the round trip
// ServerConn.SendServerRequest plus ReadResponse exercises.
func (c *testClient) respond(req *rtsp.Request, code int, reason string) {
	c.t.Helper()
	raw, err := rtsp.MarshalResponse(&rtsp.Response{
		StatusCode: code,
		Reason:     reason,
		CSeq:       req.CSeq,
	})
	if err != nil {
		c.t.Fatalf("marshal response: %v", err)
	}
	if _, err := c.conn.Write(raw); err != nil {
		c.t.Fatalf("write response: %v", err)
	}
}

// readResponse reads and parses the next RTSP response.
func (c *testClient) readResponse() (*rtsp.Response, error) {
	for {
		avail := c.buf[c.off:]
		switch rtsp.ClassifyStream(avail) {
		case rtsp.FrameResponse:
			resp, n, err := rtsp.ParseResponse(avail)
			if errors.Is(err, rtsp.ErrIncomplete) {
				if e := c.fill(); e != nil {
					return nil, e
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			c.off += n
			return resp, nil
		case rtsp.FrameNeedMore:
			if e := c.fill(); e != nil {
				return nil, e
			}
		case rtsp.FrameInterleaved, rtsp.FrameRequest, rtsp.FrameUnknown:
			return nil, fmt.Errorf("expected response, got frame kind %d", rtsp.ClassifyStream(avail))
		default:
			return nil, fmt.Errorf("unexpected frame kind %d", rtsp.ClassifyStream(avail))
		}
	}
}

// readInterleaved reads the next interleaved frame, copying its payload.
func (c *testClient) readInterleaved() (rtsp.InterleavedFrame, error) {
	for {
		avail := c.buf[c.off:]
		switch rtsp.ClassifyStream(avail) {
		case rtsp.FrameInterleaved:
			f, n, err := rtsp.ParseInterleaved(avail)
			if errors.Is(err, rtsp.ErrIncomplete) {
				if e := c.fill(); e != nil {
					return rtsp.InterleavedFrame{}, e
				}
				continue
			}
			if err != nil {
				return rtsp.InterleavedFrame{}, err
			}
			payload := append([]byte(nil), f.Payload...)
			c.off += n
			return rtsp.InterleavedFrame{Channel: f.Channel, Payload: payload}, nil
		case rtsp.FrameNeedMore:
			if e := c.fill(); e != nil {
				return rtsp.InterleavedFrame{}, e
			}
		case rtsp.FrameResponse, rtsp.FrameRequest, rtsp.FrameUnknown:
			return rtsp.InterleavedFrame{}, fmt.Errorf("expected interleaved, got frame kind %d", rtsp.ClassifyStream(avail))
		default:
			return rtsp.InterleavedFrame{}, fmt.Errorf("unexpected frame kind %d", rtsp.ClassifyStream(avail))
		}
	}
}

// readServerRequest reads the next server-initiated request.
func (c *testClient) readServerRequest() (*rtsp.Request, error) {
	for {
		avail := c.buf[c.off:]
		switch rtsp.ClassifyStream(avail) {
		case rtsp.FrameRequest:
			req, n, err := rtsp.ParseRequest(avail)
			if errors.Is(err, rtsp.ErrIncomplete) {
				if e := c.fill(); e != nil {
					return nil, e
				}
				continue
			}
			if err != nil {
				return nil, err
			}
			c.off += n
			return req, nil
		case rtsp.FrameNeedMore:
			if e := c.fill(); e != nil {
				return nil, e
			}
		case rtsp.FrameResponse, rtsp.FrameInterleaved, rtsp.FrameUnknown:
			return nil, fmt.Errorf("expected request, got frame kind %d", rtsp.ClassifyStream(avail))
		default:
			return nil, fmt.Errorf("unexpected frame kind %d", rtsp.ClassifyStream(avail))
		}
	}
}

// clientHandshake drives the client side of the standard exchange against a
// server whose handler runs ServerConn.Handshake, and returns the channel
// pairs it read from the SETUP Transport responses.
func clientHandshake(t *testing.T, c *testClient, base string, nTracks int) []ChannelPair {
	t.Helper()
	optCSeq := c.send(methodOptions, base, nil, nil)
	resp, err := c.readResponse()
	if err != nil {
		t.Fatalf("read OPTIONS response: %v", err)
	}
	if resp.StatusCode != 200 || resp.CSeq != optCSeq {
		t.Fatalf("OPTIONS: got %d cseq %d, want 200 cseq %d", resp.StatusCode, resp.CSeq, optCSeq)
	}

	c.send("DESCRIBE", base, rtsp.Header{"Accept": {"application/sdp"}}, nil)
	resp, err = c.readResponse()
	if err != nil {
		t.Fatalf("read DESCRIBE response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("DESCRIBE: got %d, want 200", resp.StatusCode)
	}

	pairs := make([]ChannelPair, 0, nTracks)
	for i := 0; i < nTracks; i++ {
		h := rtsp.Header{}
		h.Set("Transport", rtsp.BuildTransport(2*i, 2*i+1))
		c.send("SETUP", base, h, nil)
		resp, err = c.readResponse()
		if err != nil {
			t.Fatalf("read SETUP response: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("SETUP %d: got %d, want 200", i, resp.StatusCode)
		}
		tr, err := rtsp.ParseTransport(resp.Header.Get("Transport"))
		if err != nil {
			t.Fatalf("parse SETUP %d transport: %v", i, err)
		}
		if !tr.Interleaved {
			t.Fatalf("SETUP %d: transport not interleaved: %q", i, resp.Header.Get("Transport"))
		}
		pairs = append(pairs, ChannelPair{RTP: tr.RTPChannel, RTCP: tr.RTCPChannel})
	}

	h := rtsp.Header{}
	h.Set("Range", "npt=0.000-")
	c.send("PLAY", base, h, nil)
	resp, err = c.readResponse()
	if err != nil {
		t.Fatalf("read PLAY response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("PLAY: got %d, want 200", resp.StatusCode)
	}
	return pairs
}

func TestServerRespondsToRequest(t *testing.T) {
	t.Parallel()
	s := New(t, Options{Handle: func(sc *ServerConn) {
		req, err := sc.ReadRequest()
		if err != nil {
			t.Errorf("ReadRequest: %v", err)
			return
		}
		if req.Method != methodOptions {
			t.Errorf("method: got %q, want OPTIONS", req.Method)
		}
		if err := sc.Respond(req, 200, "OK", nil, nil); err != nil {
			t.Errorf("Respond: %v", err)
		}
	}})

	c := dialPlain(t, s, "/stream")
	cseq := c.send(methodOptions, s.URL("/stream"), nil, nil)
	resp, err := c.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", resp.StatusCode)
	}
	if resp.CSeq != cseq {
		t.Errorf("cseq: got %d, want %d", resp.CSeq, cseq)
	}
}

func TestServerInjectFrame(t *testing.T) {
	t.Parallel()
	want := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	s := New(t, Options{Handle: func(sc *ServerConn) {
		if _, err := sc.Handshake(HandshakeConfig{SDP: aacSDP, SessionID: "sess-inject"}); err != nil {
			t.Errorf("Handshake: %v", err)
			return
		}
		if err := sc.InjectFrame(0, want); err != nil {
			t.Errorf("InjectFrame: %v", err)
		}
	}})

	c := dialPlain(t, s, "/stream")
	clientHandshake(t, c, s.URL("/stream"), 1)
	frame, err := c.readInterleaved()
	if err != nil {
		t.Fatalf("readInterleaved: %v", err)
	}
	if frame.Channel != 0 {
		t.Errorf("channel: got %d, want 0", frame.Channel)
	}
	if !bytes.Equal(frame.Payload, want) {
		t.Errorf("payload: got %x, want %x", frame.Payload, want)
	}
}

func TestServerSendServerRequest(t *testing.T) {
	t.Parallel()
	reqURL := "rtsp://127.0.0.1/stream"
	cseqCh := make(chan int, 1)
	s := New(t, Options{Handle: func(sc *ServerConn) {
		// Closed on every path: an unconditional receive below would otherwise
		// block to the 10-minute package timeout on the error return, hiding
		// the t.Errorf that explains what actually went wrong.
		defer close(cseqCh)
		cseq, err := sc.SendServerRequest("OPTIONS", reqURL, nil)
		if err != nil {
			t.Errorf("SendServerRequest: %v", err)
			return
		}
		cseqCh <- cseq
	}})

	c := dialPlain(t, s, "/stream")
	req, err := c.readServerRequest()
	if err != nil {
		t.Fatalf("readServerRequest: %v", err)
	}
	serverCSeq := <-cseqCh
	if req.Method != methodOptions {
		t.Errorf("method: got %q, want OPTIONS", req.Method)
	}
	if req.URL != reqURL {
		t.Errorf("url: got %q, want %q", req.URL, reqURL)
	}
	if req.CSeq != serverCSeq {
		t.Errorf("cseq: got %d, want %d", req.CSeq, serverCSeq)
	}
}

func TestServerSendServerRequestRoundTrip(t *testing.T) {
	t.Parallel()
	reqURL := "rtsp://127.0.0.1/stream"
	type result struct {
		cseq int
		resp *rtsp.Response
		err  error
	}
	resCh := make(chan result, 1)
	s := New(t, Options{Handle: func(sc *ServerConn) {
		cseq, err := sc.SendServerRequest("OPTIONS", reqURL, nil)
		if err != nil {
			resCh <- result{err: err}
			return
		}
		resp, err := sc.ReadResponse()
		resCh <- result{cseq: cseq, resp: resp, err: err}
	}})

	c := dialPlain(t, s, "/stream")
	req, err := c.readServerRequest()
	if err != nil {
		t.Fatalf("readServerRequest: %v", err)
	}
	if req.Method != methodOptions {
		t.Errorf("method: got %q, want OPTIONS", req.Method)
	}
	c.respond(req, 200, "OK")

	got := <-resCh
	if got.err != nil {
		t.Fatalf("server round trip: %v", got.err)
	}
	if got.resp == nil {
		t.Fatal("ReadResponse returned nil response")
	}
	if got.resp.StatusCode != 200 {
		t.Errorf("status: got %d, want 200", got.resp.StatusCode)
	}
	if got.resp.CSeq != got.cseq {
		t.Errorf("response cseq: got %d, want %d", got.resp.CSeq, got.cseq)
	}
}

func TestServerHandshakeAAC(t *testing.T) {
	t.Parallel()
	resultCh := make(chan []ChannelPair, 1)
	s := New(t, Options{Handle: func(sc *ServerConn) {
		// Closed on every path so the receive below cannot block forever when
		// Handshake returns an error.
		defer close(resultCh)
		pairs, err := sc.Handshake(HandshakeConfig{
			SDP:           aacSDP,
			SessionID:     "sess-aac",
			PublicMethods: []string{"OPTIONS", "DESCRIBE", "SETUP", "PLAY", "TEARDOWN"},
		})
		if err != nil {
			t.Errorf("Handshake: %v", err)
			return
		}
		resultCh <- pairs
	}})

	c := dialPlain(t, s, "/stream")
	clientPairs := clientHandshake(t, c, s.URL("/stream"), 1)
	serverPairs := <-resultCh

	want := []ChannelPair{{RTP: 0, RTCP: 1}}
	if len(clientPairs) != 1 || clientPairs[0] != want[0] {
		t.Errorf("client pairs: got %+v, want %+v", clientPairs, want)
	}
	if len(serverPairs) != 1 || serverPairs[0] != want[0] {
		t.Errorf("server pairs: got %+v, want %+v", serverPairs, want)
	}
}

func TestServerHandshakeRenumbered(t *testing.T) {
	t.Parallel()
	resultCh := make(chan []ChannelPair, 1)
	s := New(t, Options{Handle: func(sc *ServerConn) {
		// Closed on every path so the receive below cannot block forever when
		// Handshake returns an error.
		defer close(resultCh)
		pairs, err := sc.Handshake(HandshakeConfig{
			SDP:             aacSDP,
			SessionID:       "sess-renum",
			InterleavedBase: 4,
		})
		if err != nil {
			t.Errorf("Handshake: %v", err)
			return
		}
		resultCh <- pairs
	}})

	c := dialPlain(t, s, "/stream")
	clientPairs := clientHandshake(t, c, s.URL("/stream"), 1)
	serverPairs := <-resultCh

	want := ChannelPair{RTP: 4, RTCP: 5}
	if len(clientPairs) != 1 || clientPairs[0] != want {
		t.Errorf("client pairs: got %+v, want %+v", clientPairs, want)
	}
	if len(serverPairs) != 1 || serverPairs[0] != want {
		t.Errorf("server pairs: got %+v, want %+v", serverPairs, want)
	}
}

func TestServerTLS(t *testing.T) {
	t.Parallel()
	s := New(t, Options{TLS: true, Handle: func(sc *ServerConn) {
		req, err := sc.ReadRequest()
		if err != nil {
			t.Errorf("ReadRequest: %v", err)
			return
		}
		if err := sc.Respond(req, 200, "OK", nil, nil); err != nil {
			t.Errorf("Respond: %v", err)
		}
	}})

	if s.CertPEM() == nil {
		t.Fatal("CertPEM is nil for a TLS server")
	}
	u := s.URL("/stream")
	if !strings.HasPrefix(u, "rtsps://") {
		t.Errorf("TLS URL scheme: got %q, want rtsps://", u)
	}

	c := dialTLS(t, s, "/stream")
	cseq := c.send(methodOptions, u, nil, nil)
	resp, err := c.readResponse()
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != 200 || resp.CSeq != cseq {
		t.Errorf("got %d cseq %d, want 200 cseq %d", resp.StatusCode, resp.CSeq, cseq)
	}
}

func TestServerCloseAbrupt(t *testing.T) {
	t.Parallel()
	s := New(t, Options{Handle: func(sc *ServerConn) {
		if _, err := sc.ReadRequest(); err != nil {
			t.Errorf("ReadRequest: %v", err)
			return
		}
		if err := sc.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}})

	c := dialPlain(t, s, "/stream")
	c.send(methodOptions, s.URL("/stream"), nil, nil)
	if _, err := c.readResponse(); err == nil {
		t.Error("expected read error after abrupt Close, got nil")
	}
}

func TestServerNonTLSCertPEMNil(t *testing.T) {
	t.Parallel()
	s := New(t, Options{Handle: func(sc *ServerConn) { _ = sc.Close() }})
	if s.CertPEM() != nil {
		t.Error("CertPEM should be nil for a non-TLS server")
	}
	if got := s.URL("/x"); !strings.HasPrefix(got, "rtsp://") {
		t.Errorf("non-TLS URL scheme: got %q, want rtsp://", got)
	}
}
