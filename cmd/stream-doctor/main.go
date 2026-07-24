// Command stream-doctor probes an RTSP camera or NVR audio stream and
// reports whether go-audio-stream can consume it: connectivity, codec
// support, and RFC 3550 packet statistics.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/tphakala/go-audio-stream/cmd/stream-doctor/internal/doctor"
)

func main() {
	os.Exit(run())
}

// run wires SIGINT to context cancellation so a Ctrl-C during capture ends the
// run cleanly, then hands off to the diagnostic engine. It is a function so
// the deferred stop runs before os.Exit.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return doctor.ExecuteContext(ctx, os.Args[1:], os.Stdout, os.Stderr)
}
