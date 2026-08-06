package rtsp_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// The zero-value Config.Transport must be PreferTCP, so the phase 1
// TCP-interleaved behavior is unchanged for every caller that has not
// opted into UDP.
func TestConfigTransportZeroValueIsPreferTCP(t *testing.T) {
	t.Parallel()
	var cfg rtsp.Config
	if cfg.Transport != rtsp.PreferTCP {
		t.Errorf("zero-value Config.Transport = %v, want PreferTCP", cfg.Transport)
	}
}

// The three TransportPreference constants must be distinct and ordered as
// declared: PreferTCP (the zero value) first, then PreferUDP, then
// PreferUDPThenTCP.
func TestTransportPreferenceConstantsOrdered(t *testing.T) {
	t.Parallel()
	if rtsp.PreferTCP != 0 {
		t.Errorf("PreferTCP = %d, want 0", rtsp.PreferTCP)
	}
	if rtsp.PreferUDP <= rtsp.PreferTCP {
		t.Errorf("PreferUDP = %d, want greater than PreferTCP (%d)", rtsp.PreferUDP, rtsp.PreferTCP)
	}
	if rtsp.PreferUDPThenTCP <= rtsp.PreferUDP {
		t.Errorf("PreferUDPThenTCP = %d, want greater than PreferUDP (%d)", rtsp.PreferUDPThenTCP, rtsp.PreferUDP)
	}
}
