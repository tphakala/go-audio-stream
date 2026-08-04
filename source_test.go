package audiostream_test

import (
	"context"
	"errors"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
)

// fakeSource is a minimal, rtsp-free implementation of audiostream.Source. It
// exists to prove the contract is satisfiable outside the rtsp package without
// importing it, which also demonstrates the interface introduces no import
// cycle: the root package defines Source and nothing here reaches back into a
// protocol client.
type fakeSource struct {
	waitErr error
	stats   audiostream.Stats
	info    audiostream.SourceInfo
}

// Compile-time: the fake satisfies the sealed capture contract, the same way
// audiostream_test pins the codec variants against Codec.
var _ audiostream.Source = (*fakeSource)(nil)

func (f *fakeSource) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return f.waitErr
	}
}

func (f *fakeSource) Close() error { return nil }

func (f *fakeSource) Stats() audiostream.Stats { return f.stats }

func (f *fakeSource) Info() audiostream.SourceInfo { return f.info }

// TestSourceContractImplementable exercises a fakeSource through the Source
// interface, so the assertion above is backed by a real call path rather than
// a bare type check that a future signature drift could leave green.
func TestSourceContractImplementable(t *testing.T) {
	t.Parallel()
	var src audiostream.Source = &fakeSource{
		waitErr: audiostream.ErrClosed,
		stats:   audiostream.Stats{Tracks: map[int]audiostream.TrackStats{0: {Packets: 3}}},
		info:    audiostream.SourceInfo{URL: "rtsp://host/stream", Server: "TestCam/1.0"},
	}

	if err := src.Wait(context.Background()); !errors.Is(err, audiostream.ErrClosed) {
		t.Errorf("Wait = %v, want ErrClosed", err)
	}
	if err := src.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if got := src.Stats().Tracks[0].Packets; got != 3 {
		t.Errorf("Stats track 0 Packets = %d, want 3", got)
	}
	info := src.Info()
	if info.URL != "rtsp://host/stream" {
		t.Errorf("Info().URL = %q, want rtsp://host/stream", info.URL)
	}
	if info.Server != "TestCam/1.0" {
		t.Errorf("Info().Server = %q, want TestCam/1.0", info.Server)
	}
}

// TestSourceWaitHonorsContext confirms the contract's promise that Wait returns
// the context error when the caller's context cancels first.
func TestSourceWaitHonorsContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var src audiostream.Source = &fakeSource{waitErr: audiostream.ErrClosed}
	if err := src.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait with cancelled ctx = %v, want context.Canceled", err)
	}
}
