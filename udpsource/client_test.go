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
	"github.com/tphakala/go-audio-stream/depacket/aac"
	"github.com/tphakala/go-audio-stream/depacket/g711"
	"github.com/tphakala/go-audio-stream/depacket/g726"
	"github.com/tphakala/go-audio-stream/internal/mediatime"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// loopbackAddr is the wildcard-port loopback bind used across the source tests.
const loopbackAddr = "127.0.0.1:0"

// rtpMap90k is the opaque-codec rtpmap used across the ModeRTP source tests.
const rtpMap90k = "X/90000"

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

// sendAndSettle writes one datagram and blocks until the reader has consumed it,
// so a following datagram cannot race ahead of it into the reader. Loopback UDP
// does not guarantee datagram order, so reorder tests that depend on a specific
// arrival order to set up the Reorderer's buffered state use this instead of a
// bare Write. WireBytes is stamped on every accepted datagram before dispatch, so
// once it reflects this datagram the reader has read it and, being
// single-threaded, finishes dispatching it before reading the next.
func sendAndSettle(t *testing.T, c *Client, conn *net.UDPConn, datagram []byte) {
	t.Helper()
	want := c.Stats().Tracks[0].WireBytes + uint64(len(datagram))
	if _, err := conn.Write(datagram); err != nil {
		t.Fatalf("write: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].WireBytes < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.Stats().Tracks[0].WireBytes < want {
		t.Fatalf("reader did not consume a %d-byte datagram within 2s", len(datagram))
	}
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

// deliveredSeqMarkers returns the first payload byte of each delivered frame.
// The reorder tests tag every datagram with a distinct low sequence number as
// its single payload byte (via the opaque codec, delivered verbatim), so this is
// the delivery order the assertions compare against.
func deliveredSeqMarkers(frames []audiostream.Frame) []byte {
	out := make([]byte, len(frames))
	for i, f := range frames {
		if len(f.Data) > 0 {
			out[i] = f.Data[0]
		}
	}
	return out
}

// opaqueReorderCfg is the shared config for the reorder tests: an opaque
// passthrough (so Frame.Data is exactly the sent payload and ordering is a
// trivial byte compare) on payload type 96, with reordering set per the caller.
//
//nolint:gocritic // Test helper mirrors Open's documented by-value Config signature.
func opaqueReorderCfg(reorder bool, onFrame func(audiostream.Frame)) Config {
	return Config{
		Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecUnknown{RTPMap: rtpMap90k},
		ClockRate: 90000, Reorder: reorder, OnFrame: onFrame,
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
		Mode: ModeRTP, PayloadType: 120, Codec: audiostream.CodecUnknown{RTPMap: rtpMap90k},
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
		// wantCause, when non-nil, is a precise underlying cause Open's error must
		// also wrap (checked with errors.Is in addition to want). The zero value
		// skips the check, so a row asserts a cause only when the precise one is the
		// point of the case.
		wantCause error
	}{
		{"empty listen addr", Config{Mode: ModeRTP, Codec: audiostream.CodecOpus{}, ClockRate: 48000}, ErrInvalidConfig, nil},
		{"rtp missing clock", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecOpus{}}, ErrInvalidConfig, nil},
		{"rtp nil codec", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, ClockRate: 48000}, ErrInvalidConfig, nil},
		{"rtp pcm codec missing channels", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecG711{Law: audiostream.MuLaw}, ClockRate: 8000}, ErrInvalidConfig, nil},
		{"rtp g726 missing channels", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecG726{BitRate: audiostream.G726Rate32}, ClockRate: 8000}, ErrInvalidConfig, nil},
		{"rtp g726 bad rate", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecG726{BitRate: 99, Packing: audiostream.G726PackingRFC3551}, ClockRate: 8000, Channels: 1}, ErrInvalidConfig, g726.ErrUnknownBitRate},
		{"rtp g726 non-mono", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecG726{BitRate: audiostream.G726Rate32}, ClockRate: 8000, Channels: 2}, ErrInvalidConfig, nil},
		{"rtp g726 wrong clock", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecG726{BitRate: audiostream.G726Rate32}, ClockRate: 16000, Channels: 1}, ErrInvalidConfig, nil},
		// An out-of-range packing is refused for the same reason as an
		// out-of-range bit rate: unpacking with the wrong bit order decodes
		// without error into plausible but wrong audio.
		{"rtp g726 bad packing", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, PayloadType: 96, Codec: audiostream.CodecG726{BitRate: audiostream.G726Rate32, Packing: audiostream.G726Packing(99)}, ClockRate: 8000, Channels: 1}, ErrInvalidConfig, g726.ErrUnknownPacking},
		{"pcm missing rate", Config{ListenAddr: loopbackAddr, Mode: ModePCM, Format: PCMFormat{Channels: 1}}, ErrInvalidConfig, nil},
		{"bad source ip", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecOpus{}, ClockRate: 48000, SourceIP: "not-an-ip"}, ErrInvalidConfig, nil},
		{"payload type above 127", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, PayloadType: 200, Codec: audiostream.CodecOpus{}, ClockRate: 48000}, ErrInvalidConfig, nil},
		{"unsupported codec", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, Codec: audiostream.CodecMP3{}, ClockRate: 90000}, ErrUnsupportedCodec, nil},
		{"aac invalid widths", Config{ListenAddr: loopbackAddr, Mode: ModeRTP, PayloadType: 97, Codec: audiostream.CodecAAC{}, ClockRate: 44100, AAC: AACParams{SizeLength: 0, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024}}, ErrInvalidConfig, aac.ErrConfigInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Open(context.Background(), tc.cfg)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open = %v, want %v", err, tc.want)
			}
			if tc.wantCause != nil && !errors.Is(err, tc.wantCause) {
				t.Fatalf("Open = %v, want it to wrap %v", err, tc.wantCause)
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

// --- reorder tests ---------------------------------------------------------

func TestReorderDropsCountAsDuplicates(t *testing.T) {
	// Every kind of dropped datagram is counted exactly once in Duplicates and
	// never delivered: a backward arrival on the disabled path (which drops rather
	// than reorders, locking in the refactored disabled path), a duplicate
	// sequence number on the enabled path, and an arrival older than the enabled
	// path's release point. The enabled cases exercise the Reorderer late count
	// folding into Duplicates.
	cases := []struct {
		name      string
		reorder   bool
		sends     []uint16
		wantOrder []byte
	}{
		{"disabled backward drop", false, []uint16{1, 3, 2}, []byte{1, 3}},
		{"enabled duplicate sequence", true, []uint16{1, 2, 2}, []byte{1, 2}},
		{"enabled late arrival", true, []uint16{1, 2, 3, 1}, []byte{1, 2, 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var col collector
			c := openOK(t, opaqueReorderCfg(tc.reorder, col.onFrame))
			defer func() { _ = c.Close() }()

			send := senderFor(t, c)
			for _, seq := range tc.sends {
				sendAndSettle(t, c, send, rtpPacket(96, seq, 0, 1, []byte{byte(seq)}))
			}
			waitCount(t, &col, len(tc.wantOrder), 2*time.Second)

			deadline := time.Now().Add(2 * time.Second)
			for c.Stats().Tracks[0].Duplicates == 0 && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			if got := deliveredSeqMarkers(col.snapshot()); !bytes.Equal(got, tc.wantOrder) {
				t.Fatalf("delivery order = %v, want %v (dropped datagram not delivered)", got, tc.wantOrder)
			}
			if d := c.Stats().Tracks[0].Duplicates; d != 1 {
				t.Errorf("Duplicates = %d, want 1", d)
			}
		})
	}
}

func TestReorderInOrderPassthrough(t *testing.T) {
	// With reordering enabled, an already in-order stream delivers unchanged and
	// records no duplicates or gaps.
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	for seq := uint16(1); seq <= 3; seq++ {
		_, _ = send.Write(rtpPacket(96, seq, 0, 1, []byte{byte(seq)}))
	}
	waitCount(t, &col, 3, 2*time.Second)

	if got := deliveredSeqMarkers(col.snapshot()); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("in-order delivery = %v, want [1 2 3]", got)
	}
	ts := c.Stats().Tracks[0]
	if ts.Duplicates != 0 || ts.SeqGaps != 0 {
		t.Errorf("Duplicates=%d SeqGaps=%d, want 0/0 for an in-order stream", ts.Duplicates, ts.SeqGaps)
	}
}

