package rtsp

import (
	"encoding/binary"
	"errors"
	"net"
	"runtime"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// buildTestRTP marshals a minimal RTP packet: version 2, no padding, extension
// or CSRC, the given payload type, sequence, timestamp and SSRC, and payload.
// It is the wire-format counterpart the UDP receive path parses.
func buildTestRTP(pt uint8, seq uint16, ts, ssrc uint32, payload []byte) []byte {
	buf := make([]byte, rtp.HeaderSize+len(payload))
	buf[0] = 0x80 // version 2
	buf[1] = pt & 0x7f
	binary.BigEndian.PutUint16(buf[2:4], seq)
	binary.BigEndian.PutUint32(buf[4:8], ts)
	binary.BigEndian.PutUint32(buf[8:12], ssrc)
	copy(buf[rtp.HeaderSize:], payload)
	return buf
}

// recvHarness drives runRTPReceiver over a real loopback UDP socket without a
// full RTSP handshake: it feeds datagrams from a local sender and captures the
// delivered frames.
type recvHarness struct {
	t        *testing.T
	c        *Client
	tr       *track
	m        *mediaSockets
	send     *net.UDPConn
	remote   net.Conn
	frames   chan audiostream.Frame
	baseline int
}

type harnessOpts struct {
	readIdle time.Duration
	playing  bool
	// peerIP overrides the negotiated media peer IP the receiver filters on.
	// The zero value (nil) uses the loopback sender's IP, so datagrams are
	// accepted; a mismatching IP makes the loopback sender foreign, so its
	// datagrams are dropped by the source-address filter.
	peerIP net.IP
}

// loopbackUDP binds a UDP socket to an explicit 127.0.0.1 address, so the source
// IP of datagrams a loopback sender delivers is deterministically 127.0.0.1 (the
// wildcard bind openMediaSockets uses would leave the family ambiguous).
func loopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	return conn
}

// newRecvHarness builds a raw-delivery track wired to a real loopback UDP socket
// pair, with a sender dialing the RTP socket. The track adopts the first payload
// type it sees (sdpPayloadType unknown), so every test packet is accepted. The
// media peers are set to opts.peerIP (default 127.0.0.1, matching the sender) so
// the source-address filter admits the sender unless a test overrides it.
func newRecvHarness(t *testing.T, opts harnessOpts) *recvHarness {
	t.Helper()
	peerIP := opts.peerIP
	if peerIP == nil {
		peerIP = net.IPv4(127, 0, 0, 1)
	}
	rtpConn := loopbackUDP(t)
	rtcpConn := loopbackUDP(t)
	m := &mediaSockets{
		rtpConn:  rtpConn,
		rtcpConn: rtcpConn,
		rtpPeer:  &net.UDPAddr{IP: peerIP, Port: rtpConn.LocalAddr().(*net.UDPAddr).Port},
		rtcpPeer: &net.UDPAddr{IP: peerIP, Port: rtcpConn.LocalAddr().(*net.UDPAddr).Port},
	}
	local, remote := net.Pipe()
	frames := make(chan audiostream.Frame, 256)
	tr := &track{id: 0, kind: deliverRaw, clockRate: 8000, sdpPayloadType: payloadTypeUnknown}
	c := &Client{
		conn:    local,
		closing: make(chan struct{}),
		media:   map[int]*mediaSockets{tr.id: m},
	}
	c.cfg.ReadIdle = opts.readIdle
	c.cfg.OnFrame = func(f audiostream.Frame) {
		cp := f
		cp.Data = append([]byte(nil), f.Data...)
		select {
		case frames <- cp:
		default:
		}
	}
	if opts.playing {
		c.playing.Store(true)
	}
	send, err := net.DialUDP("udp", nil, rtpConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		_ = m.Close()
		t.Fatalf("dial sender: %v", err)
	}
	return &recvHarness{t: t, c: c, tr: tr, m: m, send: send, remote: remote, frames: frames}
}

// start records the goroutine baseline and launches the RTP receiver under the
// same udpWG discipline Play uses.
func (h *recvHarness) start() {
	h.baseline = runtime.NumGoroutine()
	h.c.udpWG.Add(1)
	go h.c.runRTPReceiver(h.tr, h.m)
}

// sendRTP writes one RTP datagram to the receiver's RTP socket.
func (h *recvHarness) sendRTP(pkt []byte) {
	h.t.Helper()
	if _, err := h.send.Write(pkt); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}

