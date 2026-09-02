package doctor

import (
	"context"
	"time"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// fakeProber is a scripted Prober for the doctor tests. Each lifecycle
// method returns its scripted error (nil by default); Collect returns the
// scripted CaptureResult. It records the call order so tests can assert the
// walkthrough, and every Setup call so tests can assert the audio-only vs
// full-stream pattern.
type fakeProber struct {
	dialErr, describeErr, setupErr, playErr error
	tracks                                  []rtsp.Track
	session                                 rtsp.SessionInfo
	result                                  CaptureResult
	collectErr                              error
	calls                                   []string
	setups                                  []setupCall // one per Setup, in call order
}

// setupCall records one Setup invocation for assertion.
type setupCall struct {
	TrackID     int
	Discard     bool
	G726Packing rtsp.G726PackingOverride
}

// compile-time: fakeProber implements RTSPProber (and thus the narrower
// Prober), so Run's type switch drives it through the RTSP step list.
var _ RTSPProber = (*fakeProber)(nil)

func (f *fakeProber) Dial(_ context.Context) error {
	f.calls = append(f.calls, "Dial")
	return f.dialErr
}

func (f *fakeProber) Describe(_ context.Context) ([]rtsp.Track, error) {
	f.calls = append(f.calls, "Describe")
	return f.tracks, f.describeErr
}

func (f *fakeProber) Setup(_ context.Context, track rtsp.Track, opts rtsp.SetupOptions) error {
	f.calls = append(f.calls, "Setup")
	f.setups = append(f.setups, setupCall{TrackID: track.ID, Discard: opts.Discard, G726Packing: opts.G726Packing})
	return f.setupErr
}

func (f *fakeProber) Play(_ context.Context) error {
	f.calls = append(f.calls, "Play")
	return f.playErr
}

func (f *fakeProber) Collect(_ context.Context, _ rtsp.Track, _ time.Duration) (CaptureResult, error) {
	f.calls = append(f.calls, "Collect")
	return f.result, f.collectErr
}

func (f *fakeProber) SessionInfo() rtsp.SessionInfo {
	return f.session
}

func (f *fakeProber) Close() error {
	f.calls = append(f.calls, "Close")
	return nil
}
