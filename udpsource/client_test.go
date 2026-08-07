package udpsource

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/g711"
)

// loopbackAddr is the wildcard-port loopback bind used across the source tests.
const loopbackAddr = "127.0.0.1:0"

// --- shared test helpers ---------------------------------------------------

// rtpPacket builds a minimal RTP packet (version 2, no padding/extension/CSRC,
// marker clear) with the given payload type, sequence, timestamp, SSRC, and
// payload.
func rtpPacket(pt uint8, seq uint16, ts, ssrc uint32, payload []byte) []byte {
	b := make([]byte, 12+len(payload))
	b[0] = 0x80 // version 2
	b[1] = pt & 0x7F
	binary.BigEndian.PutUint16(b[2:], seq)
	binary.BigEndian.PutUint32(b[4:], ts)
	binary.BigEndian.PutUint32(b[8:], ssrc)
	copy(b[12:], payload)
	return b
}

// collector records delivered frames, copying each Data since it aliases
// reader-owned memory. Every access is locked.
type collector struct {
	mu     sync.Mutex
	frames []audiostream.Frame
}

//nolint:gocritic // The signature must match the OnFrame callback.
func (c *collector) onFrame(f audiostream.Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(f.Data))
	copy(cp, f.Data)
	f.Data = cp
	c.frames = append(c.frames, f)
}

func (c *collector) snapshot() []audiostream.Frame {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]audiostream.Frame, len(c.frames))
	copy(out, c.frames)
	return out
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

// openOK binds a source on loopback and fails the test on error.
//
//nolint:gocritic // Test helper mirrors Open's documented by-value Config signature.
func openOK(t *testing.T, cfg Config) *Client {
	t.Helper()
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = loopbackAddr
	}
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}

// senderFor dials the source's bound address so a test can push datagrams.
func senderFor(t *testing.T, c *Client) *net.UDPConn {
	t.Helper()
	addr := strings.TrimPrefix(c.Info().URL, "udp://")
	uaddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve %q: %v", addr, err)
	}
	conn, err := net.DialUDP("udp", nil, uaddr)
	if err != nil {
		t.Fatalf("dial %q: %v", addr, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// waitCount polls until at least n frames have been delivered or the deadline
// passes, failing the test on timeout.
func waitCount(t *testing.T, col *collector, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for col.count() < n && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if col.count() < n {
		t.Fatalf("delivered %d frames, want at least %d within %v", col.count(), n, within)
	}
}

// waitResult runs Wait in a goroutine and fails if it does not return in time.
func waitResult(t *testing.T, c *Client, within time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Wait(context.Background()) }()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatal("Wait did not return within bound")
		return nil
	}
}

// --- tests -----------------------------------------------------------------

