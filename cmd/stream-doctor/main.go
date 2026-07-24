// Command stream-doctor probes an RTSP camera or NVR audio stream and
// reports whether go-audio-stream can consume it: connectivity, codec
// support, and RFC 3550 packet statistics.
package main

import (
	"os"

	"github.com/tphakala/go-audio-stream/cmd/stream-doctor/internal/doctor"
)

func main() {
	os.Exit(doctor.Execute(os.Args[1:], os.Stdout, os.Stderr))
}
