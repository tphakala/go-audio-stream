package rtsp_test

import (
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp"
)

// buildTransportUDPWant is the client SETUP proposal BuildTransportUDP(5000,
// 5001) is expected to produce; it also appears as a ParseTransport input
// case below, so both directions of the round trip are exercised against
// the same literal.
const buildTransportUDPWant = "RTP/AVP;unicast;client_port=5000-5001"

func TestParseTransportUDP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		value           string
		wantClientRTP   int
		wantClientRTCP  int
		wantHasClient   bool
		wantServerRTP   int
		wantServerRTCP  int
		wantHasServer   bool
		wantUnicast     bool
		wantSSRC        string
		wantServerPorts bool
	}{
		{
			name:            "client and server ports with ssrc",
			value:           "RTP/AVP;unicast;client_port=5000-5001;server_port=6000-6001;ssrc=1a2b3c4d",
			wantClientRTP:   5000,
			wantClientRTCP:  5001,
			wantHasClient:   true,
			wantServerRTP:   6000,
			wantServerRTCP:  6001,
			wantHasServer:   true,
			wantUnicast:     true,
			wantSSRC:        "1a2b3c4d",
			wantServerPorts: true,
		},
		{
			name:            "client port only, no server_port",
			value:           buildTransportUDPWant,
			wantClientRTP:   5000,
			wantClientRTCP:  5001,
			wantHasClient:   true,
			wantHasServer:   false,
			wantUnicast:     true,
			wantServerPorts: false,
		},
		{
			name:            "non-consecutive server_port",
			value:           "RTP/AVP;unicast;server_port=6000-6003",
			wantServerRTP:   6000,
			wantServerRTCP:  6003,
			wantHasServer:   true,
			wantUnicast:     true,
			wantServerPorts: false,
		},
		{
			name:            "out-of-range server_port",
			value:           "RTP/AVP;unicast;server_port=70000-70001",
			wantServerRTP:   70000,
			wantServerRTCP:  70001,
			wantHasServer:   true,
			wantUnicast:     true,
			wantServerPorts: false,
		},
		{
			name:            "phase 1 interleaved header carries no UDP fields",
			value:           "RTP/AVP/TCP;unicast;interleaved=0-1",
			wantHasClient:   false,
			wantHasServer:   false,
			wantUnicast:     true,
			wantServerPorts: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			th, err := rtsp.ParseTransport(tt.value)
			if err != nil {
				t.Fatalf("ParseTransport(%q) error = %v", tt.value, err)
			}
			if th.ClientRTPPort != tt.wantClientRTP || th.ClientRTCPPort != tt.wantClientRTCP {
				t.Errorf("client ports = %d/%d, want %d/%d", th.ClientRTPPort, th.ClientRTCPPort, tt.wantClientRTP, tt.wantClientRTCP)
			}
			if th.HasClientPort != tt.wantHasClient {
				t.Errorf("HasClientPort = %v, want %v", th.HasClientPort, tt.wantHasClient)
			}
			if th.ServerRTPPort != tt.wantServerRTP || th.ServerRTCPPort != tt.wantServerRTCP {
				t.Errorf("server ports = %d/%d, want %d/%d", th.ServerRTPPort, th.ServerRTCPPort, tt.wantServerRTP, tt.wantServerRTCP)
			}
			if th.HasServerPort != tt.wantHasServer {
				t.Errorf("HasServerPort = %v, want %v", th.HasServerPort, tt.wantHasServer)
			}
			if th.Unicast != tt.wantUnicast {
				t.Errorf("Unicast = %v, want %v", th.Unicast, tt.wantUnicast)
			}
			if th.SSRC != tt.wantSSRC {
				t.Errorf("SSRC = %q, want %q", th.SSRC, tt.wantSSRC)
			}

			rtp, rtcp, ok := th.ServerPorts()
			if ok != tt.wantServerPorts {
				t.Errorf("ServerPorts() ok = %v, want %v", ok, tt.wantServerPorts)
			}
			if ok {
				if rtp != tt.wantServerRTP || rtcp != tt.wantServerRTCP {
					t.Errorf("ServerPorts() = %d/%d, want %d/%d", rtp, rtcp, tt.wantServerRTP, tt.wantServerRTCP)
				}
			} else if rtp != 0 || rtcp != 0 {
				t.Errorf("ServerPorts() = %d/%d, want 0/0 on failure", rtp, rtcp)
			}
		})
	}
}

func TestBuildTransportUDP(t *testing.T) {
	t.Parallel()
	got := rtsp.BuildTransportUDP(5000, 5001)
	if got != buildTransportUDPWant {
		t.Fatalf("BuildTransportUDP(5000, 5001) = %q, want %q", got, buildTransportUDPWant)
	}
}