func TestRTPG711DeliversPCM(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 0, Codec: audiostream.CodecG711{Law: audiostream.MuLaw},
		ClockRate: 8000, Channels: 1, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	payload := []byte{0xFF, 0x7F, 0x00, 0x80, 0x2A}
	send := senderFor(t, c)
	if _, err := send.Write(rtpPacket(0, 100, 8000, 0x11223344, payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitCount(t, &col, 1, 2*time.Second)

	// The delivered frame is exactly what the G.711 depacketizer produces from
	// the payload (this verifies the dispatch wiring, not the codec itself), and
	// expands 1 companded byte to one s16le sample.
	want, err := g711.DepacketizeAlloc(payload, audiostream.MuLaw)
	if err != nil {
		t.Fatalf("reference decode: %v", err)
	}
	got := col.snapshot()[0]
	if len(got.Data) != 2*len(payload) || !bytes.Equal(got.Data, want) {
		t.Fatalf("G.711 frame mismatch: got %d bytes, want %d matching the depacketizer", len(got.Data), len(want))
	}
	if got.PTS != 0 {
		t.Errorf("first frame PTS = %v, want 0", got.PTS)
	}
	if f := c.Format(); f.Kind != audiostream.KindPCMS16LE || f.SampleRate != 8000 || f.Channels != 1 {
		t.Errorf("Format = %+v, want s16le 8000/1", f)
	}
}

func TestRTPL16ByteSwaps(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecL16{ClockRate: 44100, Channels: 2},
		ClockRate: 44100, Channels: 2, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	be := []byte{0x12, 0x34, 0x56, 0x78} // two big-endian s16 samples
	send := senderFor(t, c)
	if _, err := send.Write(rtpPacket(96, 1, 0, 1, be)); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitCount(t, &col, 1, 2*time.Second)

	want := []byte{0x34, 0x12, 0x78, 0x56} // pairwise byte-swapped to little-endian
	if got := col.snapshot()[0].Data; !bytes.Equal(got, want) {
		t.Fatalf("L16 swap: got %x, want %x", got, want)
	}
	if f := c.Format(); f.Kind != audiostream.KindPCMS16LE || f.SampleRate != 44100 || f.Channels != 2 {
		t.Errorf("Format = %+v, want s16le 44100/2", f)
	}
}

func TestRTPOpusPassthrough(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, Channels: 2, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	payload := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	send := senderFor(t, c)
	if _, err := send.Write(rtpPacket(111, 7, 960, 42, payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitCount(t, &col, 1, 2*time.Second)

	if got := col.snapshot()[0].Data; !bytes.Equal(got, payload) {
		t.Fatalf("Opus passthrough: got %x, want %x", got, payload)
	}
	if f := c.Format(); f.Kind != audiostream.KindCompressed || f.SampleRate != 0 || f.Channels != 0 {
		t.Errorf("Format = %+v, want compressed 0/0", f)
	}
}

func TestRTPOpaquePassthrough(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 120, Codec: audiostream.CodecUnknown{RTPMap: "X/90000"},
		ClockRate: 90000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	send := senderFor(t, c)
	if _, err := send.Write(rtpPacket(120, 1, 0, 1, payload)); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitCount(t, &col, 1, 2*time.Second)
	if got := col.snapshot()[0].Data; !bytes.Equal(got, payload) {
		t.Fatalf("opaque passthrough: got %x, want %x", got, payload)
	}
	if f := c.Format(); f.Kind != audiostream.KindOpaque {
		t.Errorf("Format().Kind = %v, want KindOpaque", f.Kind)
	}
}

func TestRTPWrongPayloadTypeDropped(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	_, _ = send.Write(rtpPacket(97, 1, 0, 1, []byte{1, 2, 3})) // wrong PT
	_, _ = send.Write(rtpPacket(96, 2, 0, 1, []byte{4, 5, 6})) // right PT
	waitCount(t, &col, 1, 2*time.Second)

	if n := col.count(); n != 1 {
		t.Fatalf("delivered %d frames, want 1 (wrong payload type must be dropped)", n)
	}
	if m := c.Stats().Tracks[0].Malformed; m == 0 {
		t.Errorf("Malformed = 0, want >0 for the dropped wrong-payload-type datagram")
	}
}

func TestRTPMalformedDropped(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 0, Codec: audiostream.CodecG711{Law: audiostream.MuLaw},
		ClockRate: 8000, Channels: 1, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	_, _ = send.Write([]byte{0x80, 0x00}) // too short to be an RTP packet
	_, _ = send.Write(rtpPacket(0, 1, 0, 1, []byte{0x2A}))
	waitCount(t, &col, 1, 2*time.Second)
	if col.count() != 1 {
		t.Fatalf("delivered %d frames, want 1 (the truncated datagram must not yield a frame)", col.count())
	}
	if m := c.Stats().Tracks[0].Malformed; m == 0 {
		t.Errorf("Malformed = 0, want >0 for the truncated datagram")
	}
}

func TestRTPSequenceStats(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	_, _ = send.Write(rtpPacket(111, 1, 0, 1, []byte{1}))
	_, _ = send.Write(rtpPacket(111, 4, 960, 1, []byte{2})) // seq 2,3 skipped: gap 2
	_, _ = send.Write(rtpPacket(111, 4, 960, 1, []byte{3})) // duplicate seq 4
	waitCount(t, &col, 2, 2*time.Second)
	// Poll until the duplicate has been observed rather than sleeping a fixed
	// interval.
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].Duplicates == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	ts := c.Stats().Tracks[0]
	if ts.SeqGaps != 2 {
		t.Errorf("SeqGaps = %d, want 2", ts.SeqGaps)
	}
	if ts.Duplicates == 0 {
		t.Errorf("Duplicates = 0, want >0 for the repeated sequence number")
	}
	if col.count() != 2 {
		t.Errorf("delivered %d frames, want 2 (the duplicate must not be delivered)", col.count())
	}
}

func TestPCMModeDelivers(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModePCM, Format: PCMFormat{SampleRate: 16000, Channels: 1}, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	datagram := []byte{0x01, 0x00, 0x02, 0x00, 0x03, 0x00} // 3 little-endian s16 samples
	send := senderFor(t, c)
	if _, err := send.Write(datagram); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitCount(t, &col, 1, 2*time.Second)
	got := col.snapshot()[0]
	if !bytes.Equal(got.Data, datagram) {
		t.Fatalf("PCM delivery: got %x, want %x", got.Data, datagram)
	}
	if got.PTS != 0 {
		t.Errorf("first PCM frame PTS = %v, want 0", got.PTS)
	}
	if f := c.Format(); f.Kind != audiostream.KindPCMS16LE || f.SampleRate != 16000 || f.Channels != 1 {
		t.Errorf("Format = %+v, want s16le 16000/1", f)
	}
}

func TestPCMModeByteSwapsAndDropsPartialFrame(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode:    ModePCM,
		Format:  PCMFormat{SampleRate: 8000, Channels: 1, BigEndian: true},
		OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	// Two whole big-endian samples plus a trailing odd byte (partial frame).
	send := senderFor(t, c)
	if _, err := send.Write([]byte{0x12, 0x34, 0x56, 0x78, 0x99}); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitCount(t, &col, 1, 2*time.Second)
	want := []byte{0x34, 0x12, 0x78, 0x56} // swapped to little-endian, trailing byte dropped
	if got := col.snapshot()[0].Data; !bytes.Equal(got, want) {
		t.Fatalf("PCM BE swap: got %x, want %x", got, want)
	}
	if m := c.Stats().Tracks[0].Malformed; m == 0 {
		t.Errorf("Malformed = 0, want >0 for the dropped trailing partial frame")
	}
}

func TestSourceIPFilter(t *testing.T) {
	// Positive control: a source filtering on the sender's own IP delivers, which
	// proves the loopback send path works and the following rejection is genuinely
	// the filter, not a lost datagram.
	var accepted collector
	ca := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, SourceIP: "127.0.0.1", OnFrame: accepted.onFrame,
	})
	defer func() { _ = ca.Close() }()
	_, _ = senderFor(t, ca).Write(rtpPacket(111, 1, 0, 1, []byte{1, 2, 3}))
	waitCount(t, &accepted, 1, 2*time.Second)

	// A source filtering on a different IP drops the same sender's datagram.
	var rejected collector
	cr := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, SourceIP: "127.0.0.2", OnFrame: rejected.onFrame, // sender is 127.0.0.1
	})
	defer func() { _ = cr.Close() }()
	_, _ = senderFor(t, cr).Write(rtpPacket(111, 1, 0, 1, []byte{1, 2, 3}))
	time.Sleep(150 * time.Millisecond)
	if rejected.count() != 0 {
		t.Fatalf("delivered %d frames, want 0 (datagram from a non-allowlisted IP)", rejected.count())
	}
}

