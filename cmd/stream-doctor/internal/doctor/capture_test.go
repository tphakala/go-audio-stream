package doctor

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

// TestCollectCompleted asserts the fake Prober's Collect passes its scripted
// CaptureResult through unchanged to the caller.
func TestCollectCompleted(t *testing.T) {
	t.Parallel()
	want := CaptureResult{
		Reason: EndCompleted,
		Frames: []CapturedFrame{
			{Data: []byte("a")},
			{Data: []byte("b")},
			{Data: []byte("c")},
		},
	}
	fp := &fakeProber{result: want}

	got, err := fp.Collect(context.Background(), rtsp.Track{}, time.Second)
	if err != nil {
		t.Fatalf("Collect() error = %v, want nil", err)
	}
	if got.Reason != EndCompleted {
		t.Errorf("Reason = %v, want EndCompleted", got.Reason)
	}
	if len(got.Frames) != len(want.Frames) {
		t.Fatalf("len(Frames) = %d, want %d", len(got.Frames), len(want.Frames))
	}
	for i := range want.Frames {
		if !bytes.Equal(got.Frames[i].Data, want.Frames[i].Data) {
			t.Errorf("Frames[%d].Data = %q, want %q", i, got.Frames[i].Data, want.Frames[i].Data)
		}
	}
}

