package eventstore

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"testing"
	"time"
)

func testSpoolRecord() SpoolRecord {
	return SpoolRecord{
		Version: ContractVersion,
		Envelope: IngestEnvelope{
			SessionID:       "s",
			EventID:         "e1",
			Sequence:        1,
			Method:          "m",
			ReceivedAt:      time.Date(2026, 7, 17, 18, 0, 0, 123000000, time.UTC),
			SessionOffsetMS: 7,
		},
		Raw: RawRef{
			File:   "spool/raw-20260717T18-000001.binpack",
			Offset: 0,
			Length: 31,
			CRC32C: 0x364b3fb7,
		},
	}
}

func TestWALFrameGoldenRoundTrip(t *testing.T) {
	record := testSpoolRecord()
	frame, info, err := EncodeWALFrame(record, DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	const golden = "444c4557010000003000020026010000f4000000000000000100000000000000c01415349525c318a99c88171a7f7e2565317b2276657273696f6e223a312c22656e76656c6f7065223a7b2273657373696f6e5f6964223a2273222c226576656e745f6964223a226531222c2273657175656e6365223a312c226d6574686f64223a226d222c2272656365697665645f6174223a22323032362d30372d31375431383a30303a30302e3132335a222c2273657373696f6e5f6f66667365745f6d73223a377d2c22726177223a7b2266696c65223a2273706f6f6c2f7261772d32303236303731375431382d3030303030312e62696e7061636b222c226f6666736574223a302c226c656e677468223a33312c22637263333263223a3931303930313137357d7d"
	if got := hex.EncodeToString(frame); got != golden {
		t.Fatalf("wal golden = %s", got)
	}
	decoded, decodedInfo, err := DecodeWALFrame(bytes.NewReader(frame), DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Envelope.EventID != record.Envelope.EventID ||
		decoded.Envelope.Sequence != record.Envelope.Sequence ||
		decoded.Raw != record.Raw ||
		len(decoded.Envelope.Payload) != 0 ||
		decodedInfo != info {
		t.Fatalf("decoded=%+v info=%+v want=%+v", decoded, decodedInfo, info)
	}
}

func TestWALFrameDoesNotDuplicateRawPayload(t *testing.T) {
	record := testSpoolRecord()
	record.Envelope.Payload = bytes.Repeat([]byte("secret raw"), 100)
	frame, _, err := EncodeWALFrame(record, DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(frame, []byte("secret raw")) {
		t.Fatal("raw payload was duplicated into WAL")
	}
	decoded, _, err := DecodeWALFrame(bytes.NewReader(frame), DefaultMaxWALFrameBytes)
	if err != nil || decoded.Envelope.Payload != nil {
		t.Fatalf("decoded payload=%q err=%v", decoded.Envelope.Payload, err)
	}
}

func TestWALFrameRejectsHeaderAndBodyCorruption(t *testing.T) {
	frame, _, err := EncodeWALFrame(testSpoolRecord(), DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	headerBad := append([]byte(nil), frame...)
	headerBad[24] ^= 1
	if _, _, err := DecodeWALFrame(bytes.NewReader(headerBad), DefaultMaxWALFrameBytes); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("header corruption = %v", err)
	}
	bodyBad := append([]byte(nil), frame...)
	bodyBad[len(bodyBad)-1] ^= 1
	if _, _, err := DecodeWALFrame(bytes.NewReader(bodyBad), DefaultMaxWALFrameBytes); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("body corruption = %v", err)
	}

	disagrees := append([]byte(nil), frame...)
	binary.LittleEndian.PutUint64(disagrees[24:32], 2)
	binary.LittleEndian.PutUint32(disagrees[40:44], 0)
	eventIDLength := int(binary.LittleEndian.Uint16(disagrees[10:12]))
	eventID := disagrees[WALFrameHeaderSize : WALFrameHeaderSize+eventIDLength]
	binary.LittleEndian.PutUint32(disagrees[40:44], checksumWALHeader(disagrees[:WALFrameHeaderSize], eventID))
	if _, _, err := DecodeWALFrame(bytes.NewReader(disagrees), DefaultMaxWALFrameBytes); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("header/body disagreement = %v", err)
	}
}

func TestWALFrameRejectsTruncationAndBounds(t *testing.T) {
	frame, _, err := EncodeWALFrame(testSpoolRecord(), DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []int{1, WALFrameHeaderSize - 1, WALFrameHeaderSize + 1, len(frame) - 1} {
		if _, _, err := DecodeWALFrame(bytes.NewReader(frame[:cut]), DefaultMaxWALFrameBytes); !errors.Is(err, ErrFrameTruncated) {
			t.Fatalf("cut %d = %v", cut, err)
		}
	}
	if _, _, err := DecodeWALFrame(bytes.NewReader(frame), len(frame)-1); !errors.Is(err, ErrFrameCorrupt) && !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("decode bound = %v", err)
	}
	if _, _, err := EncodeWALFrame(testSpoolRecord(), WALFrameHeaderSize+1); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("encode bound = %v", err)
	}
}

func TestWALFrameValidatesEventIDAndRawReference(t *testing.T) {
	record := testSpoolRecord()
	record.Envelope.EventID = ""
	if _, _, err := EncodeWALFrame(record, DefaultMaxWALFrameBytes); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("empty event id = %v", err)
	}
	record = testSpoolRecord()
	record.Raw.File = "../raw"
	if _, _, err := EncodeWALFrame(record, DefaultMaxWALFrameBytes); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("unsafe raw ref = %v", err)
	}
	record = testSpoolRecord()
	record.Event = &Event{ID: "different"}
	if _, _, err := EncodeWALFrame(record, DefaultMaxWALFrameBytes); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("event id mismatch = %v", err)
	}
}

func TestChecksumWALHeaderCoversEventID(t *testing.T) {
	frame, _, err := EncodeWALFrame(testSpoolRecord(), DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	eventIDLength := int(binary.LittleEndian.Uint16(frame[10:12]))
	eventID := append([]byte(nil), frame[WALFrameHeaderSize:WALFrameHeaderSize+eventIDLength]...)
	want := binary.LittleEndian.Uint32(frame[40:44])
	eventID[0] ^= 1
	if got := checksumWALHeader(frame[:WALFrameHeaderSize], eventID); got == want {
		t.Fatalf("event id not covered by header crc %08x", crc32.Checksum(eventID, crc32cTable))
	}
}
