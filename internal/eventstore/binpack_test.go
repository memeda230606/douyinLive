package eventstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

func TestRawFrameGoldenRoundTrip(t *testing.T) {
	frame, info, err := EncodeRawFrame([]byte("abc"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "444c5242010000001c0000000300000003000000b73f4b36fc8e996f616263"
	if got := hex.EncodeToString(frame); got != golden {
		t.Fatalf("raw golden = %s", got)
	}
	payload, decoded, err := DecodeRawFrame(bytes.NewReader(frame), 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "abc" || decoded != info || info.Compressed {
		t.Fatalf("decoded %q %+v, encoded %+v", payload, decoded, info)
	}
}

func TestRawFrameCompressesOnlyWhenSmaller(t *testing.T) {
	compressible := bytes.Repeat([]byte("douyin-live-"), 300)
	frame, info, err := EncodeRawFrame(compressible, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Compressed || info.StoredLength >= info.RawLength {
		t.Fatalf("compression not beneficial: %+v", info)
	}
	got, _, err := DecodeRawFrame(bytes.NewReader(frame), 1<<20)
	if err != nil || !bytes.Equal(got, compressible) {
		t.Fatalf("round trip err=%v equal=%v", err, bytes.Equal(got, compressible))
	}

	incompressible := make([]byte, 300)
	var state uint32 = 1
	for i := range incompressible {
		state = state*1664525 + 1013904223
		incompressible[i] = byte(state >> 24)
	}
	_, info, err = EncodeRawFrame(incompressible, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if info.Compressed {
		t.Fatalf("larger compressed representation was retained: %+v", info)
	}
}

func TestRawFrameRejectsCRCTruncationAndDecompressionLimit(t *testing.T) {
	frame, _, err := EncodeRawFrame(bytes.Repeat([]byte("x"), 1024), 4096)
	if err != nil {
		t.Fatal(err)
	}
	corruptBody := append([]byte(nil), frame...)
	corruptBody[len(corruptBody)-1] ^= 0xff
	if _, _, err := DecodeRawFrame(bytes.NewReader(corruptBody), 4096); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("body corruption = %v", err)
	}
	corruptHeader := append([]byte(nil), frame...)
	corruptHeader[12] ^= 1
	if _, _, err := DecodeRawFrame(bytes.NewReader(corruptHeader), 4096); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("header corruption = %v", err)
	}
	for _, cut := range []int{1, RawFrameHeaderSize - 1, len(frame) - 1} {
		if _, _, err := DecodeRawFrame(bytes.NewReader(frame[:cut]), 4096); !errors.Is(err, ErrFrameTruncated) {
			t.Fatalf("cut %d = %v", cut, err)
		}
	}
	if _, _, err := DecodeRawFrame(bytes.NewReader(frame), 100); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("decompression bound = %v", err)
	}
}

func TestRawFrameReferenceAndRelativePathValidation(t *testing.T) {
	frame, info, err := EncodeRawFrame([]byte("payload"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	file := append([]byte("prefix"), frame...)
	ref := RawRef{File: "spool/raw-20260717T18-000001.binpack", Offset: 6, Length: info.EncodedLength, CRC32C: info.CRC32C}
	got, err := ReadRawFrameAt(bytes.NewReader(file), ref, 1024)
	if err != nil || string(got) != "payload" {
		t.Fatalf("read ref got=%q err=%v", got, err)
	}
	ref.CRC32C++
	if _, err := ReadRawFrameAt(bytes.NewReader(file), ref, 1024); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("reference crc = %v", err)
	}
	for _, name := range []string{"", "../raw", "/raw", "C:/raw", "spool\\raw"} {
		if validSessionRelativePath(name) {
			t.Fatalf("accepted unsafe path %q", name)
		}
	}
	if !validSessionRelativePath("spool/raw.binpack") {
		t.Fatal("rejected slash-relative path")
	}
}

func TestRawFramePayloadLimit(t *testing.T) {
	if _, _, err := EncodeRawFrame(make([]byte, 5), 4); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("encode limit = %v", err)
	}
}