func TestReorderRecoversOutOfOrder(t *testing.T) {
	// The core recovery: a late-but-in-window datagram is resequenced and the run
	// is delivered in ascending sequence order, with no gap or duplicate charged.
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	sendAndSettle(t, c, send, rtpPacket(96, 1, 0, 1, []byte{1}))
	sendAndSettle(t, c, send, rtpPacket(96, 3, 0, 1, []byte{3})) // arrives early, buffered
	sendAndSettle(t, c, send, rtpPacket(96, 2, 0, 1, []byte{2})) // fills the hole, releases 2 then 3
	waitCount(t, &col, 3, 2*time.Second)

	if got := deliveredSeqMarkers(col.snapshot()); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("recovered delivery order = %v, want [1 2 3]", got)
	}
	ts := c.Stats().Tracks[0]
	if ts.Duplicates != 0 || ts.SeqGaps != 0 {
		t.Errorf("Duplicates=%d SeqGaps=%d, want 0/0 (the hole was filled)", ts.Duplicates, ts.SeqGaps)
	}
}

func TestReorderWindowOverflowForcesRelease(t *testing.T) {
	// A never-arriving sequence number is force-released once a later datagram
	// lands more than a window ahead, so a buffered packet behind the gap still
	// gets out and the loss is counted rather than the run stalling forever.
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	sendAndSettle(t, c, send, rtpPacket(96, 1, 0, 1, []byte{1}))                         // released, next=2
	sendAndSettle(t, c, send, rtpPacket(96, 3, 0, 1, []byte{3}))                         // buffered, waits for 2
	sendAndSettle(t, c, send, rtpPacket(96, 3+rtp.MaxReorderWindow, 0, 1, []byte{0x7F})) // forces seq 2 lost, releases 3
	waitCount(t, &col, 2, 2*time.Second)

	if got := deliveredSeqMarkers(col.snapshot()); !bytes.Equal(got, []byte{1, 3}) {
		t.Fatalf("delivery = %v, want [1 3] (seq 2 force-released as lost)", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].SeqGaps == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if g := c.Stats().Tracks[0].SeqGaps; g != 1 {
		t.Errorf("SeqGaps = %d, want 1 (the lost seq 2)", g)
	}
}

func TestReorderSSRCChangeFlushesInOrder(t *testing.T) {
	// An SSRC change drains the old source's buffered packets in order before the
	// new source establishes a fresh release point, and the first new-source frame
	// is PTS-rebased to the origin.
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	sendAndSettle(t, c, send, rtpPacket(96, 10, 1000, 0xAAAA, []byte{10}))   // SSRC A: released
	sendAndSettle(t, c, send, rtpPacket(96, 12, 2920, 0xAAAA, []byte{12}))   // SSRC A: buffered (11 missing)
	sendAndSettle(t, c, send, rtpPacket(96, 100, 7000, 0xBBBB, []byte{100})) // SSRC B: flush A, then reset
	waitCount(t, &col, 3, 2*time.Second)

	frames := col.snapshot()
	if got := deliveredSeqMarkers(frames); !bytes.Equal(got, []byte{10, 12, 100}) {
		t.Fatalf("delivery = %v, want [10 12 100] (A drained in order before B)", got)
	}
	if frames[2].PTS != 0 {
		t.Errorf("first frame after SSRC change PTS = %v, want 0 (rebased)", frames[2].PTS)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].SSRCResets == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if r := c.Stats().Tracks[0].SSRCResets; r != 1 {
		t.Errorf("SSRCResets = %d, want 1", r)
	}
}

func TestReorderForeignAndMalformedDroppedAtReceive(t *testing.T) {
	// A foreign payload type whose SSRC does not match the active stream, and an
	// unparseable datagram, are both dropped before the resequencer and counted
	// malformed, so unrelated traffic on the wildcard port cannot poison or thrash
	// the buffer. This also covers handleRTPReordered's parse-error branch.
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	sendAndSettle(t, c, send, rtpPacket(96, 1, 0, 0xAAAA, []byte{1})) // establishes SSRC A
	sendAndSettle(t, c, send, rtpPacket(97, 9, 0, 0xCCCC, []byte{9})) // foreign PT, different SSRC: dropped at receive
	sendAndSettle(t, c, send, []byte{0x80, 0x00})                     // too short to parse: dropped at receive
	sendAndSettle(t, c, send, rtpPacket(96, 2, 0, 0xAAAA, []byte{2})) // our PT, SSRC A: delivered
	waitCount(t, &col, 2, 2*time.Second)

	if got := deliveredSeqMarkers(col.snapshot()); !bytes.Equal(got, []byte{1, 2}) {
		t.Fatalf("delivery = %v, want [1 2] (foreign and malformed datagrams dropped)", got)
	}
	if m := c.Stats().Tracks[0].Malformed; m != 2 {
		t.Errorf("Malformed = %d, want 2 (the foreign-SSRC payload type and the truncated datagram)", m)
	}
}

func TestReorderSameSSRCMuxDoesNotStall(t *testing.T) {
	// A foreign payload type multiplexed on the ACTIVE SSRC (an in-band event
	// occupying a real sequence number, e.g. an RFC 4733 telephone-event) must not
	// stall the stream: it fills its slot and is filtered at drain, so the next
	// real packet is delivered PROMPTLY (the waitCount below, which a receive-time
	// drop would hang until window overflow and fail) rather than waiting a whole
	// reorder window for a phantom gap to force-release.
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	sendAndSettle(t, c, send, rtpPacket(96, 1, 0, 0xAAAA, []byte{1})) // our PT, seq 1
	sendAndSettle(t, c, send, rtpPacket(97, 2, 0, 0xAAAA, []byte{2})) // foreign PT, SAME SSRC, seq 2
	sendAndSettle(t, c, send, rtpPacket(96, 3, 0, 0xAAAA, []byte{3})) // our PT, seq 3
	waitCount(t, &col, 2, 2*time.Second)

	if got := deliveredSeqMarkers(col.snapshot()); !bytes.Equal(got, []byte{1, 3}) {
		t.Fatalf("delivery = %v, want [1 3] promptly (seq 2 mux filled its slot, seq 3 not stalled)", got)
	}
	ts := c.Stats().Tracks[0]
	if ts.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1 (the in-band mux packet, filtered at drain)", ts.Malformed)
	}
	// The mux packet's sequence number is filtered before Observe, so the audio
	// stream legitimately skips it: SeqGaps counts it as one missing audio packet.
	// The point of the fix is that this shows up immediately without a stall, not
	// that the gap disappears.
	if ts.SeqGaps != 1 {
		t.Errorf("SeqGaps = %d, want 1 (the audio stream skips the filtered mux sequence number)", ts.SeqGaps)
	}
}

