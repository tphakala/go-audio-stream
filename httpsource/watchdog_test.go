package httpsource

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	audiostream "github.com/tphakala/go-audio-stream"
)

func TestWatchdogFiresOnIdle(t *testing.T) {
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	// Send the header plus one frame, then park: no further data arrives, so
	// the read-idle watchdog must fire.
	release := make(chan struct{})
	defer close(release)
	body := slices.Concat(header, pcmMono(80))
	srv := httptest.NewServer(serveThenPark("audio/wav", body, release))
	defer srv.Close()

	c := openOK(t, srv, Config{ReadIdle: 50 * time.Millisecond})
	if err := waitResult(t, c, 5*time.Second); !errors.Is(err, audiostream.ErrReadTimeout) {
		t.Fatalf("Wait = %v, want ErrReadTimeout", err)
	}
}

func TestWatchdogNotTrippedBySteadyDrips(t *testing.T) {
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	frame := pcmMono(40)
	// Drip a frame every 30ms for a while, comfortably under the 250ms window,
	// then end cleanly. The watchdog must never fire; Wait ends with ErrStreamEnded.
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(header)
		flush(w)
		for range 6 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Millisecond):
			}
			if _, err := w.Write(frame); err != nil {
				return
			}
			flush(w)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	c := openOK(t, srv, Config{ReadIdle: 250 * time.Millisecond})
	if err := waitResult(t, c, 10*time.Second); !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("Wait = %v, want ErrStreamEnded (watchdog should not fire)", err)
	}
}

func TestReadIdleDisabledByDefault(t *testing.T) {
	// With ReadIdle zero the watchdog never arms; a parked stream stays open
	// until Close. This mainly guards against a zero window arming a 0ns timer.
	header := stdWAVHeader(wavFormatPCM, 1, 8000, 16, wavUnbounded, wavUnbounded)
	release := make(chan struct{})
	defer close(release)
	srv := httptest.NewServer(serveThenPark("audio/wav", header, release))
	defer srv.Close()

	c := openOK(t, srv, Config{})
	got := make(chan error, 1)
	go func() { got <- c.Wait(context.Background()) }()
	select {
	case err := <-got:
		t.Fatalf("Wait returned %v while the stream was parked and never closed", err)
	case <-time.After(200 * time.Millisecond):
	}
	_ = c.Close()
	if err := <-got; !errors.Is(err, audiostream.ErrClosed) {
		t.Fatalf("Wait = %v, want ErrClosed", err)
	}
}
