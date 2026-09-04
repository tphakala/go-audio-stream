package rtp_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/tphakala/go-audio-stream/rtsp/rtp"
)

// A fully documented Sender Report, 28 bytes (7 words, length field 6):
//
//	80          V=2 P=0 RC=0
//	C8          PT=200 (SR)
//	00 06       length = 6 (words - 1)
//	DE AD BE EF SSRC
//	E1 23 45 67 NTP most significant word
//	89 AB CD EF NTP least significant word
//	00 00 27 10 RTP timestamp 10000
//	00 00 00 64 packet count 100
//	00 00 27 10 octet count 10000
var srBytes = []byte{
	0x80, 0xC8, 0x00, 0x06,
	0xDE, 0xAD, 0xBE, 0xEF,
	0xE1, 0x23, 0x45, 0x67,
	0x89, 0xAB, 0xCD, 0xEF,
	0x00, 0x00, 0x27, 0x10,
	0x00, 0x00, 0x00, 0x64,
	0x00, 0x00, 0x27, 0x10,
}

func TestParseSenderReport(t *testing.T) {
	t.Parallel()
	reports, err := rtp.ParseCompound(srBytes)
	if err != nil {
		t.Fatalf("ParseCompound: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("report count = %d, want 1", len(reports))
	}
	sr := reports[0]
	if sr.SSRC != 0xDEADBEEF {
		t.Errorf("SSRC = %#x", sr.SSRC)
	}
	if sr.NTPTimestamp != 0xE123456789ABCDEF {
		t.Errorf("NTP = %#x", sr.NTPTimestamp)
	}
	if sr.RTPTimestamp != 10000 || sr.PacketCount != 100 || sr.OctetCount != 10000 {
		t.Errorf("sender info = %+v", sr)
	}
}

func TestParseCompoundSkipsOther(t *testing.T) {
	t.Parallel()
	// SR followed by a BYE (PT=203, SC=1): 80|1=0x81, C8+3=0xCB, len 1,
	// then one SSRC. ParseCompound returns only the SR.
	bye := []byte{0x81, 0xCB, 0x00, 0x01, 0x00, 0x00, 0x00, 0x2A}
	compound := append(bytes.Clone(srBytes), bye...)
	reports, err := rtp.ParseCompound(compound)
	if err != nil {
		t.Fatalf("ParseCompound: %v", err)
	}
	if len(reports) != 1 || reports[0].SSRC != 0xDEADBEEF {
		t.Errorf("reports = %+v", reports)
	}
}

func TestParseCompoundErrors(t *testing.T) {
	t.Parallel()
	if _, err := rtp.ParseCompound([]byte{0x80}); !errors.Is(err, rtp.ErrShortRTCP) {
		t.Errorf("short err = %v, want ErrShortRTCP", err)
	}
	// length says 6 words (28 bytes) but only the 4-byte header is present.
	if _, err := rtp.ParseCompound([]byte{0x80, 0xC8, 0x00, 0x06}); !errors.Is(err, rtp.ErrRTCPLength) {
		t.Errorf("length err = %v, want ErrRTCPLength", err)
	}
	bad := []byte{0x40, 0xC8, 0x00, 0x06} // version 1
	if _, err := rtp.ParseCompound(bad); !errors.Is(err, rtp.ErrRTCPVersion) {
		t.Errorf("version err = %v, want ErrRTCPVersion", err)
	}
}

func TestReceiverReportMarshal(t *testing.T) {
	t.Parallel()
	rr := rtp.ReceiverReport{
		ReporterSSRC: 0x11223344,
		Blocks: []rtp.ReportBlock{{
			SSRC:             0xDEADBEEF,
			FractionLost:     0,
			CumulativeLost:   0,
			HighestSequence:  0x00010011,
			Jitter:           0,
			LastSR:           0x89ABCDEF,
			DelaySinceLastSR: 0x00010000,
		}},
	}
	// 32 bytes = 8 words, length field 7:
	//   81 C9 00 07                    V=2 RC=1 PT=201 len=7
	//   11 22 33 44                    reporter SSRC
	//   DE AD BE EF                    block SSRC
	//   00 00 00 00                    fraction lost + cumulative lost
	//   00 01 00 11                    extended highest sequence
	//   00 00 00 00                    jitter
	//   89 AB CD EF                    LSR
	//   00 01 00 00                    DLSR
	want := []byte{
		0x81, 0xC9, 0x00, 0x07,
		0x11, 0x22, 0x33, 0x44,
		0xDE, 0xAD, 0xBE, 0xEF,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x01, 0x00, 0x11,
		0x00, 0x00, 0x00, 0x00,
		0x89, 0xAB, 0xCD, 0xEF,
		0x00, 0x01, 0x00, 0x00,
	}
	if got := rr.Marshal(); !bytes.Equal(got, want) {
		t.Errorf("Marshal =\n% x\nwant\n% x", got, want)
	}
}

func TestReceiverReportMarshalClamp(t *testing.T) {
	t.Parallel()
	// 32 blocks must marshal as 31 (the 5-bit RC field maxes at 31): the
	// RC field, the declared length word, and the number of block bytes
	// written must all agree.
	blocks := make([]rtp.ReportBlock, 32)
	for i := range blocks {
		blocks[i].SSRC = uint32(i + 1)
	}
	got := rtp.ReceiverReport{ReporterSSRC: 0x11223344, Blocks: blocks}.Marshal()

	const want = 8 + 31*24 // header + reporter SSRC + 31 blocks
	if len(got) != want {
		t.Fatalf("len = %d, want %d (31 blocks, excess dropped)", len(got), want)
	}
	if rc := got[0] & 0x1f; rc != 31 {
		t.Errorf("RC field = %d, want 31", rc)
	}
	wordLen := uint16(got[2])<<8 | uint16(got[3])
	if int(wordLen+1)*4 != len(got) {
		t.Errorf("length word %d implies %d bytes, but packet is %d bytes",
			wordLen, int(wordLen+1)*4, len(got))
	}
}
