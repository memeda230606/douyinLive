package eventstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const (
	RawFrameVersion         = 1
	RawFrameHeaderSize      = 28
	RawCompressionThreshold = 256
	rawFrameFlagCompressed  = 1
)

var (
	ErrFrameTruncated = errors.New("truncated durable frame")
	ErrFrameCorrupt   = errors.New("corrupt durable frame")
	ErrFrameTooLarge  = errors.New("durable frame exceeds limit")
	crc32cTable       = crc32.MakeTable(crc32.Castagnoli)
	rawFrameMagic     = [4]byte{'D', 'L', 'R', 'B'}
)

type RawFrameInfo struct {
	EncodedLength int64
	RawLength     int
	StoredLength  int
	Compressed    bool
	CRC32C        uint32
}

func EncodeRawFrame(payload []byte, maxDecompressedBytes int) ([]byte, RawFrameInfo, error) {
	if maxDecompressedBytes <= 0 || len(payload) > maxDecompressedBytes {
		return nil, RawFrameInfo{}, ErrFrameTooLarge
	}
	stored := payload
	flags := uint16(0)
	if len(payload) > RawCompressionThreshold {
		encoder, err := zstd.NewWriter(nil,
			zstd.WithEncoderConcurrency(1),
			zstd.WithEncoderCRC(false),
			zstd.WithEncoderLevel(zstd.SpeedFastest),
			zstd.WithWindowSize(1<<20),
		)
		if err != nil {
			return nil, RawFrameInfo{}, fmt.Errorf("create raw compressor: %w", err)
		}
		compressed := encoder.EncodeAll(payload, nil)
		encoder.Close()
		if len(compressed) < len(payload) {
			stored = compressed
			flags = rawFrameFlagCompressed
		}
	}
	if uint64(len(stored)) > uint64(^uint32(0)) {
		return nil, RawFrameInfo{}, ErrFrameTooLarge
	}
	frame := make([]byte, RawFrameHeaderSize+len(stored))
	copy(frame[:4], rawFrameMagic[:])
	binary.LittleEndian.PutUint16(frame[4:6], RawFrameVersion)
	binary.LittleEndian.PutUint16(frame[6:8], flags)
	binary.LittleEndian.PutUint16(frame[8:10], RawFrameHeaderSize)
	binary.LittleEndian.PutUint32(frame[12:16], uint32(len(stored)))
	binary.LittleEndian.PutUint32(frame[16:20], uint32(len(payload)))
	rawCRC := crc32.Checksum(payload, crc32cTable)
	binary.LittleEndian.PutUint32(frame[20:24], rawCRC)
	binary.LittleEndian.PutUint32(frame[24:28], crc32.Checksum(frame[:24], crc32cTable))
	copy(frame[RawFrameHeaderSize:], stored)
	return frame, RawFrameInfo{
		EncodedLength: int64(len(frame)),
		RawLength:     len(payload),
		StoredLength:  len(stored),
		Compressed:    flags != 0,
		CRC32C:        rawCRC,
	}, nil
}

func DecodeRawFrame(reader io.Reader, maxDecompressedBytes int) ([]byte, RawFrameInfo, error) {
	if maxDecompressedBytes <= 0 {
		return nil, RawFrameInfo{}, ErrFrameTooLarge
	}
	header := make([]byte, RawFrameHeaderSize)
	n, err := io.ReadFull(reader, header)
	if err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return nil, RawFrameInfo{}, io.EOF
		}
		return nil, RawFrameInfo{}, fmt.Errorf("%w: raw header", ErrFrameTruncated)
	}
	if !bytes.Equal(header[:4], rawFrameMagic[:]) ||
		binary.LittleEndian.Uint16(header[4:6]) != RawFrameVersion ||
		binary.LittleEndian.Uint16(header[8:10]) != RawFrameHeaderSize ||
		binary.LittleEndian.Uint16(header[10:12]) != 0 {
		return nil, RawFrameInfo{}, fmt.Errorf("%w: raw header identity", ErrFrameCorrupt)
	}
	flags := binary.LittleEndian.Uint16(header[6:8])
	if flags & ^uint16(rawFrameFlagCompressed) != 0 {
		return nil, RawFrameInfo{}, fmt.Errorf("%w: raw flags", ErrFrameCorrupt)
	}
	if binary.LittleEndian.Uint32(header[24:28]) != crc32.Checksum(header[:24], crc32cTable) {
		return nil, RawFrameInfo{}, fmt.Errorf("%w: raw header crc", ErrFrameCorrupt)
	}
	storedLength := int(binary.LittleEndian.Uint32(header[12:16]))
	rawLength := int(binary.LittleEndian.Uint32(header[16:20]))
	if rawLength > maxDecompressedBytes || storedLength > maxDecompressedBytes {
		return nil, RawFrameInfo{}, ErrFrameTooLarge
	}
	if flags == 0 && storedLength != rawLength {
		return nil, RawFrameInfo{}, fmt.Errorf("%w: raw lengths", ErrFrameCorrupt)
	}
	stored := make([]byte, storedLength)
	if _, err := io.ReadFull(reader, stored); err != nil {
		return nil, RawFrameInfo{}, fmt.Errorf("%w: raw body", ErrFrameTruncated)
	}
	payload := stored
	if flags&rawFrameFlagCompressed != 0 {
		decoder, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(uint64(maxDecompressedBytes)),
		)
		if err != nil {
			return nil, RawFrameInfo{}, fmt.Errorf("create raw decompressor: %w", err)
		}
		payload, err = decoder.DecodeAll(stored, make([]byte, 0, rawLength))
		decoder.Close()
		if err != nil {
			return nil, RawFrameInfo{}, fmt.Errorf("%w: raw compression", ErrFrameCorrupt)
		}
	}
	if len(payload) != rawLength {
		return nil, RawFrameInfo{}, fmt.Errorf("%w: decompressed length", ErrFrameCorrupt)
	}
	rawCRC := binary.LittleEndian.Uint32(header[20:24])
	if crc32.Checksum(payload, crc32cTable) != rawCRC {
		return nil, RawFrameInfo{}, fmt.Errorf("%w: raw body crc", ErrFrameCorrupt)
	}
	return payload, RawFrameInfo{
		EncodedLength: int64(RawFrameHeaderSize + storedLength),
		RawLength:     rawLength,
		StoredLength:  storedLength,
		Compressed:    flags != 0,
		CRC32C:        rawCRC,
	}, nil
}

func ReadRawFrameAt(reader io.ReaderAt, ref RawRef, maxDecompressedBytes int) ([]byte, error) {
	if ref.Offset < 0 || ref.Length < RawFrameHeaderSize || !validSessionRelativePath(ref.File) {
		return nil, fmt.Errorf("%w: invalid raw reference", ErrFrameCorrupt)
	}
	section := io.NewSectionReader(reader, ref.Offset, ref.Length)
	payload, info, err := DecodeRawFrame(section, maxDecompressedBytes)
	if err != nil {
		return nil, err
	}
	if info.EncodedLength != ref.Length || info.CRC32C != ref.CRC32C {
		return nil, fmt.Errorf("%w: raw reference mismatch", ErrFrameCorrupt)
	}
	return payload, nil
}

func validSessionRelativePath(name string) bool {
	if name == "" || strings.Contains(name, "\\") || strings.Contains(name, ":") || path.IsAbs(name) {
		return false
	}
	clean := path.Clean(name)
	return clean == name && clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}