// waitFrame returns the next delivered frame or fails on timeout.
func (h *recvHarness) waitFrame() audiostream.Frame {
	h.t.Helper()
	select {
	case f := <-h.frames:
		return f
	case <-time.After(2 * time.Second):
		h.t.Fatal("timed out waiting for a delivered frame")
		return audiostream.Frame{}
	}
}

// cleanup closes the media sockets, joins the receiver, asserts no goroutine
// outlived it, and drops the sender/pipe.
func (h *recvHarness) cleanup() {
	h.t.Helper()
	h.c.closeMediaSockets()
	h.c.udpWG.Wait()
	assertGoroutinesSettled(h.t, h.baseline)
	_ = h.send.Close()
	_ = h.remote.Close()
}

// stop funnels a normal Close and then cleans up.
func (h *recvHarness) stop() {
	h.t.Helper()
	h.c.initiateShutdown(audiostream.ErrClosed)
	h.cleanup()
}

// assertGoroutinesSettled polls until the live goroutine count returns to
// baseline, dumping stacks on failure. It is the internal-package counterpart
// of assertNoGoroutineLeak, so callers must not be parallel.
func assertGoroutinesSettled(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Fatalf("goroutine leak: have %d, baseline %d\n%s", n, baseline, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// In-order datagrams deliver in order with no reported gap.
func TestUDPRecvInOrder(t *testing.T) {
	h := newRecvHarness(t, harnessOpts{})
	h.start()

	const ssrc = 0x11111111
	for i := uint16(0); i < 3; i++ {
		h.sendRTP(buildTestRTP(96, 100+i, uint32(100+i), ssrc, []byte{byte(i)}))
	}
	for i := 0; i < 3; i++ {
		f := h.waitFrame()
		if f.RTPTime != uint32(100+i) {
			t.Errorf("frame %d RTPTime = %d, want %d", i, f.RTPTime, 100+i)
		}
		if f.SeqGap != 0 {
			t.Errorf("frame %d SeqGap = %d, want 0", i, f.SeqGap)
		}
	}
	h.stop()
}

// Datagrams arriving out of order are resequenced before delivery, with no gap.
func TestUDPRecvReordered(t *testing.T) {
	h := newRecvHarness(t, harnessOpts{})
	h.start()

	const ssrc = 0x22222222
	// N is delivered first, then N+2 arrives before N+1: the Reorderer holds
	// N+2 until N+1 completes the run, then releases N+1, N+2 in order.
	h.sendRTP(buildTestRTP(96, 200, 200, ssrc, []byte{0}))
	if f := h.waitFrame(); f.RTPTime != 200 {
		t.Fatalf("first frame RTPTime = %d, want 200", f.RTPTime)
	}
	h.sendRTP(buildTestRTP(96, 202, 202, ssrc, []byte{2}))
	h.sendRTP(buildTestRTP(96, 201, 201, ssrc, []byte{1}))

	for _, want := range []uint32{201, 202} {
		f := h.waitFrame()
		if f.RTPTime != want {
			t.Errorf("resequenced frame RTPTime = %d, want %d", f.RTPTime, want)
		}
		if f.SeqGap != 0 {
			t.Errorf("resequenced frame RTPTime %d SeqGap = %d, want 0", f.RTPTime, f.SeqGap)
		}
	}
	h.stop()
}

// A packet more than the reorder window ahead force-releases the gap, and the
// buffered packet past the hole surfaces the real loss as SeqGap == 1.
func TestUDPRecvLoss(t *testing.T) {
	h := newRecvHarness(t, harnessOpts{})
	h.start()

	const ssrc = 0x33333333
	h.sendRTP(buildTestRTP(96, 300, 300, ssrc, []byte{0}))
	if f := h.waitFrame(); f.RTPTime != 300 {
		t.Fatalf("first frame RTPTime = %d, want 300", f.RTPTime)
	}
	// 301 never arrives. 302 is buffered, then a packet a full window ahead
	// forces the release point past the missing 301, releasing 302.
	h.sendRTP(buildTestRTP(96, 302, 302, ssrc, []byte{2}))
	h.sendRTP(buildTestRTP(96, 300+rtp.MaxReorderWindow+1, 999, ssrc, []byte{9}))

	f := h.waitFrame()
	if f.RTPTime != 302 {
		t.Fatalf("post-loss frame RTPTime = %d, want 302", f.RTPTime)
	}
	if f.SeqGap != 1 {
		t.Errorf("post-loss frame SeqGap = %d, want 1 (301 lost)", f.SeqGap)
	}
	h.stop()
}

// An SSRC change flushes and resets the Reorderer and re-baselines cleanly, and
// the change is counted exactly once.
func TestUDPRecvSSRCChange(t *testing.T) {
	h := newRecvHarness(t, harnessOpts{})
	h.start()

	const ssrcA = 0x44444444
	const ssrcB = 0x55555555
	h.sendRTP(buildTestRTP(96, 400, 400, ssrcA, []byte{0}))
	if f := h.waitFrame(); f.RTPTime != 400 {
		t.Fatalf("first frame RTPTime = %d, want 400", f.RTPTime)
	}
	h.sendRTP(buildTestRTP(96, 401, 401, ssrcA, []byte{1}))
	if f := h.waitFrame(); f.RTPTime != 401 {
		t.Fatalf("second frame RTPTime = %d, want 401", f.RTPTime)
	}
	// A new source with a fresh sequence space. The receiver must flush and
	// reset the Reorderer, and the new baseline delivers cleanly.
	h.sendRTP(buildTestRTP(96, 9000, 9000, ssrcB, []byte{7}))
	f := h.waitFrame()
	if f.RTPTime != 9000 {
		t.Errorf("post-SSRC frame RTPTime = %d, want 9000", f.RTPTime)
	}
	if f.SeqGap != 0 {
		t.Errorf("post-SSRC frame SeqGap = %d, want 0 (new baseline)", f.SeqGap)
	}
	if got := h.tr.ssrcResets.Load(); got != 1 {
		t.Errorf("ssrcResets = %d, want 1", got)
	}
	h.stop()
}

// With the watchdog armed and no datagrams arriving, the receiver funnels
// ErrReadTimeout and exits without leaking.
func TestUDPRecvWatchdogTimeout(t *testing.T) {
	h := newRecvHarness(t, harnessOpts{readIdle: 60 * time.Millisecond, playing: true})
	h.start()

	// No datagrams: the read-idle deadline must fire and funnel a timeout.
	h.c.udpWG.Wait()
	if err := h.c.termError(); !errors.Is(err, audiostream.ErrReadTimeout) {
		t.Errorf("termErr = %v, want ErrReadTimeout", err)
	}
	h.cleanup()
}

// A datagram from the negotiated peer is processed; one from any other source
// address is dropped before accounting or processing, so an off-path forgery
// cannot count as bandwidth, keep the watchdog alive, or disturb the pipeline.
func TestUDPRecvSourceAddressFilter(t *testing.T) {
	// Accepted: the peer IP defaults to the loopback sender's IP.
	accepted := newRecvHarness(t, harnessOpts{})
	accepted.start()
	accepted.sendRTP(buildTestRTP(96, 10, 10, 0xABCD, []byte{1}))
	if f := accepted.waitFrame(); f.RTPTime != 10 {
		t.Errorf("accepted-peer frame RTPTime = %d, want 10", f.RTPTime)
	}
	accepted.stop()

	// Dropped: a TEST-NET peer IP (RFC 5737) the loopback sender cannot match,
	// so its datagram is foreign and must leave no trace.
	dropped := newRecvHarness(t, harnessOpts{peerIP: net.IPv4(192, 0, 2, 1)})
	dropped.start()
	dropped.sendRTP(buildTestRTP(96, 20, 20, 0x1234, []byte{2}))
	select {
	case f := <-dropped.frames:
		t.Fatalf("delivered a frame from a foreign source: %+v", f)
	case <-time.After(150 * time.Millisecond):
	}
	if got := dropped.tr.wireBytes.Load(); got != 0 {
		t.Errorf("wireBytes = %d, want 0 (a foreign-source datagram is not bandwidth)", got)
	}
	if got := dropped.tr.ssrcResets.Load(); got != 0 {
		t.Errorf("ssrcResets = %d, want 0 (a foreign-source datagram must not disturb the pipeline)", got)
	}
	if got := dropped.tr.packets.Load(); got != 0 {
		t.Errorf("packets = %d, want 0", got)
	}
	if got := dropped.tr.malformed.Load(); got != 0 {
		t.Errorf("malformed = %d, want 0 (a foreign-source datagram is dropped before the parse)", got)
	}
	if got := dropped.c.lastFrameAt.Load(); got != 0 {
		t.Errorf("lastFrameAt = %d, want 0 (a foreign-source datagram must not feed the watchdog)", got)
	}
	dropped.stop()
}
