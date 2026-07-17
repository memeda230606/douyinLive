package eventstore

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"
)

const (
	WALFrameVersion         = 1
	WALFrameHeaderSize      = 48
	DefaultMaxWALFrameBytes = 8 << 20
)

var walFrameMagic = [4]byte{'D', 'L', 'E', 'W'}

type WALFrameInfo struct {
	EncodedLength int64
	BodyLength    int
	EventID       string
	Sequence      int64
	ReceivedAt    time.Time
	HeaderCRC32C  uint32
	BodyCRC32C    uint32
}

func EncodeWALFrame(record SpoolRecord, maxFrameBytes int) ([]byte, WALFrameInfo, error) {
	if maxFrameBytes <= WALFrameHeaderSize {
		return nil, WALFrameInfo{}, ErrFrameTooLarge
	}
	record.Version = ContractVersion
	record.Envelope.Payload = nil
	record.Envelope.ReceivedAt = record.Envelope.ReceivedAt.UTC()
	eventID := record.Envelope.EventID
	if eventID == "" || len(eventID) > int(^uint16(0)) {
		return nil, WALFrameInfo{}, fmt.Errorf("%w: invalid event id", ErrFrameCorrupt)
	}
	if record.Envelope.Sequence <= 0 || record.Envelope.ReceivedAt.IsZero() {
		return nil, WALFrameInfo{}, fmt.Errorf("%w: invalid sequence or time", ErrFrameCorrupt)
	}
	if record.Event != nil && record.Event.ID != "" && record.Event.ID != eventID {
		return nil, WALFrameInfo{}, fmt.Errorf("%w: event id disagreement", ErrFrameCorrupt)
	}
	if !validSessionRelativePath(record.Raw.File) || record.Raw.Offset < 0 || record.Raw.Length < RawFrameHeaderSize {
		return nil, WALFrameInfo{}, fmt.Errorf("%w: invalid raw reference", ErrFrameCorrupt)
	}
	body, err := json.Marshal(record)
	if err != nil {
		return nil, WALFrameInfo{}, fmt.Errorf("marshal wal record: %w", err)
	}
	totalLength := WALFrameHeaderSize + len(eventID) + len(body)
	if totalLength > maxFrameBytes || uint64(totalLength) > uint64(^uint32(0)) {
		return nil, WALFrameInfo{}, ErrFrameTooLarge
	}
	frame := make([]byte, totalLength)
	copy(frame[:4], walFrameMagic[:])
	binary.LittleEndian.PutUint16(frame[4:6], WALFrameVersion)
	binary.LittleEndian.PutUint16(frame[8:10], WALFrameHeaderSize)
	binary.LittleEndian.PutUint16(frame[10:12], uint16(len(eventID)))
	binary.LittleEndian.PutUint32(frame[12:16], uint32(totalLength))
	binary.LittleEndian.PutUint32(frame[16:20], uint32(len(body)))
	binary.LittleEndian.PutUint64(frame[24:32], uint64(record.Envelope.Sequence))
	binary.LittleEndian.PutUint64(frame[32:40], uint64(record.Envelope.ReceivedAt.UnixNano()))
	bodyCRC := crc32.Checksum(body, crc32cTable)
	binary.LittleEndian.PutUint32(frame[44:48], bodyCRC)
	copy(frame[WALFrameHeaderSize:WALFrameHeaderSize+len(eventID)], eventID)
	copy(frame[WALFrameHeaderSize+len(eventID):], body)
	headerCRC := checksumWALHeader(frame[:WALFrameHeaderSize], []byte(eventID))
	binary.LittleEndian.PutUint32(frame[40:44], headerCRC)
	return frame, WALFrameInfo{
		EncodedLength: int64(totalLength),
		BodyLength:    len(body),
		EventID:       eventID,
		Sequence:      record.Envelope.Sequence,
		ReceivedAt:    record.Envelope.ReceivedAt,
		HeaderCRC32C:  headerCRC,
		BodyCRC32C:    bodyCRC,
	}, nil
}

