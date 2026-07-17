package eventstore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultSpoolFlushInterval = 100 * time.Millisecond
	DefaultSpoolFlushBytes    = 64 << 10
	DefaultSpoolSyncInterval  = time.Second
	DefaultWALSegmentBytes    = 64 << 20
	DefaultRawSegmentBytes    = 128 << 20
	DefaultSpoolBufferBytes   = 64 << 10
	// DefaultSpoolMaxTotalBytes caps one session's combined raw and WAL
	// segments. Recovered segment sizes count toward the same 4 GiB budget.
	DefaultSpoolMaxTotalBytes int64 = 4 << 30
)

var (
	ErrSpoolClosed   = errors.New("event spool closed")
	ErrSpoolFailed   = errors.New("event spool failed")
	ErrSpoolLimit    = errors.New("event spool byte limit reached")
	ErrSequenceOrder = errors.New("event sequence is not strictly increasing")
)

type SpoolFile interface {
	io.Reader
	io.Writer
	io.Seeker
	Stat() (fs.FileInfo, error)
	Sync() error
	Truncate(size int64) error
	Close() error
}

type SpoolStage string

const (
	SpoolStageRawWritten SpoolStage = "raw_written"
	SpoolStageWALWritten SpoolStage = "wal_written"
	SpoolStageRawFlushed SpoolStage = "raw_flushed"
	SpoolStageWALFlushed SpoolStage = "wal_flushed"
	SpoolStageRawSynced  SpoolStage = "raw_synced"
	SpoolStageWALSynced  SpoolStage = "wal_synced"
	SpoolStageDurable    SpoolStage = "durable"
)

type SpoolOptions struct {
	Root             string
	FlushInterval    time.Duration
	FlushBytes       int64
	SyncInterval     time.Duration
	WALSegmentBytes  int64
	RawSegmentBytes  int64
	MaxPayloadBytes  int
	MaxWALFrameBytes int
	// MaxTotalBytes caps all recovered and newly appended raw+WAL bytes for
	// this session. Zero selects DefaultSpoolMaxTotalBytes.
	MaxTotalBytes int64
	BufferBytes   int
	Now           func() time.Time
	OpenFile      func(string, int, fs.FileMode) (SpoolFile, error)
	MkdirAll      func(string, fs.FileMode) error
	ReadDir       func(string) ([]fs.DirEntry, error)
	Observe       func(SpoolStage)
	// ProtectedSpool and ProtectedRaw identify an already committed checkpoint.
	// Repair validates both positions and every prospective truncation before it
	// mutates any segment. Replay sets ProtectedSpool from its after cursor.
	ProtectedSpool SpoolPosition
	ProtectedRaw   SpoolPosition
}

func DefaultSpoolOptions(root string) SpoolOptions {
	return SpoolOptions{
		Root:             root,
		FlushInterval:    DefaultSpoolFlushInterval,
		FlushBytes:       DefaultSpoolFlushBytes,
		SyncInterval:     DefaultSpoolSyncInterval,
		WALSegmentBytes:  DefaultWALSegmentBytes,
		RawSegmentBytes:  DefaultRawSegmentBytes,
		MaxPayloadBytes:  DefaultMaxPayloadBytes,
		MaxWALFrameBytes: DefaultMaxWALFrameBytes,
		MaxTotalBytes:    DefaultSpoolMaxTotalBytes,
		BufferBytes:      DefaultSpoolBufferBytes,
		Now:              time.Now,
		OpenFile: func(name string, flag int, mode fs.FileMode) (SpoolFile, error) {
			return os.OpenFile(name, flag, mode)
		},
		MkdirAll: os.MkdirAll,
		ReadDir:  os.ReadDir,
	}
}

type DurableAppend struct {
	Record SpoolRecord
	Spool  SpoolPosition
	Raw    SpoolPosition
}

type DurableResult struct {
	Append DurableAppend
	Err    error
}

type spoolRequestKind uint8

