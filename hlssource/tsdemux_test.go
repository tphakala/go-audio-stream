package hlssource

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

// collectAUs demuxes one segment and returns the access units delivered.
func collectAUs(t *testing.T, d *tsDemux, seg []byte, discontinuity bool) [][]byte {
	t.Helper()
	var got [][]byte
	if err := d.demux(seg, discontinuity, func(au []byte, _ time.Duration) {
		got = append(got, append([]byte(nil), au...))
	}); err != nil {
		t.Fatalf("demux: %v", err)
	}
	return got
}

func TestTSDemuxDeliversAccessUnitsAndASC(t *testing.T) {
	stream, aus := adtsStream(3, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	d := newTSDemux()
	got := collectAUs(t, d, seg, false)
	d.end(func(au []byte, _ time.Duration) {
		got = append(got, append([]byte(nil), au...))
	})
	if len(got) != len(aus) {
		t.Fatalf("delivered %d AUs, want %d", len(got), len(aus))
	}
	for i := range aus {
		if !bytes.Equal(got[i], aus[i]) {
			t.Errorf("AU %d mismatch", i)
		}
	}
	if !bytes.Equal(d.audioSpecificConfig(), wantASC) {
		t.Errorf("ASC = %x, want %x", d.audioSpecificConfig(), wantASC)
	}
}

func TestTSDemuxFrameDuration(t *testing.T) {
	stream, _ := adtsStream(1, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	d := newTSDemux()
	var dur time.Duration
	if err := d.demux(seg, false, func(_ []byte, du time.Duration) { dur = du }); err != nil {
		t.Fatalf("demux: %v", err)
	}
	d.end(func(_ []byte, du time.Duration) {
		if du > 0 {
			dur = du
		}
	})
	want := 1024 * time.Second / 44100
	if dur != want {
		t.Errorf("frame duration = %v, want %v", dur, want)
	}
}

func TestTSDemuxSplitAcrossSegments(t *testing.T) {
	// An ADTS frame that straddles the boundary between two segments must be
	// delivered exactly once, not flushed unconfirmed at the first segment's end.
	stream, aus := adtsStream(4, 60)
	full := buildTSSegment(stream, 0x1000, 0x0100)
	// Split the raw TS byte stream at an arbitrary packet boundary so the audio
	// PES (and an ADTS frame within it) continues in the second half.
	half := (len(full) / tsPacketLen / 2) * tsPacketLen
	d := newTSDemux()
	got := collectAUs(t, d, full[:half], false)
	got = append(got, collectAUs(t, d, full[half:], false)...)
	d.end(func(au []byte, _ time.Duration) { got = append(got, append([]byte(nil), au...)) })
	if len(got) != len(aus) {
		t.Fatalf("delivered %d AUs across the split, want %d", len(got), len(aus))
	}
	for i := range aus {
		if !bytes.Equal(got[i], aus[i]) {
			t.Errorf("AU %d mismatch across segment split", i)
		}
	}
}

func TestTSDemuxAdaptationFieldSkipped(t *testing.T) {
	// buildTSSegment already pads its final audio packet with an adaptation
	// field of stuffing; a stream that ends on such a packet must still demux.
	stream, aus := adtsStream(2, 17) // small payload forces stuffing on the last packet
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	d := newTSDemux()
	got := collectAUs(t, d, seg, false)
	d.end(func(au []byte, _ time.Duration) { got = append(got, append([]byte(nil), au...)) })
	if len(got) != len(aus) {
		t.Fatalf("delivered %d AUs, want %d", len(got), len(aus))
	}
}

func TestTSDemuxResyncsPastLeadingGarbage(t *testing.T) {
	stream, aus := adtsStream(2, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	garbled := append([]byte{0x00, 0x11, 0x22, 0x47, 0x99}, seg...)
	d := newTSDemux()
	got := collectAUs(t, d, garbled, false)
	d.end(func(au []byte, _ time.Duration) { got = append(got, append([]byte(nil), au...)) })
	if len(got) != len(aus) {
		t.Fatalf("delivered %d AUs after leading garbage, want %d", len(got), len(aus))
	}
}

func TestTSDemuxNoSyncIsMalformed(t *testing.T) {
	d := newTSDemux()
	err := d.demux(make([]byte, 200), false, func([]byte, time.Duration) {})
	if !errors.Is(err, ErrMalformedSegment) {
		t.Errorf("no-sync segment = %v, want ErrMalformedSegment", err)
	}
}

func TestTSDemuxNonAACIsUnsupportedCodec(t *testing.T) {
	stream, _ := adtsStream(1, 40)
	seg := buildTSSegmentType(stream, 0x1000, 0x0100, streamTypeMP3a)
	d := newTSDemux()
	err := d.demux(seg, false, func([]byte, time.Duration) {})
	if !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("MP3 stream_type = %v, want ErrUnsupportedCodec", err)
	}
}

func TestTSDemuxVideoOnlyYieldsNoAudio(t *testing.T) {
	// stream_type 0x1B is H.264 video: not audio, so no ErrUnsupportedCodec, but
	// no ASC resolves either (the caller treats that as a malformed segment).
	stream, _ := adtsStream(1, 40)
	seg := buildTSSegmentType(stream, 0x1000, 0x0100, 0x1B)
	d := newTSDemux()
	if err := d.demux(seg, false, func([]byte, time.Duration) {}); err != nil {
		t.Fatalf("video-only demux errored: %v", err)
	}
	if d.audioSpecificConfig() != nil {
		t.Error("video-only segment should resolve no ASC")
	}
}

func TestTSDemuxScrambledIsUnsupported(t *testing.T) {
	stream, _ := adtsStream(1, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	// Set transport_scrambling_control on the first audio packet (after PAT+PMT).
	seg[2*tsPacketLen+3] |= 0x40
	d := newTSDemux()
	err := d.demux(seg, false, func([]byte, time.Duration) {})
	if !errors.Is(err, ErrUnsupportedPlaylist) {
		t.Errorf("scrambled packet = %v, want ErrUnsupportedPlaylist", err)
	}
}

func TestTSDemuxDiscontinuityResetsAndFlushes(t *testing.T) {
	// Two independent domains: after a discontinuity the demuxer re-acquires
	// PAT/PMT and keeps delivering. Both domains' AUs arrive.
	s1, au1 := adtsStream(2, 40)
	s2, au2 := adtsStream(2, 50)
	seg1 := buildTSSegment(s1, 0x1000, 0x0100)
	seg2 := buildTSSegment(s2, 0x1000, 0x0100)
	d := newTSDemux()
	got := collectAUs(t, d, seg1, false)
	got = append(got, collectAUs(t, d, seg2, true)...) // discontinuity before seg2
	d.end(func(au []byte, _ time.Duration) { got = append(got, append([]byte(nil), au...)) })
	want := append(append([][]byte{}, au1...), au2...)
	if len(got) != len(want) {
		t.Fatalf("delivered %d AUs across a discontinuity, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("AU %d mismatch across discontinuity", i)
		}
	}
}
