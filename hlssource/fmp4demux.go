package hlssource

import (
	"errors"
	"fmt"
	"time"

	"github.com/tphakala/go-audio-stream/internal/mediatime"
	"github.com/tphakala/go-audio-stream/internal/mp4"
)

// fmp4Demux demuxes AAC access units out of fMP4 (CMAF) media fragments. Unlike
// the MPEG-TS demuxer it carries almost no per-segment state: the
// AudioSpecificConfig, timescale and audio track_ID are resolved once from the
// initialization segment (EXT-X-MAP) at construction, and each fragment already
// carries whole samples, so there is no cross-segment framer to preserve and no
// trailing partial frame to flush. It never decodes.
type fmp4Demux struct {
	init mp4.AudioInit
	// gaps counts fragments (and samples within them) that could not be sliced,
	// surfaced by the client as the source's malformed counter, mirroring the TS
	// demuxer's gapCount.
	gaps uint64
}

// newFMP4Demux parses an fMP4 initialization segment (ftyp/moov) up front, so the
// AudioSpecificConfig, timescale and audio track_ID are known before any fragment
// is demuxed. It returns a hlssource-typed error (ErrMalformedSegment or
// ErrUnsupportedCodec) when the init segment carries no usable AAC audio track.
func newFMP4Demux(initSeg []byte) (*fmp4Demux, error) {
	ai, err := mp4.ParseInit(initSeg)
	if err != nil {
		return nil, mapMP4Err(err)
	}
	return &fmp4Demux{init: ai}, nil
}

// demux slices the AAC access units out of one media fragment and delivers each to
// onAU with its duration, derived from the sample's timescale ticks through the
// overflow-safe mediatime helper. The discontinuity flag is ignored: fMP4 samples
// are self-contained, so a timeline break needs no demuxer reset. A sample whose
// data overran the fragment is counted as a gap and stops that fragment without
// failing the stream; a structurally unusable fragment returns an error.
func (d *fmp4Demux) demux(seg []byte, _ bool, onAU func(au []byte, dur time.Duration)) error {
	err := mp4.ParseFragment(d.init, seg, func(s mp4.Sample) error {
		onAU(s.Data, mediatime.PTSFromSamples(uint64(s.Dur), int(d.init.Timescale)))
		return nil
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, mp4.ErrShortMdat) {
		d.gaps++
		return nil
	}
	return mapMP4Err(err)
}

// end is a no-op: an fMP4 fragment carries whole samples, so there is no buffered
// trailing frame to flush at true stream end.
func (d *fmp4Demux) end(func(au []byte, dur time.Duration)) {}

// audioSpecificConfig returns the AAC AudioSpecificConfig resolved from the
// initialization segment. It is known before the first fragment is demuxed.
func (d *fmp4Demux) audioSpecificConfig() []byte { return d.init.ASC }

// gapCount is the running count of fragments or samples that could not be sliced,
// surfaced by the client as the source's malformed counter.
func (d *fmp4Demux) gapCount() uint64 { return d.gaps }

// mapMP4Err maps an internal/mp4 sentinel to the hlssource taxonomy: an encrypted
// or non-AAC sample entry is an unsupported codec; a missing audio track, a
// missing AudioSpecificConfig, or a malformed box is a malformed segment.
func mapMP4Err(err error) error {
	if errors.Is(err, mp4.ErrUnsupportedSampleEntry) {
		return fmt.Errorf("%w: %w", ErrUnsupportedCodec, err)
	}
	return fmt.Errorf("%w: %w", ErrMalformedSegment, err)
}
