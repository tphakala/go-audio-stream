package audiostream_test

import (
	"errors"
	"fmt"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
)

// Compile-time: every codec variant satisfies the sealed Codec interface.
var (
	_ audiostream.Codec = audiostream.CodecAAC{}
	_ audiostream.Codec = audiostream.CodecOpus{}
	_ audiostream.Codec = audiostream.CodecG711{}
	_ audiostream.Codec = audiostream.CodecUnknown{}
)

func TestMediaKindString(t *testing.T) {
	t.Parallel()
	cases := map[audiostream.MediaKind]string{
		audiostream.MediaUnknown:  "unknown",
		audiostream.MediaAudio:    "audio",
		audiostream.MediaVideo:    "video",
		audiostream.MediaOther:    "other",
		audiostream.MediaKind(99): "unknown",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("MediaKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}

func TestLawString(t *testing.T) {
	t.Parallel()
	if got := audiostream.MuLaw.String(); got != "mu-law" {
		t.Errorf("MuLaw.String() = %q, want %q", got, "mu-law")
	}
	if got := audiostream.ALaw.String(); got != "a-law" {
		t.Errorf("ALaw.String() = %q, want %q", got, "a-law")
	}
}

func TestSentinelErrors(t *testing.T) {
	t.Parallel()
	wrapped := fmt.Errorf("op failed: %w", audiostream.ErrClosed)
	if !errors.Is(wrapped, audiostream.ErrClosed) {
		t.Error("ErrClosed must be recoverable from a wrapped error with errors.Is")
	}
	if audiostream.ErrClosed.Error() == "" || audiostream.ErrReadTimeout.Error() == "" {
		t.Error("sentinel errors must carry messages")
	}
}

func TestRedirectError(t *testing.T) {
	t.Parallel()
	var redirect *audiostream.RedirectError
	err := error(&audiostream.RedirectError{Location: "rtsp://other/stream"})
	if !errors.As(err, &redirect) {
		t.Fatal("RedirectError must be matchable with errors.As")
	}
	if redirect.Location != "rtsp://other/stream" {
		t.Errorf("Location = %q, want %q", redirect.Location, "rtsp://other/stream")
	}
}