const (
	spoolRequestAppend spoolRequestKind = iota
	spoolRequestAppendBatch
	spoolRequestFlush
)

type spoolBatchResult struct {
	appends []DurableAppend
	err     error
}

type spoolRequest struct {
	kind     spoolRequestKind
	envelope IngestEnvelope
	batch    []IngestEnvelope
	result   chan DurableResult
	batchOut chan spoolBatchResult
	barrier  chan error
}

type spoolSegment struct {
	file     SpoolFile
	writer   *bufio.Writer
	relative string
	offset   int64
	hour     time.Time
}

type pendingAppend struct {
	result chan DurableResult
	value  DurableAppend
}

type Spool struct {
	options SpoolOptions

	stateMu      sync.Mutex
	accepting    bool
	closeStarted bool
	closeDone    chan struct{}
	closeErr     error
	admissions   sync.WaitGroup
	requests     chan spoolRequest
	submitMu     sync.Mutex

	raw             spoolSegment
	wal             spoolSegment
	nextRawIndex    int
	nextWALIndex    int
	lastSequence    int64
	durableSequence int64
	durableRaw      SpoolPosition
	durableWAL      SpoolPosition
	totalBytes      int64
	dirtyBytes      int64
	dirty           bool
	pending         []pendingAppend
}

func OpenSpool(options SpoolOptions) (*Spool, error) {
	normalized, err := normalizeSpoolOptions(options)
	if err != nil {
		return nil, err
	}
	spoolDir := filepath.Join(normalized.Root, "spool")
	if err := normalized.MkdirAll(spoolDir, 0o700); err != nil {
		return nil, fmt.Errorf("create spool directory: %w", err)
	}
	recovery, err := repairSpool(normalized)
	if err != nil {
		return nil, err
	}
	s := &Spool{
		options:         normalized,
		accepting:       true,
		closeDone:       make(chan struct{}),
		requests:        make(chan spoolRequest),
		nextRawIndex:    recovery.NextRawIndex,
		nextWALIndex:    recovery.NextWALIndex,
		lastSequence:    recovery.LastSequence,
		durableSequence: recovery.LastSequence,
		totalBytes:      recovery.TotalBytes,
	}
	hour := normalized.Now().UTC().Truncate(time.Hour)
	if err := s.openRaw(hour); err != nil {
		return nil, err
	}
	if err := s.openWAL(hour); err != nil {
		_ = s.raw.file.Close()
		return nil, err
	}
	go s.run()
	return s, nil
}

func normalizeSpoolOptions(options SpoolOptions) (SpoolOptions, error) {
	defaults := DefaultSpoolOptions(options.Root)
	if strings.TrimSpace(options.Root) == "" {
		return SpoolOptions{}, fmt.Errorf("spool root is required")
	}
	absolute, err := filepath.Abs(options.Root)
	if err != nil {
		return SpoolOptions{}, fmt.Errorf("resolve spool root: %w", err)
	}
	options.Root = filepath.Clean(absolute)
	if options.FlushInterval == 0 {
		options.FlushInterval = defaults.FlushInterval
	}
	if options.FlushBytes == 0 {
		options.FlushBytes = defaults.FlushBytes
	}
	if options.SyncInterval == 0 {
		options.SyncInterval = defaults.SyncInterval
	}
	if options.WALSegmentBytes == 0 {
		options.WALSegmentBytes = defaults.WALSegmentBytes
	}
	if options.RawSegmentBytes == 0 {
		options.RawSegmentBytes = defaults.RawSegmentBytes
	}
	if options.MaxPayloadBytes == 0 {
		options.MaxPayloadBytes = defaults.MaxPayloadBytes
	}
	if options.MaxWALFrameBytes == 0 {
		options.MaxWALFrameBytes = defaults.MaxWALFrameBytes
	}
	if options.MaxTotalBytes == 0 {
		options.MaxTotalBytes = defaults.MaxTotalBytes
	}
	if options.BufferBytes == 0 {
		options.BufferBytes = defaults.BufferBytes
	}
	if options.Now == nil {
		options.Now = defaults.Now
	}
	if options.OpenFile == nil {
		options.OpenFile = defaults.OpenFile
	}
	if options.MkdirAll == nil {
		options.MkdirAll = defaults.MkdirAll
	}
	if options.ReadDir == nil {
		options.ReadDir = defaults.ReadDir
	}
	if options.FlushInterval <= 0 || options.FlushBytes <= 0 || options.SyncInterval <= 0 ||
		options.WALSegmentBytes <= 0 || options.RawSegmentBytes <= 0 ||
		options.MaxPayloadBytes <= 0 || options.MaxWALFrameBytes <= WALFrameHeaderSize ||
		options.BufferBytes <= 0 || options.MaxTotalBytes < 0 {
		return SpoolOptions{}, fmt.Errorf("invalid spool options")
	}
	return options, nil
}

