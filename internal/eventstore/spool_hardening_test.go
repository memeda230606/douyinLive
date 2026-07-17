package eventstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type spoolDiskFileState struct {
	Size int64
	Sum  [sha256.Size]byte
}

func snapshotSpoolDisk(t *testing.T, root string) map[string]spoolDiskFileState {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string]spoolDiskFileState)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, "spool", entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = spoolDiskFileState{
			Size: int64(len(data)),
			Sum:  sha256.Sum256(data),
		}
	}
	return result
}

func assertSpoolDiskUnchanged(
	t *testing.T,
	before map[string]spoolDiskFileState,
	root string,
) {
	t.Helper()
	after := snapshotSpoolDisk(t, root)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("spool bytes changed on fail-closed path: before=%v after=%v", before, after)
	}
}

func appendSpoolTestBytes(t *testing.T, name string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func openSpoolWithTwoRecords(
	t *testing.T,
) (SpoolOptions, []DurableAppend) {
	t.Helper()
	options := spoolTestOptions(t)
	options.Now = func() time.Time {
		return time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC)
	}
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	results, err := spool.AppendBatch(context.Background(), []IngestEnvelope{
		spoolTestEnvelope(1, "one"),
		spoolTestEnvelope(2, "two"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return options, results
}

func TestSpoolAppendBatchLimitPreflightIsAllOrNothing(t *testing.T) {
	options := spoolTestOptions(t)
	fixedNow := time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC)
	options.Now = func() time.Time { return fixedNow }
	first := spoolTestEnvelope(1, "small")
	second := spoolTestEnvelope(2, strings.Repeat("mixed-size-payload", 1024))
	options.MaxTotalBytes = spoolTestEncodedAppendBytes(
		t, first, "spool/raw-20260717T18-000001.binpack",
	)
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotSpoolDisk(t, options.Root)
	if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{
		first, second,
	}); !errors.Is(err, ErrSpoolLimit) {
		t.Fatalf("mixed-size batch = %v", err)
	}
	assertSpoolDiskUnchanged(t, before, options.Root)

	results, err := spool.AppendBatch(context.Background(), []IngestEnvelope{first})
	if err != nil {
		t.Fatalf("first sequence was consumed by rejected batch: %v", err)
	}
	if len(results) != 1 || results[0].Record.Envelope.Sequence != 1 {
		t.Fatalf("post-rejection append = %+v", results)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var replayed []int64
	if err := ReplaySpool(options.Root, SpoolPosition{}, func(
		record SpoolRecord,
		_ SpoolPosition,
	) error {
		replayed = append(replayed, record.Envelope.Sequence)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed, []int64{1}) {
		t.Fatalf("replayed sequences = %v", replayed)
	}
}

func TestReplayProtectedCheckpointFailsBeforeAnyRepairMutation(t *testing.T) {
	t.Run("wal candidate below protected cursor", func(t *testing.T) {
		options, results := openSpoolWithTwoRecords(t)
		walPath := filepath.Join(options.Root, filepath.FromSlash(results[1].Spool.File))
		if err := os.Truncate(walPath, results[0].Spool.Offset+3); err != nil {
			t.Fatal(err)
		}
		before := snapshotSpoolDisk(t, options.Root)
		checkpoint := Checkpoint{Spool: results[1].Spool, Raw: results[1].Raw}
		err := ReplaySpoolCheckpointWithOptions(
			options, checkpoint, func(SpoolRecord, SpoolPosition) error { return nil },
		)
		if !errors.Is(err, ErrFrameCorrupt) {
			t.Fatalf("protected wal replay = %v", err)
		}
		assertSpoolDiskUnchanged(t, before, options.Root)
	})

	t.Run("raw candidate below protected cursor", func(t *testing.T) {
		options, results := openSpoolWithTwoRecords(t)
		rawPath := filepath.Join(options.Root, filepath.FromSlash(results[1].Raw.File))
		if err := os.Truncate(rawPath, results[0].Raw.Offset+3); err != nil {
			t.Fatal(err)
		}
		before := snapshotSpoolDisk(t, options.Root)
		checkpoint := Checkpoint{Spool: results[1].Spool, Raw: results[1].Raw}
		err := ReplaySpoolCheckpointWithOptions(
			options, checkpoint, func(SpoolRecord, SpoolPosition) error { return nil },
		)
		if !errors.Is(err, ErrFrameCorrupt) {
			t.Fatalf("protected raw replay = %v", err)
		}
		assertSpoolDiskUnchanged(t, before, options.Root)
	})

	t.Run("protected files disagree", func(t *testing.T) {
		options, results := openSpoolWithTwoRecords(t)
		rawPath := filepath.Join(options.Root, filepath.FromSlash(results[1].Raw.File))
		walPath := filepath.Join(options.Root, filepath.FromSlash(results[1].Spool.File))
		appendSpoolTestBytes(t, rawPath, []byte{1, 2, 3})
		appendSpoolTestBytes(t, walPath, []byte{4, 5, 6})
		before := snapshotSpoolDisk(t, options.Root)
		checkpoint := Checkpoint{Spool: results[0].Spool, Raw: results[1].Raw}
		err := ReplaySpoolCheckpointWithOptions(
			options, checkpoint, func(SpoolRecord, SpoolPosition) error { return nil },
		)
		if !errors.Is(err, ErrFrameCorrupt) {
			t.Fatalf("disagreeing checkpoint = %v", err)
		}
		assertSpoolDiskUnchanged(t, before, options.Root)
	})

	t.Run("protected wal file missing", func(t *testing.T) {
		options, results := openSpoolWithTwoRecords(t)
		rawPath := filepath.Join(options.Root, filepath.FromSlash(results[1].Raw.File))
		walPath := filepath.Join(options.Root, filepath.FromSlash(results[1].Spool.File))
		appendSpoolTestBytes(t, rawPath, []byte{1, 2, 3})
		appendSpoolTestBytes(t, walPath, []byte{4, 5, 6})
		before := snapshotSpoolDisk(t, options.Root)
		checkpoint := Checkpoint{
			Spool: SpoolPosition{
				File: "spool/wal-20260717T18-000002.wal", Offset: 1,
			},
			Raw: results[1].Raw,
		}
		err := ReplaySpoolCheckpointWithOptions(
			options, checkpoint, func(SpoolRecord, SpoolPosition) error { return nil },
		)
		if !errors.Is(err, ErrFrameCorrupt) {
			t.Fatalf("missing protected wal = %v", err)
		}
		assertSpoolDiskUnchanged(t, before, options.Root)
	})
}

func TestRepairSpoolRejectsRawAndWALSegmentIndexGapsWithoutMutation(t *testing.T) {
	for _, testCase := range []struct {
		name string
		path func(DurableAppend) string
	}{
		{name: "raw", path: func(value DurableAppend) string { return value.Raw.File }},
		{name: "wal", path: func(value DurableAppend) string { return value.Spool.File }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			options := spoolTestOptions(t)
			options.RawSegmentBytes = 1
			options.WALSegmentBytes = 1
			spool, err := OpenSpool(options)
			if err != nil {
				t.Fatal(err)
			}
			results, err := spool.AppendBatch(context.Background(), []IngestEnvelope{
				spoolTestEnvelope(1, "one"),
				spoolTestEnvelope(2, "two"),
				spoolTestEnvelope(3, "three"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := spool.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(
				options.Root, filepath.FromSlash(testCase.path(results[1])),
			)); err != nil {
				t.Fatal(err)
			}
			before := snapshotSpoolDisk(t, options.Root)
			if err := RepairSpoolWithOptions(options); !errors.Is(err, ErrFrameCorrupt) {
				t.Fatalf("segment gap repair = %v", err)
			}
			assertSpoolDiskUnchanged(t, before, options.Root)
		})
	}
}

func TestRepairSpoolRejectsValidRecordAfterBrokenFinalRawReference(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	rawName := "spool/raw-20260717T18-000001.binpack"
	walName := "spool/wal-20260717T18-000001.wal"
	validRaw, validInfo, err := EncodeRawFrame([]byte("valid"), DefaultMaxPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	missingRaw, missingInfo, err := EncodeRawFrame([]byte("missing"), DefaultMaxPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, filepath.FromSlash(rawName)), validRaw, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	broken := SpoolRecord{
		Version:  ContractVersion,
		Envelope: spoolTestEnvelope(1, ""),
		Raw: RawRef{
			File:   "spool/raw-20260717T18-000002.binpack",
			Offset: 0, Length: missingInfo.EncodedLength, CRC32C: missingInfo.CRC32C,
		},
	}
	broken.Envelope.Payload = nil
	laterValid := SpoolRecord{
		Version:  ContractVersion,
		Envelope: spoolTestEnvelope(2, ""),
		Raw: RawRef{
			File:   rawName,
			Offset: 0, Length: validInfo.EncodedLength, CRC32C: validInfo.CRC32C,
		},
	}
	laterValid.Envelope.Payload = nil
	brokenFrame, _, err := EncodeWALFrame(broken, DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	validFrame, _, err := EncodeWALFrame(laterValid, DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, filepath.FromSlash(walName)),
		append(brokenFrame, validFrame...),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	_ = missingRaw
	before := snapshotSpoolDisk(t, root)
	if err := RepairSpool(root); !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("broken-then-valid repair = %v", err)
	}
	assertSpoolDiskUnchanged(t, before, root)
}

type targetedSpoolFault struct {
	mu        sync.Mutex
	operation string
	fired     bool
	err       error
}

func (f *targetedSpoolFault) fail(operation string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fired || f.operation != operation {
		return false
	}
	f.fired = true
	return true
}

type targetedSpoolFaultFile struct {
	SpoolFile
	fault *targetedSpoolFault
}

func (f *targetedSpoolFaultFile) Write(data []byte) (int, error) {
	if f.fault.fail("write") {
		return 0, f.fault.err
	}
	return f.SpoolFile.Write(data)
}

func (f *targetedSpoolFaultFile) Sync() error {
	if f.fault.fail("sync") {
		return f.fault.err
	}
	return f.SpoolFile.Sync()
}

func TestSpoolAppendBatchReturnsExactDurablePrefixAcrossSegmentFault(t *testing.T) {
	testCases := []struct {
		name      string
		kind      string
		operation string
		suffix    string
	}{
		{name: "raw write", kind: "raw", operation: "write", suffix: "-000002.binpack"},
		{name: "raw sync", kind: "raw", operation: "sync", suffix: "-000002.binpack"},
		{name: "wal write", kind: "wal", operation: "write", suffix: "-000002.wal"},
		{name: "wal sync", kind: "wal", operation: "sync", suffix: "-000002.wal"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			options := spoolTestOptions(t)
			options.RawSegmentBytes = 1
			options.WALSegmentBytes = 1
			injected := errors.New("injected second-segment " + testCase.name)
			fault := &targetedSpoolFault{operation: testCase.operation, err: injected}
			options.OpenFile = func(name string, flag int, mode fs.FileMode) (SpoolFile, error) {
				file, err := os.OpenFile(name, flag, mode)
				if err != nil {
					return nil, err
				}
				base := filepath.Base(name)
				if strings.Contains(base, testCase.kind+"-") &&
					strings.HasSuffix(base, testCase.suffix) {
					return &targetedSpoolFaultFile{SpoolFile: file, fault: fault}, nil
				}
				return file, nil
			}
			spool, err := OpenSpool(options)
			if err != nil {
				t.Fatal(err)
			}
			results, err := spool.AppendBatch(context.Background(), []IngestEnvelope{
				spoolTestEnvelope(1, "first durable segment"),
				spoolTestEnvelope(2, "faulted later segment"),
			})
			if !errors.Is(err, injected) {
				t.Fatalf("batch error = %v", err)
			}
			if len(results) != 1 || results[0].Record.Envelope.Sequence != 1 {
				t.Fatalf("durable prefix = %+v", results)
			}
			if err := spool.Close(context.Background()); !errors.Is(err, injected) {
				t.Fatalf("close error = %v", err)
			}
			fault.mu.Lock()
			fired := fault.fired
			fault.mu.Unlock()
			if !fired {
				t.Fatal("targeted fault did not fire")
			}
			var replayed []int64
			if err := ReplaySpool(options.Root, SpoolPosition{}, func(
				record SpoolRecord,
				_ SpoolPosition,
			) error {
				replayed = append(replayed, record.Envelope.Sequence)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(replayed, []int64{1}) {
				t.Fatalf("replayed after %s fault = %v", testCase.name, replayed)
			}
		})
	}
}

func TestSpoolFatalRollbackErrorCannotBecomeNonFatal(t *testing.T) {
	options := spoolTestOptions(t)
	injected := errors.New("injected fatal sync")
	fault := &targetedSpoolFault{operation: "sync", err: injected}
	options.OpenFile = func(name string, flag int, mode fs.FileMode) (SpoolFile, error) {
		file, err := os.OpenFile(name, flag, mode)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(name, ".binpack") && flag&os.O_CREATE != 0 {
			return &targetedSpoolFaultFile{SpoolFile: file, fault: fault}, nil
		}
		return file, nil
	}
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	// Force the rollback validator itself to contribute ErrFrameCorrupt. The
	// original Sync error must still classify the worker as terminal.
	spool.durableRaw.Offset = 1 << 30
	results, err := spool.AppendBatch(
		context.Background(),
		[]IngestEnvelope{spoolTestEnvelope(1, "fatal rollback")},
	)
	if len(results) != 0 || !errors.Is(err, injected) || !errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("fatal rollback result=%+v err=%v", results, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := spool.Flush(ctx); !errors.Is(err, ErrSpoolFailed) {
		t.Fatalf("post-fatal flush = %v", err)
	}
	if err := spool.Close(context.Background()); !errors.Is(err, injected) ||
		!errors.Is(err, ErrFrameCorrupt) {
		t.Fatalf("close error = %v", err)
	}
}