// TestTransportPreference covers the mapping from the -transport flag value to
// the library preference: the three accepted values map through, an empty string
// defaults to TCP, and anything else is rejected (ok false) so parseArgs turns
// it into a usage error.
func TestTransportPreference(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in     string
		want   rtsp.TransportPreference
		wantOK bool
	}{
		{"", rtsp.PreferTCP, true},
		{transportTCP, rtsp.PreferTCP, true},
		{transportUDP, rtsp.PreferUDP, true},
		{transportUDPThenTCP, rtsp.PreferUDPThenTCP, true},
		{"tls", rtsp.PreferTCP, false},
		{"UDP", rtsp.PreferTCP, false},
	} {
		got, ok := transportPreference(tc.in)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("transportPreference(%q) = (%v, %v), want (%v, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestRTSPProberOnFrameCopies drives the production adapter's OnFrame sink
// directly (white-box: it needs the unexported rtspProber internals) and
// asserts the stored frame is an owned copy, unaffected by a subsequent
// mutation of the caller's buffer, and that frames on a non-audio track are
// ignored.
func TestRTSPProberOnFrameCopies(t *testing.T) {
	t.Parallel()
	p := newRTSPProber(Options{})
	p.audioTrackID.Store(1)

	data := []byte{1, 2, 3}
	p.onFrame(audiostream.Frame{TrackID: 1, Data: data, RTPTime: 42})
	data[0] = 99 // mutate the caller's buffer after delivery

	p.sink.mu.Lock()
	got := append([]CapturedFrame(nil), p.sink.frames...)
	p.sink.mu.Unlock()

	if len(got) != 1 {
		t.Fatalf("len(frames) = %d, want 1", len(got))
	}
	if got[0].Data[0] != 1 {
		t.Errorf("stored Data[0] = %d, want 1 (mutation after delivery must not affect the copy)", got[0].Data[0])
	}

	// A frame on a non-audio track must be ignored.
	p.onFrame(audiostream.Frame{TrackID: 2, Data: []byte{9}})
	p.sink.mu.Lock()
	n := len(p.sink.frames)
	p.sink.mu.Unlock()
	if n != 1 {
		t.Errorf("len(frames) after non-audio frame = %d, want 1 (must be ignored)", n)
	}
}

// TestRTSPProberCapTruncates feeds the sink more frames than a (tiny,
// test-only) override of the frame cap and asserts it stops appending and
// records the truncation.
func TestRTSPProberCapTruncates(t *testing.T) {
	t.Parallel()
	p := &rtspProber{sink: frameSink{maxFrames: 3, maxBytes: maxCaptureBytes}}
	p.audioTrackID.Store(0)

	for range 5 {
		p.onFrame(audiostream.Frame{TrackID: 0, Data: []byte{1}})
	}

	p.sink.mu.Lock()
	n := len(p.sink.frames)
	truncated := p.sink.truncated
	p.sink.mu.Unlock()

	if n != 3 {
		t.Errorf("len(frames) = %d, want 3", n)
	}
	if !truncated {
		t.Error("truncated = false, want true")
	}
}

// TestRTSPProberByteCapLatches feeds a frame that overflows a tiny byte cap,
// then a smaller frame that would fit the remaining budget, and asserts the
// smaller frame is dropped: once the cap is hit, truncation latches so the
// capture stays a contiguous prefix rather than growing a hole.
func TestRTSPProberByteCapLatches(t *testing.T) {
	t.Parallel()
	p := &rtspProber{sink: frameSink{maxFrames: 1000, maxBytes: 10}}
	p.audioTrackID.Store(0)

	p.onFrame(audiostream.Frame{TrackID: 0, Data: []byte{1, 2, 3, 4, 5, 6}}) // 6 bytes: fits (0+6 <= 10)
	p.onFrame(audiostream.Frame{TrackID: 0, Data: []byte{7, 8, 9, 10, 11}})  // 5 bytes: 6+5 > 10, truncates
	p.onFrame(audiostream.Frame{TrackID: 0, Data: []byte{12}})               // 1 byte: would fit, but must drop

	p.sink.mu.Lock()
	n := len(p.sink.frames)
	bytesAcc := p.sink.bytes
	truncated := p.sink.truncated
	p.sink.mu.Unlock()

	if n != 1 {
		t.Errorf("len(frames) = %d, want 1 (truncation must latch; a later small frame must not fill the gap)", n)
	}
	if bytesAcc != 6 {
		t.Errorf("bytes = %d, want 6", bytesAcc)
	}
	if !truncated {
		t.Error("truncated = false, want true")
	}
}

// TestClassifyEndReason pins the precedence of the terminal-reason mapping: a
// parent-context cancellation beats the capture-window deadline, the
// !truncated guard separates a clean window from a truncated one, and each
// sentinel error maps to its own reason.
func TestClassifyEndReason(t *testing.T) {
	t.Parallel()

	otherErr := errors.New("some transport failure")

	tests := []struct {
		name       string
		cancel     bool
		err        error
		truncated  bool
		frameCount int
		want       EndReason
	}{
		{"parent cancel beats window deadline", true, context.DeadlineExceeded, false, 10, EndCancelled},
		{"window elapsed cleanly", false, context.DeadlineExceeded, false, 10, EndCompleted},
		{"window elapsed but truncated", false, context.DeadlineExceeded, true, 10, EndTruncated},
		{"truncated without a deadline", false, nil, true, 5, EndTruncated},
		{"read-idle watchdog", false, audiostream.ErrReadTimeout, false, 3, EndWatchdog},
		{"server sent teardown", false, rtsp.ErrServerTeardown, false, 3, EndTeardown},
		{"connection closed", false, rtsp.ErrConnectionClosed, false, 3, EndDisconnect},
		{"unknown error, nothing captured", false, otherErr, false, 0, EndDisconnect},
		{"unknown error with frames", false, otherErr, false, 7, EndCompleted},
		{"clean end with frames", false, nil, false, 7, EndCompleted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if got := classifyEndReason(ctx, tt.err, tt.truncated, tt.frameCount); got != tt.want {
				t.Errorf("classifyEndReason() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRTSPProberDropsTrackZeroBeforeSetup asserts a freshly constructed prober
// does not capture track 0 as audio before Setup records the real audio track:
// newRTSPProber seeds audioTrackID with an impossible ID for exactly this.
func TestRTSPProberDropsTrackZeroBeforeSetup(t *testing.T) {
	t.Parallel()
	p := newRTSPProber(Options{})
	p.onFrame(audiostream.Frame{TrackID: 0, Data: []byte{1, 2, 3}})
	p.sink.mu.Lock()
	n := len(p.sink.frames)
	p.sink.mu.Unlock()
	if n != 0 {
		t.Errorf("len(frames) = %d, want 0 (track 0 must not be captured before Setup)", n)
	}
}

// TestRTSPProberCloseAndSessionInfoBeforeDial asserts the two methods
// documented as usable outside the Dial->...->Close sequence do not panic when
// the client has not been created yet.
func TestRTSPProberCloseAndSessionInfoBeforeDial(t *testing.T) {
	t.Parallel()
	p := newRTSPProber(Options{})
	if err := p.Close(); err != nil {
		t.Errorf("Close() before Dial = %v, want nil", err)
	}
	if got := p.SessionInfo(); got.SessionID != "" || got.KeepaliveMethod != "" || got.Channels != nil {
		t.Errorf("SessionInfo() before Dial = %+v, want zero", got)
	}
}
