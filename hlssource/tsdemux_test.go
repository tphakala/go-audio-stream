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

func TestTSDemuxPMTSpansPackets(t *testing.T) {
	// A PMT padded past 184 bytes spans two TS packets; reassembly must still find
	// the audio PID and demux the elementary stream.
	stream, aus := adtsStream(2, 40)
	seg := buildPAT(0x1000)
	seg = append(seg, psiPackets(0x1000, buildPMTSection(0x0100, streamTypeAAC, 220))...)
	seg = append(seg, tsPESPackets(0x0100, buildPES(stream))...)
	d := newTSDemux()
	got := collectAUs(t, d, seg, false)
	d.end(func(au []byte, _ time.Duration) { got = append(got, append([]byte(nil), au...)) })
	if len(got) != len(aus) {
		t.Fatalf("delivered %d AUs with a multi-packet PMT, want %d", len(got), len(aus))
	}
	for i := range aus {
		if !bytes.Equal(got[i], aus[i]) {
			t.Errorf("AU %d mismatch with spanning PMT", i)
		}
	}
}

func TestTSDemuxPESHeaderSpansPackets(t *testing.T) {
	// A PES optional-header longer than one TS packet pushes the elementary stream
	// start into the continuation packet; the skip counter must consume the rest
	// of the header before feeding audio.
	stream, aus := adtsStream(3, 60)
	pes := buildPESHdr(stream, 220) // 9 + 220 = 229-byte header, past the 184-byte first payload
	seg := buildPAT(0x1000)
	seg = append(seg, buildPMT(0x1000, 0x0100, streamTypeAAC)...)
	seg = append(seg, tsPESPackets(0x0100, pes)...)
	d := newTSDemux()
	got := collectAUs(t, d, seg, false)
	d.end(func(au []byte, _ time.Duration) { got = append(got, append([]byte(nil), au...)) })
	if len(got) != len(aus) {
		t.Fatalf("delivered %d AUs with a spanning PES header, want %d", len(got), len(aus))
	}
	for i := range aus {
		if !bytes.Equal(got[i], aus[i]) {
			t.Errorf("AU %d mismatch with spanning PES header", i)
		}
	}
}

func TestTSDemuxGapCountAccumulatesAcrossReset(t *testing.T) {
	// Leading garbage before a real frame makes the framer count a discard. That
	// count must survive a continuity-domain reset and appear in gapCount.
	stream, _ := adtsStream(2, 40)
	garbled := append(bytes.Repeat([]byte{0xFF, 0x00}, 20), stream...) // non-ADTS lead-in
	seg1 := buildTSSegment(garbled, 0x1000, 0x0100)
	s2, _ := adtsStream(2, 40)
	seg2 := buildTSSegment(s2, 0x1000, 0x0100)
	d := newTSDemux()
	_ = collectAUs(t, d, seg1, false)
	afterFirst := d.gapCount()
	if afterFirst == 0 {
		t.Fatal("gapCount = 0 after leading garbage, want > 0")
	}
	// A discontinuity replaces the framer; the prior count must not be lost.
	_ = collectAUs(t, d, seg2, true)
	if d.gapCount() < afterFirst {
		t.Errorf("gapCount = %d after reset, want >= %d (accumulated)", d.gapCount(), afterFirst)
	}
}