func TestReadIdleWatchdog(t *testing.T) {
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{},
		ClockRate: 48000, ReadIdle: 100 * time.Millisecond,
	})
	defer func() { _ = c.Close() }()
	if err := waitResult(t, c, 3*time.Second); !errors.Is(err, audiostream.ErrReadTimeout) {
		t.Fatalf("Wait = %v, want ErrReadTimeout", err)
	}
}

func TestCloseMidStream(t *testing.T) {
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000,
	})
	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil (idempotent)", err)
	}
	if err := waitResult(t, c, 3*time.Second); !errors.Is(err, audiostream.ErrClosed) {
		t.Fatalf("Wait = %v, want ErrClosed", err)
	}
}

func TestWaitContextCancel(t *testing.T) {
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000,
	})
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait = %v, want context.Canceled", err)
	}
}

func TestOpenInvalidConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"empty listen addr", Config{Mode: ModeRTP, Codec: audiostream.CodecOpus{}, ClockRate: 48000}, ErrInvalidConfig},
		{"rtp missing clock", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecOpus{}}, ErrInvalidConfig},
		{"rtp nil codec", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, ClockRate: 48000}, ErrInvalidConfig},
		{"rtp pcm codec missing channels", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecG711{Law: audiostream.MuLaw}, ClockRate: 8000}, ErrInvalidConfig},
		{"pcm missing rate", Config{ListenAddr: loopbackAddr, Mode: ModePCM, Format: PCMFormat{Channels: 1}}, ErrInvalidConfig},
		{"bad source ip", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecOpus{}, ClockRate: 48000, SourceIP: "not-an-ip"}, ErrInvalidConfig},
		{"unsupported codec", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecAAC{}, ClockRate: 90000}, ErrUnsupportedCodec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Open(context.Background(), tc.cfg); !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOpenNilOnFrameStillCounts(t *testing.T) {
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000,
	}) // OnFrame nil
	defer func() { _ = c.Close() }()
	send := senderFor(t, c)
	_, _ = send.Write(rtpPacket(111, 1, 0, 1, []byte{1, 2, 3}))
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].Packets == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	stats := c.Stats().Tracks[0]
	if stats.Packets != 1 || stats.PayloadBytes != 3 {
		t.Fatalf("Stats = packets %d payload %d, want 1/3 with nil OnFrame", stats.Packets, stats.PayloadBytes)
	}
}