func TestReorderManyPacketsDeliverInOrder(t *testing.T) {
	// Drive more than one reorder window of datagrams, each with its own payload,
	// and confirm every frame carries the payload of its own sequence: delivery is
	// correct and lossless across a span longer than the fixed window, and the
	// per-packet copies never mix payloads.
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	const n = rtp.MaxReorderWindow + 20
	send := senderFor(t, c)
	for seq := uint16(1); seq <= n; seq++ {
		if _, err := send.Write(rtpPacket(96, seq, 0, 1, []byte{byte(seq)})); err != nil {
			t.Fatalf("write seq %d: %v", seq, err)
		}
	}
	waitCount(t, &col, n, 3*time.Second)

	frames := col.snapshot()
	if len(frames) != n {
		t.Fatalf("delivered %d frames, want %d", len(frames), n)
	}
	for i, f := range frames {
		if want := byte(i + 1); len(f.Data) != 1 || f.Data[0] != want {
			t.Fatalf("frame %d payload = %x, want [%02x] (a copy aliased another packet)", i, f.Data, want)
		}
	}
	if d := c.Stats().Tracks[0].Duplicates; d != 0 {
		t.Errorf("Duplicates = %d, want 0", d)
	}
}

func TestReorderDuplicateSeqKeepsBufferedPayload(t *testing.T) {
	// A duplicate of a still-buffered sequence number is rejected as late and does
	// not disturb the buffered packet: the delivered frame carries the payload of
	// the first (buffered) copy, and the per-packet copy makes that deterministic.
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	sendAndSettle(t, c, send, rtpPacket(96, 1, 0, 1, []byte{1}))
	sendAndSettle(t, c, send, rtpPacket(96, 3, 0, 1, []byte{0xA3})) // seq 3, payload A: buffered
	sendAndSettle(t, c, send, rtpPacket(96, 3, 0, 1, []byte{0xB3})) // seq 3 again: rejected as late, cannot corrupt A
	sendAndSettle(t, c, send, rtpPacket(96, 2, 0, 1, []byte{2}))    // releases 2 then 3
	waitCount(t, &col, 3, 2*time.Second)

	frames := col.snapshot()
	if got := deliveredSeqMarkers(frames); !bytes.Equal(got, []byte{1, 2, 0xA3}) {
		t.Fatalf("delivery = %v, want [1 2 A3] (buffered payload preserved)", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].Duplicates == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if d := c.Stats().Tracks[0].Duplicates; d != 1 {
		t.Errorf("Duplicates = %d, want 1 (the duplicate seq 3)", d)
	}
}

func TestReorderDropCountSurvivesSSRCReset(t *testing.T) {
	// A late drop is counted, and an SSRC change resets the Reorderer, zeroing its
	// internal late counter. The next Push reads the late count fresh after the
	// reset, so a drop before the reset is preserved and a drop after it is still
	// counted, neither lost nor double-counted through the zeroing (guarding the
	// compare-not-subtract accounting across a reset).
	var col collector
	c := openOK(t, opaqueReorderCfg(true, col.onFrame))
	defer func() { _ = c.Close() }()

	send := senderFor(t, c)
	sendAndSettle(t, c, send, rtpPacket(96, 5, 0, 0xAAAA, []byte{5}))     // SSRC A: released, next=6
	sendAndSettle(t, c, send, rtpPacket(96, 3, 0, 0xAAAA, []byte{3}))     // SSRC A: before release point, dropped late
	sendAndSettle(t, c, send, rtpPacket(96, 100, 0, 0xBBBB, []byte{100})) // SSRC B: flush+reset (zeroes late), released
	sendAndSettle(t, c, send, rtpPacket(96, 50, 0, 0xBBBB, []byte{50}))   // SSRC B: before release point, dropped late

	waitCount(t, &col, 2, 2*time.Second)
	if got := deliveredSeqMarkers(col.snapshot()); !bytes.Equal(got, []byte{5, 100}) {
		t.Fatalf("delivery = %v, want [5 100]", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].Duplicates < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	ts := c.Stats().Tracks[0]
	if ts.Duplicates != 2 {
		t.Errorf("Duplicates = %d, want 2 (one late drop before the reset, one after)", ts.Duplicates)
	}
	if ts.SSRCResets != 1 {
		t.Errorf("SSRCResets = %d, want 1", ts.SSRCResets)
	}
}

func TestReorderIgnoredInPCMMode(t *testing.T) {
	// Reorder is a ModeRTP option; a ModePCM source ignores it (the resequencer is
	// never allocated) and delivers datagrams normally.
	var col collector
	c := openOK(t, Config{
		Mode: ModePCM, Format: PCMFormat{SampleRate: 16000, Channels: 1}, Reorder: true, OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()
	if c.reorder != nil {
		t.Fatal("reorder resequencer allocated for a ModePCM source, want nil (flag ignored)")
	}

	datagram := []byte{0x01, 0x00, 0x02, 0x00}
	if _, err := senderFor(t, c).Write(datagram); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitCount(t, &col, 1, 2*time.Second)
	if got := col.snapshot()[0].Data; !bytes.Equal(got, datagram) {
		t.Fatalf("PCM delivery = %x, want %x", got, datagram)
	}
}

// --- AAC over raw RTP tests ------------------------------------------------

// aacAUHeader16 packs one AAC-hbr AU-header into 16 bits big-endian: the 13-bit
// size in the high bits and the 3-bit index (or index-delta) in the low bits,
// matching the depacket/aac test builders.
func aacAUHeader16(size, index int) []byte {
	v := uint16(size<<3) | uint16(index&0x07)
	return []byte{byte(v >> 8), byte(v)}
}

// buildAACHBR assembles an AAC-hbr payload: a 16-bit AU-headers-length in bits
// (16 * len(headers)), then the packed headers, then the concatenated AU data.
func buildAACHBR(headers, data [][]byte) []byte {
	bits := len(headers) * 16
	out := []byte{byte(bits >> 8), byte(bits)}
	for _, h := range headers {
		out = append(out, h...)
	}
	for _, d := range data {
		out = append(out, d...)
	}
	return out
}

// withMarker sets the RTP marker (M) bit on a packet built by rtpPacket, which
// clears it. The final fragment of a fragmented access unit carries the marker.
func withMarker(datagram []byte) []byte {
	datagram[1] |= 0x80
	return datagram
}

// aacRTPCfg is the shared AAC-hbr config: payload type 97, a 44100 Hz clock, and
// the ubiquitous 13/3/3 AU-header widths at 1024 samples per frame.
//
//nolint:gocritic // Test helper mirrors Open's documented by-value Config signature.
func aacRTPCfg(onFrame func(audiostream.Frame)) Config {
	return Config{
		Mode: ModeRTP, PayloadType: 97, Codec: audiostream.CodecAAC{}, ClockRate: 44100,
		AAC:     AACParams{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024},
		OnFrame: onFrame,
	}
}

func TestRTPAACSingleAU(t *testing.T) {
	var col collector
	c := openOK(t, aacRTPCfg(col.onFrame))
	defer func() { _ = c.Close() }()

	data := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	pkt := buildAACHBR([][]byte{aacAUHeader16(len(data), 0)}, [][]byte{data})
	_, _ = senderFor(t, c).Write(withMarker(rtpPacket(97, 1, 1000, 5, pkt)))
	waitCount(t, &col, 1, 2*time.Second)

	f := col.snapshot()[0]
	if !bytes.Equal(f.Data, data) {
		t.Errorf("AU data = % x, want % x", f.Data, data)
	}
	if f.PTS != 0 {
		t.Errorf("PTS = %v, want 0 (rebased to the origin)", f.PTS)
	}
	if f.SeqGap != 0 {
		t.Errorf("SeqGap = %d, want 0", f.SeqGap)
	}
	if p := c.Stats().Tracks[0].Packets; p != 1 {
		t.Errorf("Packets = %d, want 1", p)
	}
}

func TestRTPAACMultiAU(t *testing.T) {
	var col collector
	c := openOK(t, aacRTPCfg(col.onFrame))
	defer func() { _ = c.Close() }()

	d0 := []byte{0x11, 0x22, 0x33}
	d1 := []byte{0x44, 0x55, 0x66, 0x77, 0x88}
	pkt := buildAACHBR(
		[][]byte{aacAUHeader16(len(d0), 0), aacAUHeader16(len(d1), 0)},
		[][]byte{d0, d1},
	)
	_, _ = senderFor(t, c).Write(withMarker(rtpPacket(97, 1, 2000, 5, pkt)))
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if !bytes.Equal(frames[0].Data, d0) || frames[0].PTS != 0 || frames[0].SeqGap != 0 {
		t.Errorf("AU0 = % x @ %v gap %d, want % x @ 0 gap 0", frames[0].Data, frames[0].PTS, frames[0].SeqGap, d0)
	}
	// The second access unit advances by one SamplesPerFrame (1024) at 44100 Hz.
	if want := mediatime.PTSFromSamples(1024, 44100); frames[1].PTS != want {
		t.Errorf("AU1 PTS = %v, want %v", frames[1].PTS, want)
	}
	if !bytes.Equal(frames[1].Data, d1) {
		t.Errorf("AU1 data = % x, want % x", frames[1].Data, d1)
	}
	// One RTP packet is one Packet regardless of how many access units it carries.
	if p := c.Stats().Tracks[0].Packets; p != 1 {
		t.Errorf("Packets = %d, want 1", p)
	}
}

func TestRTPAACFragmentedAU(t *testing.T) {
	var col collector
	c := openOK(t, aacRTPCfg(col.onFrame))
	defer func() { _ = c.Close() }()
	send := senderFor(t, c)

	full := []byte{1, 2, 3, 4, 5, 6}
	// Two fragments of one access unit share the RTP timestamp. The first declares
	// the full size but carries only the first half (marker clear); the second
	// carries the rest with the marker set to complete the unit.
	frag1 := buildAACHBR([][]byte{aacAUHeader16(len(full), 0)}, [][]byte{full[:3]})
	frag2 := buildAACHBR([][]byte{aacAUHeader16(len(full), 0)}, [][]byte{full[3:]})
	sendAndSettle(t, c, send, rtpPacket(97, 1, 1000, 5, frag1))
	sendAndSettle(t, c, send, withMarker(rtpPacket(97, 2, 1000, 5, frag2)))
	waitCount(t, &col, 1, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 1 {
		t.Fatalf("delivered %d frames, want 1 (the reassembled access unit)", len(frames))
	}
	if !bytes.Equal(frames[0].Data, full) {
		t.Errorf("reassembled AU = % x, want % x", frames[0].Data, full)
	}
	if frames[0].PTS != 0 {
		t.Errorf("PTS = %v, want 0 (both fragments carry the origin timestamp)", frames[0].PTS)
	}
	if p := c.Stats().Tracks[0].Packets; p != 2 {
		t.Errorf("Packets = %d, want 2 (both fragment packets accepted)", p)
	}
}

func TestRTPAACMalformedCounted(t *testing.T) {
	var col collector
	c := openOK(t, aacRTPCfg(col.onFrame))
	defer func() { _ = c.Close() }()
	send := senderFor(t, c)

	// A zero AU-headers-length is a truncated/inconsistent header: counted
	// malformed, no frame. A following well-formed packet must still deliver,
	// proving the reader kept running. sendAndSettle keeps the malformed packet
	// ahead of the good one, since delivery order is load-bearing here.
	sendAndSettle(t, c, send, rtpPacket(97, 1, 1000, 5, []byte{0x00, 0x00}))
	good := []byte{0x9A, 0x9B}
	sendAndSettle(t, c, send, withMarker(rtpPacket(97, 2, 1000, 5, buildAACHBR([][]byte{aacAUHeader16(len(good), 0)}, [][]byte{good}))))
	waitCount(t, &col, 1, 2*time.Second)

	if got := col.snapshot()[0].Data; !bytes.Equal(got, good) {
		t.Errorf("recovered AU = % x, want % x", got, good)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].Malformed == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if m := c.Stats().Tracks[0].Malformed; m != 1 {
		t.Errorf("Malformed = %d, want 1", m)
	}
}

func TestRTPAACPTSProgression(t *testing.T) {
	var col collector
	c := openOK(t, aacRTPCfg(col.onFrame))
	defer func() { _ = c.Close() }()
	send := senderFor(t, c)

	one := func(seq uint16, ts uint32, b byte) []byte {
		return withMarker(rtpPacket(97, seq, ts, 5, buildAACHBR([][]byte{aacAUHeader16(1, 0)}, [][]byte{{b}})))
	}
	sendAndSettle(t, c, send, one(1, 5000, 0x01))      // base 5000 -> PTS 0
	sendAndSettle(t, c, send, one(2, 5000+1024, 0x02)) // +1024 samples at 44100
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if frames[0].PTS != 0 {
		t.Errorf("frame 0 PTS = %v, want 0", frames[0].PTS)
	}
	if want := mediatime.PTSFromSamples(1024, 44100); frames[1].PTS != want {
		t.Errorf("frame 1 PTS = %v, want %v", frames[1].PTS, want)
	}
}

func TestRTPAACFragmentResetOnGap(t *testing.T) {
	var col collector
	c := openOK(t, aacRTPCfg(col.onFrame))
	defer func() { _ = c.Close() }()
	send := senderFor(t, c)

	// Begin a fragmented access unit (marker clear, declared size exceeds the
	// bytes present), then jump the sequence forward so a packet is lost. The gap
	// must drop the partial reassembly, so the next clean packet delivers its own
	// access unit rather than a corrupt splice of the abandoned fragment.
	frag1 := buildAACHBR([][]byte{aacAUHeader16(6, 0)}, [][]byte{{1, 2, 3}})
	sendAndSettle(t, c, send, rtpPacket(97, 1, 1000, 5, frag1))

	clean := []byte{0x77, 0x88}
	cleanPkt := buildAACHBR([][]byte{aacAUHeader16(len(clean), 0)}, [][]byte{clean})
	sendAndSettle(t, c, send, withMarker(rtpPacket(97, 3, 2000, 5, cleanPkt))) // seq 2 lost
	waitCount(t, &col, 1, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 1 {
		t.Fatalf("delivered %d frames, want 1 (the abandoned fragment must not surface)", len(frames))
	}
	if !bytes.Equal(frames[0].Data, clean) {
		t.Errorf("post-gap AU = % x, want % x (no splice of the dropped fragment)", frames[0].Data, clean)
	}
	if frames[0].SeqGap != 1 {
		t.Errorf("SeqGap = %d, want 1 (the lost seq 2)", frames[0].SeqGap)
	}
}

func TestRTPAACFragmentResetOnSSRCReset(t *testing.T) {
	var col collector
	c := openOK(t, aacRTPCfg(col.onFrame))
	defer func() { _ = c.Close() }()
	send := senderFor(t, c)

	// Begin a fragmented access unit on one source, then switch SSRC. The new
	// source restarts the media timeline, so the partial reassembly from the old
	// source must be dropped; the new source's first clean packet delivers its own
	// access unit instead of a corrupt splice.
	frag1 := buildAACHBR([][]byte{aacAUHeader16(6, 0)}, [][]byte{{1, 2, 3}})
	sendAndSettle(t, c, send, rtpPacket(97, 1, 1000, 0xAAAA, frag1))

	clean := []byte{0x55, 0x66}
	cleanPkt := buildAACHBR([][]byte{aacAUHeader16(len(clean), 0)}, [][]byte{clean})
	sendAndSettle(t, c, send, withMarker(rtpPacket(97, 100, 7000, 0xBBBB, cleanPkt))) // new SSRC
	waitCount(t, &col, 1, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 1 {
		t.Fatalf("delivered %d frames, want 1 (the old source's fragment must not surface)", len(frames))
	}
	if !bytes.Equal(frames[0].Data, clean) {
		t.Errorf("post-reset AU = % x, want % x (no splice of the dropped fragment)", frames[0].Data, clean)
	}
	if frames[0].PTS != 0 {
		t.Errorf("PTS = %v, want 0 (re-based on the new source)", frames[0].PTS)
	}
	deadline := time.Now().Add(2 * time.Second)
	for c.Stats().Tracks[0].SSRCResets == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if r := c.Stats().Tracks[0].SSRCResets; r != 1 {
		t.Errorf("SSRCResets = %d, want 1", r)
	}
}

func TestRTPAACSeqGapCarriesToReassembledAU(t *testing.T) {
	var col collector
	c := openOK(t, aacRTPCfg(col.onFrame))
	defer func() { _ = c.Close() }()
	send := senderFor(t, c)

	// A complete AU establishes the stream, then a packet is lost immediately
	// before a fragment START (which completes no access unit). The gap must be
	// carried onto the reassembled AU's frame, not swallowed because the
	// fragment-start packet delivered nothing.
	first := buildAACHBR([][]byte{aacAUHeader16(1, 0)}, [][]byte{{0xA0}})
	sendAndSettle(t, c, send, withMarker(rtpPacket(97, 1, 0, 5, first)))

	full := []byte{1, 2, 3, 4, 5, 6}
	frag1 := buildAACHBR([][]byte{aacAUHeader16(len(full), 0)}, [][]byte{full[:3]})
	frag2 := buildAACHBR([][]byte{aacAUHeader16(len(full), 0)}, [][]byte{full[3:]})
	sendAndSettle(t, c, send, rtpPacket(97, 3, 1024, 5, frag1))             // seq 2 lost; fragment start, no frame
	sendAndSettle(t, c, send, withMarker(rtpPacket(97, 4, 1024, 5, frag2))) // completes the AU
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if len(frames) != 2 {
		t.Fatalf("delivered %d frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[1].Data, full) {
		t.Errorf("reassembled AU = % x, want % x", frames[1].Data, full)
	}
	if frames[1].SeqGap != 1 {
		t.Errorf("SeqGap = %d, want 1 (the loss before the fragment start must carry onto this AU)", frames[1].SeqGap)
	}
}

func TestRTPAACReorderDeliversInSequence(t *testing.T) {
	var col collector
	cfg := aacRTPCfg(col.onFrame)
	cfg.Reorder = true
	c := openOK(t, cfg)
	defer func() { _ = c.Close() }()
	send := senderFor(t, c)

	one := func(seq uint16, b byte) []byte {
		return withMarker(rtpPacket(97, seq, 1000, 5, buildAACHBR([][]byte{aacAUHeader16(1, 0)}, [][]byte{{b}})))
	}
	// Deliver out of order; the resequencer feeds processRTP in ascending order,
	// so the depacketizer sees seq 1,2,3 and the access units emerge in order.
	sendAndSettle(t, c, send, one(1, 0x01))
	sendAndSettle(t, c, send, one(3, 0x03)) // early, buffered
	sendAndSettle(t, c, send, one(2, 0x02)) // fills the hole, releases 2 then 3
	waitCount(t, &col, 3, 2*time.Second)

	if got := deliveredSeqMarkers(col.snapshot()); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("recovered AAC delivery order = %v, want [1 2 3]", got)
	}
	if ts := c.Stats().Tracks[0]; ts.Duplicates != 0 || ts.SeqGaps != 0 {
		t.Errorf("Duplicates=%d SeqGaps=%d, want 0/0 (the hole was filled)", ts.Duplicates, ts.SeqGaps)
	}
}

func TestRTPAACSamplesPerFrameDefaults(t *testing.T) {
	// Leaving SamplesPerFrame zero must default to 1024 (AAC-LC), not merely let
	// Open succeed: a two-AU packet must space AU1 by exactly 1024 ticks, which
	// fails if the default is any other value.
	var col collector
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 97, Codec: audiostream.CodecAAC{}, ClockRate: 44100,
		AAC:     AACParams{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3}, // SamplesPerFrame omitted -> 1024
		OnFrame: col.onFrame,
	})
	defer func() { _ = c.Close() }()

	d0, d1 := []byte{0x11, 0x22}, []byte{0x33, 0x44}
	pkt := buildAACHBR([][]byte{aacAUHeader16(len(d0), 0), aacAUHeader16(len(d1), 0)}, [][]byte{d0, d1})
	_, _ = senderFor(t, c).Write(withMarker(rtpPacket(97, 1, 0, 5, pkt)))
	waitCount(t, &col, 2, 2*time.Second)

	frames := col.snapshot()
	if want := mediatime.PTSFromSamples(1024, 44100); frames[1].PTS != want {
		t.Errorf("AU1 PTS = %v, want %v (default SamplesPerFrame must be 1024)", frames[1].PTS, want)
	}
}

func TestRTPAACExplicitCodecASCWinsOverParams(t *testing.T) {
	// The doc contract: an AudioSpecificConfig set on the CodecAAC value itself
	// takes precedence over AACParams.AudioSpecificConfig, which is only the
	// fallback. Set both, differently, and assert the codec value wins.
	codecASC := []byte{0x11, 0x90}
	paramsASC := []byte{0x22, 0x88}
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 97, Codec: audiostream.CodecAAC{AudioSpecificConfig: codecASC}, ClockRate: 44100,
		AAC: AACParams{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024, AudioSpecificConfig: paramsASC},
	})
	defer func() { _ = c.Close() }()

	got, ok := c.Format().Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("Format().Codec = %T, want audiostream.CodecAAC", c.Format().Codec)
	}
	if !bytes.Equal(got.AudioSpecificConfig, codecASC) {
		t.Errorf("reported ASC = % x, want the CodecAAC value's % x (not the AACParams fallback)", got.AudioSpecificConfig, codecASC)
	}
}

func TestRTPAACFormatReportsASC(t *testing.T) {
	asc := []byte{0x12, 0x10}
	c := openOK(t, Config{
		Mode: ModeRTP, PayloadType: 97, Codec: audiostream.CodecAAC{}, ClockRate: 44100,
		AAC: AACParams{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024, AudioSpecificConfig: asc},
	})
	defer func() { _ = c.Close() }()

	f := c.Format()
	got, ok := f.Codec.(audiostream.CodecAAC)
	if !ok {
		t.Fatalf("Format().Codec = %T, want audiostream.CodecAAC", f.Codec)
	}
	if !bytes.Equal(got.AudioSpecificConfig, asc) {
		t.Errorf("reported ASC = % x, want % x", got.AudioSpecificConfig, asc)
	}
	if f.Kind != audiostream.PayloadKindFor(audiostream.CodecAAC{}) {
		t.Errorf("Kind = %v, want the CodecAAC payload kind", f.Kind)
	}
	// A compressed codec reports no PCM geometry; sample rate and channels come
	// from the ASC, read by a downstream decoder, not from this descriptor.
	if f.SampleRate != 0 || f.Channels != 0 {
		t.Errorf("SampleRate=%d Channels=%d, want 0/0 for a compressed codec", f.SampleRate, f.Channels)
	}
}
