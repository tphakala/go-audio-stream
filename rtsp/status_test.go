package rtsp_test

import (
	"errors"
	"testing"

	audiostream "github.com/tphakala/go-audio-stream"
	"github.com/tphakala/go-audio-stream/rtsp"
)

const wwwAuthHeader = "WWW-Authenticate"

func TestClassifyStatusSuccess(t *testing.T) {
	t.Parallel()
	for _, code := range []int{rtsp.StatusOK, rtsp.StatusLowOnStorageSpace} {
		resp := &rtsp.Response{StatusCode: code, Header: rtsp.Header{}}
		if err := rtsp.ClassifyStatus(resp); err != nil {
			t.Errorf("ClassifyStatus(%d) = %v, want nil", code, err)
		}
	}
}

func TestClassifyStatusRedirectWithLocation(t *testing.T) {
	t.Parallel()
	resp := &rtsp.Response{StatusCode: rtsp.StatusMovedPermanently, Header: rtsp.Header{}}
	resp.Header.Set("Location", "rtsp://other/stream")

	err := rtsp.ClassifyStatus(resp)
	var re *audiostream.RedirectError
	if !errors.As(err, &re) {
		t.Fatalf("ClassifyStatus(301) = %v, want *audiostream.RedirectError", err)
	}
	if re.Location != "rtsp://other/stream" {
		t.Errorf("Location = %q, want rtsp://other/stream", re.Location)
	}
}

func TestClassifyStatusRedirectWithoutLocation(t *testing.T) {
	t.Parallel()
	resp := &rtsp.Response{StatusCode: rtsp.StatusMovedTemporarily, Header: rtsp.Header{}}

	err := rtsp.ClassifyStatus(resp)
	var re *audiostream.RedirectError
	if !errors.As(err, &re) {
		t.Fatalf("ClassifyStatus(302) = %v, want *audiostream.RedirectError", err)
	}
	if re.Location != "" {
		t.Errorf("Location = %q, want empty", re.Location)
	}
}

func TestClassifyStatusUnauthorized(t *testing.T) {
	t.Parallel()
	resp := &rtsp.Response{StatusCode: rtsp.StatusUnauthorized, Header: rtsp.Header{}}
	resp.Header.Add(wwwAuthHeader, "Basic realm=\"cam\"")
	resp.Header.Add(wwwAuthHeader, "Digest realm=\"cam\", nonce=\"abc\"")

	err := rtsp.ClassifyStatus(resp)
	var ue *rtsp.UnauthorizedError
	if !errors.As(err, &ue) {
		t.Fatalf("ClassifyStatus(401) = %v, want *rtsp.UnauthorizedError", err)
	}
	if len(ue.Challenges) != 2 {
		t.Fatalf("Challenges len = %d, want 2", len(ue.Challenges))
	}
	if ue.Challenges[0] != "Basic realm=\"cam\"" || ue.Challenges[1] != "Digest realm=\"cam\", nonce=\"abc\"" {
		t.Errorf("Challenges = %#v, want the raw values in order", ue.Challenges)
	}
}

func TestClassifyStatusResponseError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		code   int
		reason string
	}{
		{rtsp.StatusSessionNotFound, "Session Not Found"},
		{rtsp.StatusUnsupportedTransport, "Unsupported Transport"},
		{rtsp.StatusKeyManagementFailure, "Key Management Failure"},
		{rtsp.StatusInternalServerError, "Internal Server Error"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			t.Parallel()
			resp := &rtsp.Response{StatusCode: tt.code, Reason: tt.reason, Header: rtsp.Header{}}
			err := rtsp.ClassifyStatus(resp)
			var respErr *rtsp.ResponseError
			if !errors.As(err, &respErr) {
				t.Fatalf("ClassifyStatus(%d) = %v, want *rtsp.ResponseError", tt.code, err)
			}
			if respErr.Code != tt.code {
				t.Errorf("Code = %d, want %d", respErr.Code, tt.code)
			}
			if respErr.Reason != tt.reason {
				t.Errorf("Reason = %q, want %q", respErr.Reason, tt.reason)
			}
		})
	}
}

func TestStatusConstants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"StatusSessionNotFound", rtsp.StatusSessionNotFound, 454},
		{"StatusUnsupportedTransport", rtsp.StatusUnsupportedTransport, 461},
		{"StatusKeyManagementFailure", rtsp.StatusKeyManagementFailure, 463},
		{"StatusOptionNotSupported", rtsp.StatusOptionNotSupported, 551},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestClassifyStatusNilResponse(t *testing.T) {
	t.Parallel()
	// The package promises never to panic. A nil response is a caller
	// mistake, so it has to surface as an error rather than a crash.
	if err := rtsp.ClassifyStatus(nil); !errors.Is(err, rtsp.ErrNilResponse) {
		t.Errorf("ClassifyStatus(nil) = %v, want ErrNilResponse", err)
	}
}