func TestTSDemuxDropsPESWithBadStartCode(t *testing.T) {
	// Enough frames that the first audio packet is a full 184-byte payload (no
	// adaptation field), so the PES start code sits at a known offset. Corrupting
	// it makes the PES unparseable: the demux drops it rather than feeding the
	// header to the framer, so no access unit is delivered and nothing panics.
	stream, _ := adtsStream(6, 40)
	seg := buildTSSegment(stream, 0x1000, 0x0100)
	// PAT and PMT are packets 0 and 1; the first audio packet is packet 2, whose
	// payload begins at byte 4 (afc payload-only). Break packet_start_code_prefix.
	seg[2*tsPacketLen+6] = 0xFF
	d := newTSDemux()
	got := collectAUs(t, d, seg, false)
	d.end(func(au []byte, _ time.Duration) { got = append(got, au) })
	if len(got) != 0 {
		t.Errorf("delivered %d AUs from a PES with a broken start code, want 0", len(got))
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

// tsAudioPacketsCC slices a PES into audioPID TS packets, each carrying at most
// 182 bytes so every packet has an adaptation field with a flags byte (which
// lets the discontinuity_indicator be set). When ccSkipAt >= 0 the
// continuity_counter skips one value before that packet index, simulating a
// dropped TS packet mid-segment; when discAt >= 0 the adaptation-field
// discontinuity_indicator is set on that packet index.
func tsAudioPacketsCC(audioPID uint16, pes []byte, ccSkipAt, discAt int) []byte {
	out := make([]byte, 0, (len(pes)/182+1)*tsPacketLen)
	var cc uint8
	first := true
	for idx := 0; len(pes) > 0; idx++ {
		if idx == ccSkipAt {
			cc++ // leave a hole in the counter the demux must notice
		}
		n := min(182, len(pes))
		pkt := tsPacket(audioPID, first, &cc, pes[:n])
		if idx == discAt {
			pkt[5] |= 0x80 // adaptation-field discontinuity_indicator
		}
		out = append(out, pkt...)
		pes = pes[n:]
		first = false
	}
	return out
}

// tsSegmentCC builds a whole AAC segment whose audio packets are sliced with
// tsAudioPacketsCC, so a test can inject a continuity_counter gap and/or a
// discontinuity_indicator.
func tsSegmentCC(es []byte, pmtPID, audioPID uint16, ccSkipAt, discAt int) []byte {
	seg := buildPAT(pmtPID)
	seg = append(seg, buildPMT(pmtPID, audioPID, streamTypeAAC)...)
	return append(seg, tsAudioPacketsCC(audioPID, buildPES(es), ccSkipAt, discAt)...)
}

func TestTSDemuxContinuityCounterGapCountsLoss(t *testing.T) {
	// A clean multi-packet segment delivers every AU with no gap; the same bytes
	// with a continuity_counter hole (a dropped TS packet) must reset the framer,
	// keep delivering, and surface the loss distinctly in gapCount.
	stream, aus := adtsStream(6, 120)

	clean := tsSegmentCC(stream, 0x1000, 0x0100, -1, -1)
	dc := newTSDemux()
	gotClean := collectAUs(t, dc, clean, false)
	dc.end(func(au []byte, _ time.Duration) { gotClean = append(gotClean, append([]byte(nil), au...)) })
	if len(gotClean) != len(aus) {
		t.Fatalf("clean segment delivered %d AUs, want %d", len(gotClean), len(aus))
	}
	if dc.gapCount() != 0 {
		t.Fatalf("clean segment gapCount = %d, want 0", dc.gapCount())
	}

	gapped := tsSegmentCC(stream, 0x1000, 0x0100, 2, -1)
	dg := newTSDemux()
	gotGapped := collectAUs(t, dg, gapped, false)
	dg.end(func(au []byte, _ time.Duration) { gotGapped = append(gotGapped, append([]byte(nil), au...)) })
	if len(gotGapped) == 0 {
		t.Fatal("gapped segment delivered no AUs; the framer should resync and keep delivering")
	}
	if dg.gapCount() == 0 {
		t.Fatalf("gapped segment gapCount = 0, want > 0 (the dropped packet must be counted)")
	}
}

func TestTSDemuxDuplicateContinuityCounterIsDropped(t *testing.T) {
	// A TS packet may be legitimately transmitted twice (same continuity_counter,
	// same bytes). The demuxer must DROP the repeat, not splice the repeated
	// partial frame into the framer, and must not count it as a loss. A clean
	// stream and the same stream with one audio packet duplicated byte-for-byte
	// must deliver identical access units, and the duplicate must add no gap.
	stream, _ := adtsStream(6, 120)
	pes := buildPES(stream)

	// Build the audio packets once (each at most 182 bytes so none is a boundary
	// case), then optionally splice in a byte-identical duplicate of one packet.
	var audioPackets [][]byte
	{
		var cc uint8
		first := true
		for rest := pes; len(rest) > 0; {
			n := min(182, len(rest))
			audioPackets = append(audioPackets, tsPacket(0x0100, first, &cc, rest[:n]))
			rest = rest[n:]
			first = false
		}
	}

	assemble := func(dupIdx int) []byte {
		seg := buildPAT(0x1000)
		seg = append(seg, buildPMT(0x1000, 0x0100, streamTypeAAC)...)
		for i, p := range audioPackets {
			seg = append(seg, p...)
			if i == dupIdx {
				seg = append(seg, p...) // byte-identical duplicate (same cc and payload)
			}
		}
		return seg
	}

	dc := newTSDemux()
	cleanAUs := collectAUs(t, dc, assemble(-1), false)
	dc.end(func(au []byte, _ time.Duration) { cleanAUs = append(cleanAUs, append([]byte(nil), au...)) })
	if dc.gapCount() != 0 {
		t.Fatalf("clean segment gapCount = %d, want 0", dc.gapCount())
	}

	dd := newTSDemux()
	dupAUs := collectAUs(t, dd, assemble(2), false)
	dd.end(func(au []byte, _ time.Duration) { dupAUs = append(dupAUs, append([]byte(nil), au...)) })
	if dd.gapCount() != 0 {
		t.Errorf("duplicate-packet segment gapCount = %d, want 0 (a duplicate is not a loss)", dd.gapCount())
	}
	if len(dupAUs) != len(cleanAUs) {
		t.Fatalf("duplicate-packet segment delivered %d AUs, want %d (the duplicate must be dropped, not spliced)",
			len(dupAUs), len(cleanAUs))
	}
	for i := range cleanAUs {
		if !bytes.Equal(dupAUs[i], cleanAUs[i]) {
			t.Errorf("AU %d differs after a dropped duplicate packet", i)
		}
	}
}

// tsAdaptOnlyDiscPacket builds a 188-byte adaptation-only (afc=0b10, no payload)
// TS packet for pid with the adaptation-field discontinuity_indicator set. The
// continuity_counter does not advance on a payloadless packet, so cc is the last
// value seen.
func tsAdaptOnlyDiscPacket(pid uint16, cc uint8) []byte {
	pkt := make([]byte, tsPacketLen)
	pkt[0] = tsSync
	pkt[1] = byte(pid >> 8)
	pkt[2] = byte(pid)
	pkt[3] = 0x20 | (cc & 0x0F) // afc=0b10 adaptation only, no payload
	pkt[4] = tsPacketLen - 5    // adaptation_field_length fills the packet
	pkt[5] = 0x80               // discontinuity_indicator
	for i := 6; i < tsPacketLen; i++ {
		pkt[i] = 0xFF // stuffing
	}
	return pkt
}

func TestTSDemuxAdaptationOnlyDiscontinuityIndicatorSuppressesLossCount(t *testing.T) {
	// The discontinuity_indicator can ride on an adaptation-only packet with no
	// payload. That packet is dropped by the payload path, but the splice it
	// announces must still be honored: the following payload packet's counter break
	// is expected, not a dropped-packet loss. Compared against the same counter
	// break with NO announcing packet (a real drop), the announced case reports
	// exactly one fewer gap.
	stream, _ := adtsStream(6, 120)
	pes := buildPES(stream)

	build := func(announce bool) []byte {
		seg := buildPAT(0x1000)
		seg = append(seg, buildPMT(0x1000, 0x0100, streamTypeAAC)...)
		var cc uint8
		first := true
		rest := pes
		for idx := 0; len(rest) > 0; idx++ {
			if idx == 2 {
				if announce {
					seg = append(seg, tsAdaptOnlyDiscPacket(0x0100, cc)...)
				}
				cc++ // jump the counter: an announced splice, or a bare drop
			}
			n := min(182, len(rest))
			seg = append(seg, tsPacket(0x0100, first, &cc, rest[:n])...)
			rest = rest[n:]
			first = false
		}
		return seg
	}

	dAnnounced := newTSDemux()
	_ = collectAUs(t, dAnnounced, build(true), false)
	dAnnounced.end(func([]byte, time.Duration) {})

	dBare := newTSDemux()
	_ = collectAUs(t, dBare, build(false), false)
	dBare.end(func([]byte, time.Duration) {})

	if got, want := dAnnounced.gapCount(), dBare.gapCount()-1; got != want {
		t.Errorf("adaptation-only announced-discontinuity gapCount = %d, want %d (one fewer than the bare drop %d)",
			got, want, dBare.gapCount())
	}
}

func TestTSDemuxDiscontinuityIndicatorSuppressesLossCount(t *testing.T) {
	// The same continuity_counter hole, but with the adaptation-field
	// discontinuity_indicator set on the packet after it, is an announced splice,
	// not a loss: the framer still resets, so the framer's own discard count is
	// identical, but the extra loss increment must be suppressed. The indicator
	// case therefore reports exactly one fewer gap than the bare-hole case.
	stream, _ := adtsStream(6, 120)

	gapOnly := tsSegmentCC(stream, 0x1000, 0x0100, 2, -1)
	dGap := newTSDemux()
	_ = collectAUs(t, dGap, gapOnly, false)
	dGap.end(func([]byte, time.Duration) {})

	gapWithDisc := tsSegmentCC(stream, 0x1000, 0x0100, 2, 2)
	dDisc := newTSDemux()
	_ = collectAUs(t, dDisc, gapWithDisc, false)
	dDisc.end(func([]byte, time.Duration) {})

	if got, want := dDisc.gapCount(), dGap.gapCount()-1; got != want {
		t.Errorf("gapCount with discontinuity_indicator = %d, want %d (one fewer than the bare-hole %d)",
			got, want, dGap.gapCount())
	}
}