// Submit transfers ownership of a deep-copied envelope to the single spool
// worker and returns immediately after admission. The future resolves only
// after both raw and WAL files have been flushed and synced.
func (s *Spool) Submit(ctx context.Context, envelope IngestEnvelope) (<-chan DurableResult, error) {
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	return s.submitLocked(ctx, envelope)
}

func (s *Spool) submitLocked(ctx context.Context, envelope IngestEnvelope) (<-chan DurableResult, error) {
	if len(envelope.Payload) > s.options.MaxPayloadBytes {
		return nil, ErrPayloadTooLarge
	}
	envelope = cloneEnvelope(envelope)
	future := make(chan DurableResult, 1)
	request := spoolRequest{kind: spoolRequestAppend, envelope: envelope, result: future}
	if err := s.sendLocked(ctx, request); err != nil {
		return nil, err
	}
	return future, nil
}

func (s *Spool) Append(ctx context.Context, envelope IngestEnvelope) (DurableAppend, error) {
	future, err := s.Submit(ctx, envelope)
	if err != nil {
		return DurableAppend{}, err
	}
	select {
	case result := <-future:
		return result.Append, result.Err
	case <-ctx.Done():
		return DurableAppend{}, ctx.Err()
	}
}

// AppendBatch admits envelopes in slice order and places one durability barrier
// after the batch. If it returns an error, results is the exact ordered prefix
// already confirmed by both raw and WAL Sync; the remaining suffix is not
// reported as durable. It is the preferred API for the ordered consumer.
func (s *Spool) AppendBatch(ctx context.Context, envelopes []IngestEnvelope) ([]DurableAppend, error) {
	if len(envelopes) == 0 {
		if err := s.Flush(ctx); err != nil {
			return nil, err
		}
		return []DurableAppend{}, nil
	}
	batch := make([]IngestEnvelope, len(envelopes))
	for index, envelope := range envelopes {
		if len(envelope.Payload) > s.options.MaxPayloadBytes {
			return nil, ErrPayloadTooLarge
		}
		batch[index] = cloneEnvelope(envelope)
	}
	out := make(chan spoolBatchResult, 1)
	s.submitMu.Lock()
	err := s.sendLocked(ctx, spoolRequest{
		kind: spoolRequestAppendBatch, batch: batch, batchOut: out,
	})
	s.submitMu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case result := <-out:
		return result.appends, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Spool) Flush(ctx context.Context) error {
	s.submitMu.Lock()
	barrier, err := s.flushLocked(ctx)
	s.submitMu.Unlock()
	if err != nil {
		return err
	}
	select {
	case err := <-barrier:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Replay places a durability barrier after all previously submitted envelopes
// and blocks new submissions while the repaired snapshot is read.
func (s *Spool) Replay(ctx context.Context, after SpoolPosition, visit func(SpoolRecord, SpoolPosition) error) error {
	return s.replay(ctx, after, SpoolPosition{}, visit)
}

// ReplayCheckpoint protects both committed WAL and raw cursors before repair.
func (s *Spool) ReplayCheckpoint(
	ctx context.Context,
	checkpoint Checkpoint,
	visit func(SpoolRecord, SpoolPosition) error,
) error {
	return s.replay(ctx, checkpoint.Spool, checkpoint.Raw, visit)
}

func (s *Spool) replay(
	ctx context.Context,
	after SpoolPosition,
	protectedRaw SpoolPosition,
	visit func(SpoolRecord, SpoolPosition) error,
) error {
	s.submitMu.Lock()
	defer s.submitMu.Unlock()
	barrier, err := s.flushLocked(ctx)
	if err != nil {
		return err
	}
	select {
	case err := <-barrier:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	options := s.options
	options.ProtectedSpool = after
	options.ProtectedRaw = protectedRaw
	return ReplaySpoolWithOptions(options, after, func(record SpoolRecord, position SpoolPosition) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return visit(record, position)
		}
	})
}

func (s *Spool) flushLocked(ctx context.Context) (<-chan error, error) {
	barrier := make(chan error, 1)
	if err := s.sendLocked(ctx, spoolRequest{kind: spoolRequestFlush, barrier: barrier}); err != nil {
		return nil, err
	}
	return barrier, nil
}

func (s *Spool) sendLocked(ctx context.Context, request spoolRequest) error {
	if err := s.beginAdmission(); err != nil {
		return err
	}
	defer s.admissions.Done()
	select {
	case s.requests <- request:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Spool) beginAdmission() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if !s.accepting {
		if s.closeErr != nil {
			return errors.Join(ErrSpoolFailed, s.closeErr)
		}
		return ErrSpoolClosed
	}
	s.admissions.Add(1)
	return nil
}

// Close starts one caller-independent drain. A caller timeout only stops that
// caller's wait; later callers join the same close result.
func (s *Spool) Close(ctx context.Context) error {
	done := s.startClose(nil)
	select {
	case <-done:
		s.stateMu.Lock()
		err := s.closeErr
		s.stateMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Spool) startClose(cause error) <-chan struct{} {
	s.stateMu.Lock()
	if cause != nil && s.closeErr == nil {
		s.closeErr = cause
	}
	if !s.closeStarted {
		s.closeStarted = true
		s.accepting = false
		go func() {
			s.admissions.Wait()
			close(s.requests)
		}()
	}
	done := s.closeDone
	s.stateMu.Unlock()
	return done
}

func (s *Spool) run() {
	flushTimer := time.NewTicker(s.options.FlushInterval)
	syncTimer := time.NewTicker(s.options.SyncInterval)
	defer flushTimer.Stop()
	defer syncTimer.Stop()

	var terminal error
	for terminal == nil {
		select {
		case request, ok := <-s.requests:
			if !ok {
				terminal = io.EOF
				break
			}
			switch request.kind {
			case spoolRequestAppend:
				value, err := s.writeEnvelope(request.envelope)
				if err != nil {
					if isNonFatalSpoolInputError(err) {
						request.result <- DurableResult{Err: err}
						continue
					}
					err = errors.Join(err, s.rollbackUnconfirmed())
					request.result <- DurableResult{Err: err}
					terminal = err
					break
				}
				s.pending = append(s.pending, pendingAppend{result: request.result, value: value})
				if s.dirtyBytes >= s.options.FlushBytes {
					if err := s.flushOnly(); err != nil {
						terminal = errors.Join(err, s.rollbackUnconfirmed())
					}
				}
			case spoolRequestAppendBatch:
				values, err := s.writeEnvelopeBatch(request.batch)
				if err == nil {
					err = s.durableBarrier()
				}
				fatal := err != nil && !isNonFatalSpoolInputError(err)
				if fatal {
					err = errors.Join(err, s.rollbackUnconfirmed())
				}
				if err != nil {
					values = s.durableBatchPrefix(values)
				}
				request.batchOut <- spoolBatchResult{appends: values, err: err}
				if fatal {
					terminal = err
				}
			case spoolRequestFlush:
				err := s.durableBarrier()
				if err != nil {
					err = errors.Join(err, s.rollbackUnconfirmed())
					terminal = err
				}
				request.barrier <- err
			}
		case <-flushTimer.C:
			if s.dirty {
				if err := s.flushOnly(); err != nil {
					terminal = errors.Join(err, s.rollbackUnconfirmed())
				}
			}
		case <-syncTimer.C:
			if s.dirty {
				if err := s.durableBarrier(); err != nil {
					terminal = errors.Join(err, s.rollbackUnconfirmed())
				}
			}
		}
	}
	if errors.Is(terminal, io.EOF) {
		terminal = s.durableBarrier()
		if terminal != nil {
			terminal = errors.Join(terminal, s.rollbackUnconfirmed())
			s.failPending(terminal)
		}
	} else if terminal != nil {
		s.failPending(terminal)
		s.startClose(terminal)
		for request := range s.requests {
			err := errors.Join(ErrSpoolFailed, terminal)
			switch request.kind {
			case spoolRequestAppend:
				request.result <- DurableResult{Err: err}
			case spoolRequestAppendBatch:
				request.batchOut <- spoolBatchResult{err: err}
			case spoolRequestFlush:
				request.barrier <- err
			}
		}
	}
	closeErr := s.closeFiles()
	if terminal != nil && !errors.Is(terminal, io.EOF) {
		closeErr = errors.Join(terminal, closeErr)
	}
	s.finishClose(closeErr)
}

func isNonFatalSpoolInputError(err error) bool {
	return errors.Is(err, ErrSequenceOrder) || errors.Is(err, ErrSpoolLimit) ||
		errors.Is(err, ErrPayloadTooLarge) || errors.Is(err, ErrFrameCorrupt) ||
		errors.Is(err, ErrFrameTooLarge)
}

func (s *Spool) writeEnvelopeBatch(envelopes []IngestEnvelope) ([]DurableAppend, error) {
	hours, err := s.preflightEnvelopeBatch(envelopes)
	if err != nil {
		return nil, err
	}
	appends := make([]DurableAppend, 0, len(envelopes))
	for index, envelope := range envelopes {
		appendValue, err := s.writeEnvelopeAt(envelope, hours[index])
		if err != nil {
			return appends, err
		}
		appends = append(appends, appendValue)
	}
	return appends, nil
}

func (s *Spool) durableBatchPrefix(appends []DurableAppend) []DurableAppend {
	prefixLength := 0
	for prefixLength < len(appends) &&
		appends[prefixLength].Record.Envelope.Sequence <= s.durableSequence {
		prefixLength++
	}
	return appends[:prefixLength]
}

// preflightEnvelopeBatch simulates every rotation and encoded frame before the
// first mutation. In particular, ErrSpoolLimit means the entire batch left no
// durable prefix and did not grow or rotate any segment.
func (s *Spool) preflightEnvelopeBatch(envelopes []IngestEnvelope) ([]time.Time, error) {
	rawRelative, rawOffset, rawHour := s.raw.relative, s.raw.offset, s.raw.hour
	walOffset, walHour := s.wal.offset, s.wal.hour
	nextRawIndex := s.nextRawIndex
	lastSequence := s.lastSequence
	totalBytes := s.totalBytes
	hours := make([]time.Time, len(envelopes))

	for index, envelope := range envelopes {
		if len(envelope.Payload) > s.options.MaxPayloadBytes {
			return nil, ErrPayloadTooLarge
		}
		if envelope.Sequence <= lastSequence {
			return nil, ErrSequenceOrder
		}
		rawFrame, rawInfo, err := EncodeRawFrame(envelope.Payload, s.options.MaxPayloadBytes)
		if err != nil {
			return nil, err
		}
		hour := s.options.Now().UTC().Truncate(time.Hour)
		hours[index] = hour
		if !hour.Equal(rawHour) || (rawOffset > 0 && rawOffset+int64(len(rawFrame)) > s.options.RawSegmentBytes) {
			rawRelative = fmt.Sprintf(
				"spool/raw-%s-%06d.binpack", hour.Format("20060102T15"), nextRawIndex,
			)
			nextRawIndex++
			rawOffset = 0
			rawHour = hour
		}
		record := SpoolRecord{
			Version:  ContractVersion,
			Envelope: cloneEnvelope(envelope),
			Raw: RawRef{
				File: rawRelative, Offset: rawOffset,
				Length: rawInfo.EncodedLength, CRC32C: rawInfo.CRC32C,
			},
		}
		record.Envelope.Payload = nil
		walFrame, _, err := EncodeWALFrame(record, s.options.MaxWALFrameBytes)
		if err != nil {
			return nil, err
		}
		if !hour.Equal(walHour) || (walOffset > 0 && walOffset+int64(len(walFrame)) > s.options.WALSegmentBytes) {
			walOffset = 0
			walHour = hour
		}
		growth := int64(len(rawFrame)) + int64(len(walFrame))
		if s.options.MaxTotalBytes > 0 &&
			(totalBytes > s.options.MaxTotalBytes || growth > s.options.MaxTotalBytes-totalBytes) {
			return nil, ErrSpoolLimit
		}
		totalBytes += growth
		rawOffset += int64(len(rawFrame))
		walOffset += int64(len(walFrame))
		lastSequence = envelope.Sequence
	}
	return hours, nil
}

func (s *Spool) writeEnvelope(envelope IngestEnvelope) (DurableAppend, error) {
	return s.writeEnvelopeAt(envelope, s.options.Now().UTC().Truncate(time.Hour))
}

func (s *Spool) writeEnvelopeAt(envelope IngestEnvelope, nowHour time.Time) (DurableAppend, error) {
	if envelope.Sequence <= s.lastSequence {
		return DurableAppend{}, ErrSequenceOrder
	}
	rawFrame, rawInfo, err := EncodeRawFrame(envelope.Payload, s.options.MaxPayloadBytes)
	if err != nil {
		return DurableAppend{}, err
	}
	if !nowHour.Equal(s.raw.hour) || (s.raw.offset > 0 && s.raw.offset+int64(len(rawFrame)) > s.options.RawSegmentBytes) {
		if err := s.durableBarrier(); err != nil {
			return DurableAppend{}, err
		}
		if err := s.rotateRaw(nowHour); err != nil {
			return DurableAppend{}, err
		}
	}
	rawRef := RawRef{
		File:   s.raw.relative,
		Offset: s.raw.offset,
		Length: rawInfo.EncodedLength,
		CRC32C: rawInfo.CRC32C,
	}
	record := SpoolRecord{
		Version:  ContractVersion,
		Envelope: cloneEnvelope(envelope),
		Raw:      rawRef,
	}
	record.Envelope.Payload = nil
	walFrame, _, err := EncodeWALFrame(record, s.options.MaxWALFrameBytes)
	if err != nil {
		return DurableAppend{}, err
	}
	if !nowHour.Equal(s.wal.hour) || (s.wal.offset > 0 && s.wal.offset+int64(len(walFrame)) > s.options.WALSegmentBytes) {
		if err := s.durableBarrier(); err != nil {
			return DurableAppend{}, err
		}
		if err := s.rotateWAL(nowHour); err != nil {
			return DurableAppend{}, err
		}
	}
	if s.options.MaxTotalBytes > 0 && s.totalBytes+int64(len(rawFrame))+int64(len(walFrame)) > s.options.MaxTotalBytes {
		return DurableAppend{}, ErrSpoolLimit
	}
	if err := writeFull(s.raw.writer, rawFrame); err != nil {
		return DurableAppend{}, fmt.Errorf("write raw frame: %w", err)
	}
	s.observe(SpoolStageRawWritten)
	if err := writeFull(s.wal.writer, walFrame); err != nil {
		return DurableAppend{}, fmt.Errorf("write wal frame: %w", err)
	}
	s.observe(SpoolStageWALWritten)
	s.raw.offset += int64(len(rawFrame))
	s.wal.offset += int64(len(walFrame))
	s.totalBytes += int64(len(rawFrame)) + int64(len(walFrame))
	s.dirtyBytes += int64(len(rawFrame)) + int64(len(walFrame))
	s.dirty = true
	s.lastSequence = envelope.Sequence
	return DurableAppend{
		Record: record,
		Spool:  SpoolPosition{File: s.wal.relative, Offset: s.wal.offset},
		Raw:    SpoolPosition{File: s.raw.relative, Offset: s.raw.offset},
	}, nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (s *Spool) flushOnly() error {
	if !s.dirty {
		return nil
	}
	if err := s.raw.writer.Flush(); err != nil {
		return fmt.Errorf("flush raw spool: %w", err)
	}
	s.observe(SpoolStageRawFlushed)
	if err := s.wal.writer.Flush(); err != nil {
		return fmt.Errorf("flush wal spool: %w", err)
	}
	s.observe(SpoolStageWALFlushed)
	s.dirtyBytes = 0
	return nil
}

func (s *Spool) durableBarrier() error {
	if !s.dirty {
		return nil
	}
	if err := s.flushOnly(); err != nil {
		s.failPending(err)
		return err
	}
	if err := s.raw.file.Sync(); err != nil {
		err = fmt.Errorf("sync raw spool: %w", err)
		s.failPending(err)
		return err
	}
	s.observe(SpoolStageRawSynced)
	if err := s.wal.file.Sync(); err != nil {
		err = fmt.Errorf("sync wal spool: %w", err)
		s.failPending(err)
		return err
	}
	s.observe(SpoolStageWALSynced)
	s.durableSequence = s.lastSequence
	s.durableRaw = SpoolPosition{File: s.raw.relative, Offset: s.raw.offset}
	s.durableWAL = SpoolPosition{File: s.wal.relative, Offset: s.wal.offset}
	s.dirty = false
	pending := s.pending
	s.pending = nil
	for _, item := range pending {
		item.result <- DurableResult{Append: item.value}
	}
	s.observe(SpoolStageDurable)
	return nil
}

func (s *Spool) failPending(err error) {
	pending := s.pending
	s.pending = nil
	for _, item := range pending {
		item.result <- DurableResult{Err: err}
	}
}

// rollbackUnconfirmed removes every byte after the last completed dual Sync.
// Rotation always follows a barrier, so unconfirmed bytes can only exist in
// the two current segments.
func (s *Spool) rollbackUnconfirmed() error {
	var result error
	result = errors.Join(result, rollbackSpoolSegment(&s.raw, s.durableRaw, "raw"))
	result = errors.Join(result, rollbackSpoolSegment(&s.wal, s.durableWAL, "wal"))
	s.dirty = false
	s.dirtyBytes = 0
	return result
}

func rollbackSpoolSegment(segment *spoolSegment, durable SpoolPosition, kind string) error {
	if segment == nil || segment.file == nil {
		return nil
	}
	if segment.writer != nil {
		segment.writer.Reset(io.Discard)
	}
	target := int64(0)
	if durable.File == segment.relative {
		target = durable.Offset
	}
	if target < 0 || target > segment.offset {
		return fmt.Errorf("rollback %s spool: %w", kind, ErrFrameCorrupt)
	}
	info, err := segment.file.Stat()
	if err != nil {
		return fmt.Errorf("stat %s spool rollback: %w", kind, err)
	}
	if info.Size() < target {
		return fmt.Errorf("rollback %s spool: %w", kind, ErrFrameCorrupt)
	}
	if err := segment.file.Truncate(target); err != nil {
		return fmt.Errorf("truncate %s spool rollback: %w", kind, err)
	}
	if _, err := segment.file.Seek(target, io.SeekStart); err != nil {
		return fmt.Errorf("seek %s spool rollback: %w", kind, err)
	}
	if err := segment.file.Sync(); err != nil {
		return fmt.Errorf("sync %s spool rollback: %w", kind, err)
	}
	segment.offset = target
	return nil
}

func (s *Spool) rotateRaw(hour time.Time) error {
	if err := s.raw.file.Close(); err != nil {
		return fmt.Errorf("close raw segment: %w", err)
	}
	return s.openRaw(hour)
}

func (s *Spool) rotateWAL(hour time.Time) error {
	if err := s.wal.file.Close(); err != nil {
		return fmt.Errorf("close wal segment: %w", err)
	}
	return s.openWAL(hour)
}

func (s *Spool) openRaw(hour time.Time) error {
	segment, next, err := s.openSegment("raw", ".binpack", hour, s.nextRawIndex)
	if err != nil {
		return err
	}
	s.raw = segment
	s.nextRawIndex = next
	s.durableRaw = SpoolPosition{File: segment.relative, Offset: 0}
	return nil
}

func (s *Spool) openWAL(hour time.Time) error {
	segment, next, err := s.openSegment("wal", ".wal", hour, s.nextWALIndex)
	if err != nil {
		return err
	}
	s.wal = segment
	s.nextWALIndex = next
	s.durableWAL = SpoolPosition{File: segment.relative, Offset: 0}
	return nil
}

func (s *Spool) openSegment(kind, extension string, hour time.Time, index int) (spoolSegment, int, error) {
	for {
		relative := fmt.Sprintf("spool/%s-%s-%06d%s", kind, hour.Format("20060102T15"), index, extension)
		absolute := filepath.Join(s.options.Root, filepath.FromSlash(relative))
		file, err := s.options.OpenFile(absolute, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, fs.ErrExist) {
			index++
			continue
		}
		if err != nil {
			return spoolSegment{}, index, fmt.Errorf("open %s segment: %w", kind, err)
		}
		return spoolSegment{
			file:     file,
			writer:   bufio.NewWriterSize(file, s.options.BufferBytes),
			relative: relative,
			hour:     hour,
		}, index + 1, nil
	}
}

func (s *Spool) closeFiles() error {
	var result error
	if s.raw.file != nil {
		result = errors.Join(result, s.raw.file.Close())
	}
	if s.wal.file != nil {
		result = errors.Join(result, s.wal.file.Close())
	}
	return result
}

func (s *Spool) finishClose(err error) {
	s.stateMu.Lock()
	switch {
	case s.closeErr == nil:
		s.closeErr = err
	case err == nil:
	case errors.Is(err, s.closeErr):
		s.closeErr = err
	case errors.Is(s.closeErr, err):
	default:
		s.closeErr = errors.Join(s.closeErr, err)
	}
	s.accepting = false
	if !s.closeStarted {
		s.closeStarted = true
	}
	close(s.closeDone)
	s.stateMu.Unlock()
}

func (s *Spool) observe(stage SpoolStage) {
	if s.options.Observe != nil {
		defer func() {
			_ = recover()
		}()
		s.options.Observe(stage)
	}
}

type spoolRecovery struct {
	LastSequence int64
	TotalBytes   int64
	NextRawIndex int
	NextWALIndex int
}

func sortedSpoolFiles(entries []fs.DirEntry, suffix string) []string {
	names := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), suffix) {
			names = append(names, entry.Name())
		}
	}
	sort.SliceStable(names, func(i, j int) bool {
		left, leftOK := spoolSegmentIndex(names[i])
		right, rightOK := spoolSegmentIndex(names[j])
		if leftOK && rightOK && left != right {
			return left < right
		}
		if leftOK != rightOK {
			return leftOK
		}
		return names[i] < names[j]
	})
	return names
}
