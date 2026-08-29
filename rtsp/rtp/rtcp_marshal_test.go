package rtp

import (
	"testing"
	"time"
)

func TestSenderReportMarshalRoundTrip(t *testing.T) {
	tests := []SenderReport{
		{SSRC: 0x11223344, NTPTimestamp: 0xdddddddd11111111, RTPTimestamp: 960, PacketCount: 5, OctetCount: 1600},
		{SSRC: 1, NTPTimestamp: 1, RTPTimestamp: 0xffffffff, PacketCount: 0, OctetCount: 0},
		{SSRC: 0xffffffff, NTPTimestamp: 0x8000000000000000, RTPTimestamp: 42, PacketCount: 1<<31 + 1, OctetCount: 99},
	}
	for _, want := range tests {
		raw := want.Marshal()
		if len(raw) != 28 {
			t.Fatalf("Marshal len = %d, want 28", len(raw))
		}
		got, err := ParseCompound(raw)
		if err != nil {
			t.Fatalf("ParseCompound: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("ParseCompound returned %d reports, want 1", len(got))
		}
		if got[0] != want {
			t.Errorf("round-trip:\n got %+v\nwant %+v", got[0], want)
		}
	}
}

func TestNTPFromTimeRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		t    time.Time
	}{
		{"epoch", time.Unix(0, 0)},
		{"current", time.Unix(1756454400, 123456789)},
		{"era1-post-2036", time.Unix(2500000000, 500000000)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := NTPFromTime(tt.t)
			if ts == 0 {
				t.Fatal("NTPFromTime produced an all-zero timestamp (reserved for 'no clock')")
			}
			got := NTPTime(ts)
			diff := got.UnixNano() - tt.t.UnixNano()
			if diff < -1 || diff > 1 {
				t.Errorf("round-trip diff = %d ns (want within 1 ns): got %v, want %v", diff, got, tt.t)
			}
		})
	}
}

func TestSenderReportMarshalNoExtraAlloc(t *testing.T) {
	sr := SenderReport{SSRC: 7, NTPTimestamp: NTPFromTime(time.Unix(1756454400, 0)), RTPTimestamp: 100}
	allocs := testing.AllocsPerRun(200, func() {
		_ = sr.Marshal()
	})
	// One allocation for the returned 28-byte slice; nothing more.
	if allocs > 1 {
		t.Errorf("Marshal allocs/op = %v, want <= 1", allocs)
	}
}
