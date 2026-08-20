package rtsp

import (
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// The single-frame delivery path (deliverOpus/deliverG711/deliverL16/deliverRaw
// funnelling through deliverOne) is treated as zero-allocation in steady state:
// pcmBuf is reused across packets and the L16 byte-swap is a manual loop chosen
// because encoding/binary benchmarked slower. Nothing enforced that contract, so
// an edit that made frame delivery allocate would pass CI silently. These
// benchmarks report allocs for humans, and the paired TestDeliverZeroAlloc turns
// the 0-allocs/op contract into a CI-enforceable assertion via AllocsPerRun.

// Codec case names, kept as constants so goconst does not count the shared
// literals against the occurrences already present in pipeline_test.go.
const (
	caseOpus = "opus"
	caseG711 = "g711"
	caseL16  = "l16"
	caseRaw  = "raw"
)

// singleFrameCase is one codec's delivery setup: a freshly built track and a
// valid packet whose payload depacketizes cleanly with no per-packet allocation
// in steady state.
type singleFrameCase struct {
	name string
	tr   *track
	pkt  rtp.Packet
}

// bytePattern returns an n-byte slice with a non-trivial repeating pattern, so a
// payload is neither all-zero nor empty (an empty Opus payload is malformed and
// would take the no-frame path instead of the delivery path under test).
func bytePattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 1)
	}
	return b
}

// singleFrameCases builds one delivery case per single-frame codec. Each track is
// marked baseSet so deliver takes the steady-state path rather than establishing
// a baseline on the first packet.
func singleFrameCases() []singleFrameCase {
	newTrackFor := func(kind codecKind, clockRate uint64, law audiostream.Law) *track {
		tr := &track{kind: kind, clockRate: clockRate, law: law}
		tr.baseSet.Store(true)
		return tr
	}
	l16 := newTrackFor(deliverL16, 48000, 0)
	l16.l16FrameSize = 2

	ts := uint32(480)
	return []singleFrameCase{
		{
			name: caseOpus,
			tr:   newTrackFor(deliverOpus, 48000, 0),
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: bytePattern(120)},
		},
		{
			name: caseG711,
			tr:   newTrackFor(deliverG711, 8000, audiostream.MuLaw),
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: bytePattern(160)},
		},
		{
			name: caseL16,
			tr:   l16,
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: bytePattern(160)},
		},
		{
			name: caseRaw,
			tr:   newTrackFor(deliverRaw, 90000, 0),
			pkt:  rtp.Packet{Header: rtp.Header{Timestamp: ts}, Payload: bytePattern(120)},
		},
	}
}

// BenchmarkDeliver measures the per-packet single-frame delivery path under a
// non-nil no-op callback. Run: go test -bench=BenchmarkDeliver -benchmem -run='^$' ./rtsp/
func BenchmarkDeliver(b *testing.B) {
	now := time.Now()
	up := rtp.Update{Timestamp: 480, Gap: 0}
	noop := func(audiostream.Frame) {}
	for _, tc := range singleFrameCases() {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				tc.tr.deliver(tc.pkt, up, now, noop)
			}
		})
	}
}

// TestDeliverZeroAlloc enforces the 0-allocs/op steady-state contract on the
// single-frame delivery path. AllocsPerRun runs the function once to warm up (so
// the one-time pcmBuf growth is amortized, not counted) and then averages the
// allocations over the measured runs.
func TestDeliverZeroAlloc(t *testing.T) {
	now := time.Now()
	up := rtp.Update{Timestamp: 480, Gap: 0}
	noop := func(audiostream.Frame) {}
	for _, tc := range singleFrameCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := testing.AllocsPerRun(1000, func() {
				tc.tr.deliver(tc.pkt, up, now, noop)
			})
			if got != 0 {
				t.Errorf("%s delivery allocated %v allocs/op in steady state, want 0", tc.name, got)
			}
		})
	}
}
