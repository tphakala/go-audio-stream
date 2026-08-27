package hlssource

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/tphakala/go-audio-stream/internal/mp4"
)

// collectFMP4 demuxes one fMP4 fragment and returns the access units delivered.
func collectFMP4(t *testing.T, d *fmp4Demux, frag []byte) [][]byte {
	t.Helper()
	var got [][]byte
	if err := d.demux(frag, false, func(au []byte, _ time.Duration) {
		got = append(got, append([]byte(nil), au...))
	}); err != nil {
		t.Fatalf("demux: %v", err)
	}
	return got
}

func TestFMP4DemuxResolvesASCFromInit(t *testing.T) {
	// The ASC is known at construction, before any fragment is demuxed, and is
	// byte-identical to the TS path's wantASC (codec parity).
	init := buildInitSegment(wantASC, 44100, 1)
	d, err := newFMP4Demux(init)
	if err != nil {
		t.Fatalf("newFMP4Demux: %v", err)
	}
	if !bytes.Equal(d.audioSpecificConfig(), wantASC) {
		t.Errorf("ASC = %x, want %x", d.audioSpecificConfig(), wantASC)
	}
}

func TestFMP4DemuxDeliversSamplesAndDurations(t *testing.T) {
	init := buildInitSegment(wantASC, 44100, 1)
	d, err := newFMP4Demux(init)
	if err != nil {
		t.Fatalf("newFMP4Demux: %v", err)
	}
	samples := fmp4Samples(3, 48)
	frag := buildFragment(1, samples, 1024)
	var durs []time.Duration
	got := [][]byte{}
	if err := d.demux(frag, false, func(au []byte, dur time.Duration) {
		got = append(got, append([]byte(nil), au...))
		durs = append(durs, dur)
	}); err != nil {
		t.Fatalf("demux: %v", err)
	}
	if len(got) != len(samples) {
		t.Fatalf("delivered %d samples, want %d", len(got), len(samples))
	}
	for i := range samples {
		if !bytes.Equal(got[i], samples[i]) {
			t.Errorf("sample %d mismatch", i)
		}
	}
	// 1024 ticks at 44100 Hz, the same value the ADTS path reports for a frame.
	want := 1024 * time.Second / 44100
	for i, dur := range durs {
		if dur != want {
			t.Errorf("sample %d dur = %v, want %v", i, dur, want)
		}
	}
	// end() is a no-op; it must deliver nothing.
	extra := 0
	d.end(func([]byte, time.Duration) { extra++ })
	if extra != 0 {
		t.Errorf("end() delivered %d frames, want 0 (fMP4 end is a no-op)", extra)
	}
}

func TestFMP4DemuxAcrossTwoFragments(t *testing.T) {
	init := buildInitSegment(wantASC, 48000, 2)
	d, err := newFMP4Demux(init)
	if err != nil {
		t.Fatalf("newFMP4Demux: %v", err)
	}
	s0 := fmp4Samples(2, 40)
	s1 := fmp4Samples(2, 55)
	got := collectFMP4(t, d, buildFragment(2, s0, 1024))
	got = append(got, collectFMP4(t, d, buildFragment(2, s1, 1024))...)
	want := append(append([][]byte{}, s0...), s1...)
	if len(got) != len(want) {
		t.Fatalf("delivered %d samples across two fragments, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("sample %d mismatch across fragments", i)
		}
	}
}

func TestFMP4DemuxTruncatedInitIsMalformed(t *testing.T) {
	init := buildInitSegment(wantASC, 44100, 1)
	if _, err := newFMP4Demux(init[:len(init)-4]); !errors.Is(err, ErrMalformedSegment) {
		t.Errorf("truncated init = %v, want ErrMalformedSegment", err)
	}
}

func TestMapMP4Err(t *testing.T) {
	// An encrypted or non-AAC sample entry is an unsupported codec; every other
	// mp4 sentinel is a malformed segment.
	if got := mapMP4Err(mp4.ErrUnsupportedSampleEntry); !errors.Is(got, ErrUnsupportedCodec) {
		t.Errorf("ErrUnsupportedSampleEntry -> %v, want ErrUnsupportedCodec", got)
	}
	for _, e := range []error{mp4.ErrNoAudioTrack, mp4.ErrNoASC, mp4.ErrMalformedBox} {
		if got := mapMP4Err(e); !errors.Is(got, ErrMalformedSegment) {
			t.Errorf("%v -> %v, want ErrMalformedSegment", e, got)
		}
	}
}

func TestFMP4DemuxShortMdatCountsGap(t *testing.T) {
	init := buildInitSegment(wantASC, 44100, 1)
	d, err := newFMP4Demux(init)
	if err != nil {
		t.Fatalf("newFMP4Demux: %v", err)
	}
	// A fragment whose sample sizes overrun the mdat is a counted gap, not a fatal
	// error: demux returns nil and gapCount rises.
	samples := fmp4Samples(1, 30)
	frag := buildFragment(1, samples, 1024)
	// Truncate the mdat payload so the declared 30-byte sample overruns.
	frag = frag[:len(frag)-20]
	before := d.gapCount()
	if err := d.demux(frag, false, func([]byte, time.Duration) {}); err != nil {
		t.Fatalf("demux returned a fatal error on a short mdat: %v", err)
	}
	if d.gapCount() <= before {
		t.Errorf("gapCount did not rise on a short mdat: %d", d.gapCount())
	}
}
