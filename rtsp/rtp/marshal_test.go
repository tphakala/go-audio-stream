package rtp

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestHeaderMarshalRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		header  Header
		payload []byte
	}{
		{"minimal", Header{Version: 2, PayloadType: 96, SequenceNumber: 0, Timestamp: 0, SSRC: 0x11223344}, []byte{1, 2, 3, 4}},
		{"marker", Header{Version: 2, Marker: true, PayloadType: 97, SequenceNumber: 1, Timestamp: 960, SSRC: 0xdeadbeef}, []byte{0xff}},
		{"seq-wrap", Header{Version: 2, PayloadType: 10, SequenceNumber: 65535, Timestamp: 0xfffffffe, SSRC: 1}, nil},
		{"two-csrc", Header{Version: 2, PayloadType: 96, SequenceNumber: 42, Timestamp: 7, SSRC: 9, CSRC: []uint32{0xaabbccdd, 0x01020304}}, []byte{5, 6}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := AppendPacket(nil, tt.header, tt.payload)
			if err != nil {
				t.Fatalf("AppendPacket: %v", err)
			}
			pkt, err := ParsePacket(raw)
			if err != nil {
				t.Fatalf("ParsePacket: %v", err)
			}
			if !reflect.DeepEqual(pkt.Header, tt.header) {
				t.Errorf("header round-trip:\n got %+v\nwant %+v", pkt.Header, tt.header)
			}
			if !bytes.Equal(pkt.Payload, tt.payload) {
				t.Errorf("payload round-trip: got %v, want %v", pkt.Payload, tt.payload)
			}
		})
	}
}

func TestAppendToProducesFixedLayout(t *testing.T) {
	h := Header{Version: 2, Marker: true, PayloadType: 96, SequenceNumber: 0x0102, Timestamp: 0x03040506, SSRC: 0x0708090a}
	got, err := h.AppendTo(nil)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	want := []byte{
		0x80,       // V=2, P=0, X=0, CC=0
		0x80 | 96,  // M=1, PT=96
		0x01, 0x02, // seq
		0x03, 0x04, 0x05, 0x06, // timestamp
		0x07, 0x08, 0x09, 0x0a, // ssrc
	}
	if !bytes.Equal(got, want) {
		t.Errorf("AppendTo bytes:\n got %x\nwant %x", got, want)
	}
}

func TestMarshalRejectsExtension(t *testing.T) {
	h := Header{Version: 2, Extension: true, PayloadType: 96}
	if _, err := h.AppendTo(nil); !errors.Is(err, ErrMarshalExtension) {
		t.Errorf("AppendTo with extension: err = %v, want ErrMarshalExtension", err)
	}
}

func TestMarshalRejectsTooManyCSRC(t *testing.T) {
	h := Header{Version: 2, PayloadType: 96, CSRC: make([]uint32, MaxCSRC+1)}
	if _, err := h.AppendTo(nil); !errors.Is(err, ErrTooManyCSRC) {
		t.Errorf("AppendTo with %d CSRCs: err = %v, want ErrTooManyCSRC", MaxCSRC+1, err)
	}
}

func TestAppendPacketNoAlloc(t *testing.T) {
	h := Header{Version: 2, PayloadType: 96, SequenceNumber: 1, Timestamp: 960, SSRC: 7}
	payload := make([]byte, 320)
	dst := make([]byte, 0, HeaderSize+len(payload))
	allocs := testing.AllocsPerRun(200, func() {
		var err error
		dst, err = AppendPacket(dst[:0], h, payload)
		if err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("AppendPacket allocs/op = %v, want 0", allocs)
	}
}
