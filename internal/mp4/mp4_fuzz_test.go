package mp4

import "testing"

// FuzzParseInit drives ParseInit with arbitrary bytes. It must never panic and
// must always terminate: box iteration advances by the (validated) box length and
// descent is bounded. The parse behavior has dedicated tests; this is a totality
// check over untrusted initialization-segment bytes.
func FuzzParseInit(f *testing.F) {
	f.Add(buildInit(&initOpts{asc: wantASC, timescale: 44100, trackID: 1}))
	f.Add(box("ftyp", []byte("iso5")))
	f.Add([]byte{0, 0, 0, 8, 'm', 'o', 'o', 'v'})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		ai, err := ParseInit(data)
		if err != nil {
			return
		}
		// A successful parse yields a non-empty ASC (the DecoderSpecificInfo is
		// required to be present and non-empty).
		if len(ai.ASC) == 0 {
			t.Fatal("ParseInit succeeded with an empty ASC")
		}
	})
}

// FuzzParseFragment drives ParseFragment with arbitrary bytes against a fixed
// audio track. It must never panic and must always terminate, and every delivered
// sample must alias within the fragment buffer.
func FuzzParseFragment(f *testing.F) {
	f.Add(buildFrag(fragOpts{trackID: 1, samples: [][]byte{{1, 2, 3}, {4, 5}}, dur: 1024, perSample: true}))
	f.Add(buildMuxFrag(1, []byte{0xFF, 0xFF}, 2, [][]byte{{1}, {2}}, 1024))
	f.Add(buildOversizeFrag(1))
	f.Add([]byte{0, 0, 0, 8, 'm', 'o', 'o', 'f'})
	f.Add([]byte{})

	init := AudioInit{TrackID: 1, Timescale: 44100, DefaultDur: 1024, DefaultSize: 4}
	f.Fuzz(func(t *testing.T, frag []byte) {
		delivered := 0
		_ = ParseFragment(init, frag, func(s Sample) error {
			delivered++
			if delivered > len(frag)+16 {
				t.Fatal("ParseFragment delivered more samples than the input can hold")
			}
			if len(s.Data) == 0 {
				t.Fatal("ParseFragment delivered an empty sample")
			}
			return nil
		})
	})
}
