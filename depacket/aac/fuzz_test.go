package aac

import "testing"

// FuzzParseAUHeaders drives the AU-header parser directly with arbitrary
// bytes under a fixed valid config. It must never panic and must always
// return either a header slice within the caps or a sentinel error.
func FuzzParseAUHeaders(f *testing.F) {
	seeds := [][]byte{
		{},
		{0x00},
		{0x00, 0x10, 0x00, 0x20, 0xAA, 0xBB, 0xCC, 0xDD},
		{0x00, 0x20, 0x00, 0x18, 0x00, 0x28},
		{0xFF, 0xFF, 0x00, 0x00},
		{0x00, 0x00, 0xAA},
	}
	for _, s := range seeds {
		f.Add(s)
	}
	d, err := New(Config{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024})
	if err != nil {
		f.Fatalf("New: %v", err)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		headers, dataStart, err := d.parseAUHeaders(payload)
		if err != nil {
			return
		}
		if len(headers) > MaxAUsPerPacket {
			t.Fatalf("headers %d exceed cap %d", len(headers), MaxAUsPerPacket)
		}
		if dataStart < 2 || dataStart > len(payload) {
			t.Fatalf("dataStart %d out of range for len %d", dataStart, len(payload))
		}
	})
}

// FuzzDepacketize drives the full public entry point across a sequence of
// two packets (to exercise fragment continuation) with arbitrary bytes
// and marker bits. It must never panic.
func FuzzDepacketize(f *testing.F) {
	f.Add([]byte{0x00, 0x10, 0x00, 0x20, 0xAA, 0xBB, 0xCC, 0xDD}, true,
		[]byte{0x00, 0x10, 0x00, 0x30, 0x11}, false)
	d, err := New(Config{SizeLength: 13, IndexLength: 3, IndexDeltaLength: 3, SamplesPerFrame: 1024})
	if err != nil {
		f.Fatalf("New: %v", err)
	}
	f.Fuzz(func(t *testing.T, p1 []byte, m1 bool, p2 []byte, m2 bool) {
		d.Reset()
		_, _ = d.Depacketize(p1, m1, 0)
		_, _ = d.Depacketize(p2, m2, 0)
	})
}