func TestRTPPTSProgression(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	_, _ = send.Write(rtpPacket(111, 1, 5000, 7, []byte{1}))     // base 5000 -> PTS 0
	_, _ = send.Write(rtpPacket(111, 2, 5000+960, 7, []byte{2})) // +960 samples at 48000 -> +20ms
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if frames[0].PTS != 0 {
		t.Errorf("frame 0 PTS = %v, want 0 (rebased to the origin)", frames[0].PTS)
	}
	if want := 20 * time.Millisecond; frames[1].PTS != want {
		t.Errorf("frame 1 PTS = %v, want %v", frames[1].PTS, want)
	}
}

func TestRTPBackwardTimestampClampsPTS(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	_, _ = send.Write(rtpPacket(111, 1, 10000, 7, []byte{1})) // origin 10000
	_, _ = send.Write(rtpPacket(111, 2, 500, 7, []byte{2}))   // forward seq, timestamp before the origin
	waitCount(t, &col, 2, 2*time.Second)

	// The second frame's timestamp unwraps below the origin; PTS must clamp to 0,
	// not underflow to a nonsense (clamped-huge) value.
	if pts := col.snapshot()[1].PTS; pts != 0 {
		t.Errorf("backward-timestamp frame PTS = %v, want 0 (clamped, not underflowed)", pts)
	}
}

func TestRTPSSRCReset(t *testing.T) {
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	_, _ = send.Write(rtpPacket(111, 1, 1000, 0xAAAA, []byte{1}))   // SSRC A, base 1000
	_, _ = send.Write(rtpPacket(111, 2, 1960, 0xAAAA, []byte{2}))   // +960 -> 20ms
	_, _ = send.Write(rtpPacket(111, 100, 7000, 0xBBBB, []byte{3})) // SSRC B: reset, new base 7000
	waitCount(t, &col, 3, 2*time.Second)

	if pts := col.snapshot()[2].PTS; pts != 0 {
		t.Errorf("first frame after SSRC reset PTS = %v, want 0 (re-based)", pts)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].SSRCResets == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if r := c.Stats().Tracks[0].SSRCResets; r != 1 {
		t.Errorf("SSRCResets = %d, want 1", r)
	}
}

func TestRTPDepacketizeMalformed(t *testing.T) {
	t.Run("L16 sub-frame payload", func(t *testing.T) {
		var col collector
		c := openOK(t, Config{
			Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecL16{ClockRate: 44100, Channels: 2},
			ClockRate: 44100, Channels: 2, OnFrame: col.onFrame,
		})
		defer func() { _ = c.Close() }()
		send := senderFor(t, c)
		_, _ = send.Write(rtpPacket(96, 1, 0, 1, []byte{0x12, 0x34}))             // 2 bytes < frameBytes (4)
		_, _ = send.Write(rtpPacket(96, 2, 0, 1, []byte{0x12, 0x34, 0x56, 0x78})) // one whole stereo frame
		waitCount(t, &col, 1, 2*time.Second)
		if col.count() != 1 {
			t.Fatalf("delivered %d frames, want 1 (the sub-frame L16 payload must be dropped)", col.count())
		}
		if c.Stats().Tracks[0].Malformed == 0 {
			t.Error("Malformed = 0, want >0 for the sub-frame L16 payload")
		}
	})
	t.Run("empty Opus payload", func(t *testing.T) {
		var col collector
		c := openOK(t, Config{
			Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000, OnFrame: col.onFrame,
		})
		defer func() { _ = c.Close() }()
		send := senderFor(t, c)
		_, _ = send.Write(rtpPacket(111, 1, 0, 1, nil)) // empty payload
		_, _ = send.Write(rtpPacket(111, 2, 960, 1, []byte{1}))
		waitCount(t, &col, 1, 2*time.Second)
		if c.Stats().Tracks[0].Malformed == 0 {
			t.Error("Malformed = 0, want >0 for the empty Opus payload")
		}
	})
}

func TestOnFramePanicRecover(t *testing.T) {
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000,
		OnFrame: func(audiostream.Frame) { panic("boom") },
	})
	defer func() { _ = c.Close() }()
	_, _ = senderFor(t, c).Write(rtpPacket(111, 1, 0, 1, []byte{1}))
	err := waitResult(t, c, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "reader panic") {
		t.Fatalf("Wait = %v, want a reader-panic terminal cause", err)
	}
}

func TestOpenErrBind(t *testing.T) {
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000,
	})
	defer func() { _ = c.Close() }()
	addr := strings.TrimPrefix(c.Info().URL, "udp://")
	_, err := Open(context.Background(), Config{
		ListenAddr: addr, Mode: ModeRTP, PayloadType: 111, Codec: audiostream.CodecOpus{}, ClockRate: 48000,
	})
	if !errors.Is(err, ErrBind) {
		t.Fatalf("Open on an already-bound address = %v, want ErrBind", err)
	}
}
