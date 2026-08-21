package doctor

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// progressInterval is how often the live capture meter redraws. One second
// matches the resolution of the elapsed clock it shows and keeps the terminal
// write rate negligible.
const progressInterval = time.Second

// startProgressMeter launches a live capture progress meter and returns a stop
// function. The meter redraws an in-place line on errOut every second for the
// duration of the capture window, showing elapsed time against the window and
// the running packet, loss, and malformed counters, so a multi-second capture
// no longer looks like the tool has hung.
//
// It draws only when a human is watching: the prober must expose progress (the
// captureProgressReporter capability) and errOut must be an interactive
// terminal. Otherwise it returns a no-op stop and writes nothing, so piped
// output and the golden-output tests (which pass a plain buffer) stay
// byte-identical.
//
// The returned stop function halts the ticker, waits for the meter goroutine to
// exit, and clears the meter line, so nothing the final render writes can
// interleave with a half-drawn meter. It is safe to call exactly once.
func (r *runner) startProgressMeter() func() {
	reporter, ok := r.prober.(captureProgressReporter)
	if !ok || !isTerminal(r.errOut) {
		return func() {}
	}

	start := r.now()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		// Draw once up front so the line appears the instant the handshake
		// ends, rather than after the first tick.
		r.drawProgress(start, reporter.Progress())
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				r.drawProgress(start, reporter.Progress())
			}
		}
	})

	return func() {
		close(done)
		wg.Wait()
		r.clearProgress()
	}
}

// drawProgress writes one in-place meter line to errOut. The leading \r returns
// the cursor to column 0 and \033[K clears to end of line, so a shorter line
// never leaves stale characters from a longer previous draw. Elapsed is rounded
// to whole seconds to match the once-per-second redraw.
func (r *runner) drawProgress(start time.Time, p CaptureProgress) {
	elapsed := r.now().Sub(start).Round(time.Second)
	_, _ = fmt.Fprintf(r.errOut, "\r\033[K  capturing  %s / %s   %d packets   %d lost   %d malformed",
		elapsed, r.opts.Duration, p.Packets, p.Lost, p.Malformed)
}

// clearProgress erases the meter line and leaves the cursor at column 0 of an
// empty line, so the final render starts clean with no blank line and no meter
// remnant.
func (r *runner) clearProgress() {
	_, _ = fmt.Fprint(r.errOut, "\r\033[K")
}

// isTerminal reports whether w is a character device, the proxy for "a human is
// watching an interactive terminal". A non-terminal writer (a pipe, a regular
// file, a test buffer) returns false, which suppresses the live meter so piped
// output and golden tests stay byte-stable. It uses only the standard library,
// keeping the tool free of runtime dependencies.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
