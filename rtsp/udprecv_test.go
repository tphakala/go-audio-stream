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
}

// newRecvHarness builds a raw-delivery track wired to a real UDP socket pair,
// with a sender dialing the RTP socket. The track adopts the first payload type
// it sees (sdpPayloadType unknown), so every test packet is accepted.
func newRecvHarness(t *testing.T, opts harnessOpts) *recvHarness {
	t.Helper()
	m, err := openMediaSockets()
	if err != nil {
		t.Fatalf("openMediaSockets: %v", err)
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
	send, err := net.DialUDP("udp", nil, m.rtpConn.LocalAddr().(*net.UDPAddr))
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
