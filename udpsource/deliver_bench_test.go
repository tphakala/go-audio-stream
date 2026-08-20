package udpsource

import (
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/depacket/aac"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// These benchmarks and the paired zero-alloc test cover udpsource's per-packet
// delivery path (deliverRTP for the single-frame codecs, deliverAAC for AAC),
// the counterpart to the rtsp BenchmarkDeliver in rtsp/pipeline_bench_test.go.
// The opaque/Opus passthrough and the reused pcmBuf for G.711/L16 are meant to be
// zero-allocation in steady state, and TestDeliverRTPZeroAlloc turns that into a
// CI-enforceable assertion. It also guards the issue #107 fix: the added
// pendingGap fold and drain are scalar operations that must not allocate.
//
// This addresses the benchmark item noted in issue #90 for the udpsource raw
// path; the remaining #90 items (RTCP / sender clock) are out of scope here, so
// #90 stays open.

// deliverBenchCase is one codec's delivery setup: a Client wired for that kind
// and a packet whose payload depacketizes cleanly with no per-packet allocation
// in steady state.
type deliverBenchCase struct {
	name string
	c    *Client
	pkt  rtp.Packet
	up   rtp.Update
}

// udpBytePattern returns an n-byte slice with a non-trivial repeating pattern, so
// a payload is neither all-zero nor empty (an empty Opus payload is malformed and
// would take the no-frame path instead of the delivery path under test).
func udpBytePattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 1)
	}
	return b
}

// deliverBenchCases builds one delivery case per codec. tb is used only to fail
// on AAC depacketizer construction; nothing here binds a socket.
func deliverBenchCases(tb testing.TB) []deliverBenchCase {
	tb.Helper()
	noop := func(audiostream.Frame) {}
	up := rtp.Update{Timestamp: 480, Gap: 0}

	// AAC: build the depacketizer exactly as resolveFormat does for a CodecAAC
	// source, and a single complete AU packet (Marker set) so deliverAAC emits one
	// frame per call.
	dp, err := aac.New(aac.Config{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024})
	if err != nil {
		tb.Fatalf("aac.New: %v", err)
	}
	aacData := udpBytePattern(160)
	aacPayload := buildAACHBR([][]byte{aacAUHeader16(len(aacData), 0)}, [][]byte{aacData})

	return []deliverBenchCase{
		{
			name: "opaque",
			c:    &Client{kind: kindOpaque, cfg: Config{ClockRate: 90000, OnFrame: noop}},
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: 480}, Payload: udpBytePattern(120)},
			up:   up,
		},
		{
			name: "opus",
			c:    &Client{kind: kindOpus, cfg: Config{ClockRate: 48000, OnFrame: noop}},
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: 480}, Payload: udpBytePattern(120)},
			up:   up,
		},
		{
			name: "g711",
			c:    &Client{kind: kindG711, law: audiostream.MuLaw, cfg: Config{ClockRate: 8000, OnFrame: noop}},
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: 480}, Payload: udpBytePattern(160)},
			up:   up,
		},
		{
			name: "l16",
			c:    &Client{kind: kindL16, frameBytes: 2, cfg: Config{ClockRate: 44100, OnFrame: noop}},
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: 480}, Payload: udpBytePattern(160)},
			up:   up,
		},
		{
			name: "aac",
			c:    &Client{kind: kindAAC, aac: dp, cfg: Config{ClockRate: 44100, OnFrame: noop}},
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: 480, Marker: true}, Payload: aacPayload},
			up:   up,
		},
	}
}

// BenchmarkDeliverRTP measures the per-packet delivery path under a non-nil
// no-op callback. Run: go test -bench=BenchmarkDeliverRTP -benchmem -run='^$' ./udpsource/
func BenchmarkDeliverRTP(b *testing.B) {
	now := time.Now()
	for _, tc := range deliverBenchCases(b) {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tc.c.deliverRTP(tc.pkt, tc.up, now)
			}
		})
	}
}

// TestDeliverRTPZeroAlloc enforces the 0-allocs/op steady-state contract on the
// udpsource delivery path. AllocsPerRun warms up once (amortizing the one-time
// pcmBuf / depacketizer-buffer growth) and then averages the measured runs.
func TestDeliverRTPZeroAlloc(t *testing.T) {
	now := time.Now()
	for _, tc := range deliverBenchCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			got := testing.AllocsPerRun(1000, func() {
				tc.c.deliverRTP(tc.pkt, tc.up, now)
			})
			if got != 0 {
				t.Errorf("%s delivery allocated %v allocs/op in steady state, want 0", tc.name, got)
			}
		})
	}
}