func DecodeWALFrame(reader io.Reader, maxFrameBytes int) (SpoolRecord, WALFrameInfo, error) {
	if maxFrameBytes <= WALFrameHeaderSize {
		return SpoolRecord{}, WALFrameInfo{}, ErrFrameTooLarge
	}
	header := make([]byte, WALFrameHeaderSize)
	n, err := io.ReadFull(reader, header)
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return SpoolRecord{}, WALFrameInfo{}, io.EOF
		}
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal header", ErrFrameTruncated)
	}
	if !bytes.Equal(header[:4], walFrameMagic[:]) ||
		binary.LittleEndian.Uint16(header[4:6]) != WALFrameVersion ||
		binary.LittleEndian.Uint16(header[8:10]) != WALFrameHeaderSize ||
		binary.LittleEndian.Uint16(header[6:8]) != 0 ||
		binary.LittleEndian.Uint32(header[20:24]) != 0 {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal header identity", ErrFrameCorrupt)
	}
	eventIDLength := int(binary.LittleEndian.Uint16(header[10:12]))
	totalLength := int(binary.LittleEndian.Uint32(header[12:16]))
	bodyLength := int(binary.LittleEndian.Uint32(header[16:20]))
	if totalLength > maxFrameBytes || bodyLength > maxFrameBytes {
		return SpoolRecord{}, WALFrameInfo{}, ErrFrameTooLarge
	}
	if eventIDLength == 0 || totalLength < WALFrameHeaderSize ||
		WALFrameHeaderSize+eventIDLength+bodyLength != totalLength {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal lengths", ErrFrameCorrupt)
	}
	eventIDBytes := make([]byte, eventIDLength)
	if _, err := io.ReadFull(reader, eventIDBytes); err != nil {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal event id", ErrFrameTruncated)
	}
	headerCRC := binary.LittleEndian.Uint32(header[40:44])
	if checksumWALHeader(header, eventIDBytes) != headerCRC {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal header crc", ErrFrameCorrupt)
	}
	body := make([]byte, bodyLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal body", ErrFrameTruncated)
	}
	bodyCRC := binary.LittleEndian.Uint32(header[44:48])
	if crc32.Checksum(body, crc32cTable) != bodyCRC {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal body crc", ErrFrameCorrupt)
	}
	var record SpoolRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal body schema", ErrFrameCorrupt)
	}
	sequence := int64(binary.LittleEndian.Uint64(header[24:32]))
	receivedAt := time.Unix(0, int64(binary.LittleEndian.Uint64(header[32:40]))).UTC()
	eventID := string(eventIDBytes)
	if record.Version != ContractVersion || record.Envelope.Sequence != sequence ||
		!record.Envelope.ReceivedAt.Equal(receivedAt) || record.Envelope.EventID != eventID ||
		len(record.Envelope.Payload) != 0 {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal header/body disagreement", ErrFrameCorrupt)
	}
	if record.Event != nil && record.Event.ID != "" && record.Event.ID != eventID {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal event id disagreement", ErrFrameCorrupt)
	}
	if !validSessionRelativePath(record.Raw.File) || record.Raw.Offset < 0 || record.Raw.Length < RawFrameHeaderSize {
		return SpoolRecord{}, WALFrameInfo{}, fmt.Errorf("%w: wal raw reference", ErrFrameCorrupt)
	}
	return record, WALFrameInfo{
		EncodedLength: int64(totalLength),
		BodyLength:    bodyLength,
		EventID:       eventID,
		Sequence:      sequence,
		ReceivedAt:    receivedAt,
		HeaderCRC32C:  headerCRC,
		BodyCRC32C:    bodyCRC,
	}, nil
}

func checksumWALHeader(header, eventID []byte) uint32 {
	copyHeader := append([]byte(nil), header...)
	for i := 40; i < 44 && i < len(copyHeader); i++ {
		copyHeader[i] = 0
	}
	checksum := crc32.New(crc32cTable)
	_, _ = checksum.Write(copyHeader)
	_, _ = checksum.Write(eventID)
	return checksum.Sum32()
}
