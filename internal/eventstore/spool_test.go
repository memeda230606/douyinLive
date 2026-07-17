package eventstore

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func spoolTestOptions(t *testing.T) SpoolOptions {
	t.Helper()
	options := DefaultSpoolOptions(t.TempDir())
	options.FlushInterval = time.Hour
	options.SyncInterval = time.Hour
	options.FlushBytes = 1 << 20
	return options
}

func spoolTestEnvelope(sequence int64, payload string) IngestEnvelope {
	return IngestEnvelope{
		SessionID:       "session-1",
		EventID:         "event-" + time.Unix(sequence, 0).UTC().Format("150405"),
		Sequence:        sequence,
		Method:          "WebcastChatMessage",
		PlatformRoomID:  "room",
		ReceivedAt:      time.Date(2026, 7, 17, 18, 0, 0, int(sequence), time.UTC),
		SessionOffsetMS: sequence * 10,
		Payload:         []byte(payload),
	}
}

func waitDurable(t *testing.T, future <-chan DurableResult) DurableAppend {
	t.Helper()
	select {
	case result := <-future:
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		return result.Append
	case <-time.After(3 * time.Second):
		t.Fatal("durable result timed out")
		return DurableAppend{}
	}
}

func TestSpoolSubmitPipelinesInOrderAndPublishesOnlyAfterDoubleSync(t *testing.T) {
	options := spoolTestOptions(t)
	var stagesMu sync.Mutex
	var stages []SpoolStage
	options.Observe = func(stage SpoolStage) {
		stagesMu.Lock()
		stages = append(stages, stage)
		stagesMu.Unlock()
	}
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	firstFuture, err := spool.Submit(context.Background(), spoolTestEnvelope(1, "one"))
	if err != nil {
		t.Fatal(err)
	}
	secondFuture, err := spool.Submit(context.Background(), spoolTestEnvelope(3, "three"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-firstFuture:
		t.Fatalf("future completed before sync: %+v", result)
	case <-time.After(20 * time.Millisecond):
	}
	if err := spool.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := waitDurable(t, firstFuture)
	second := waitDurable(t, secondFuture)
	if first.Record.Envelope.Sequence != 1 || second.Record.Envelope.Sequence != 3 {
		t.Fatalf("durable order = %d, %d", first.Record.Envelope.Sequence, second.Record.Envelope.Sequence)
	}
	if first.Spool.File != second.Spool.File || first.Spool.Offset >= second.Spool.Offset {
		t.Fatalf("spool cursors are not ordered: %+v %+v", first.Spool, second.Spool)
	}
	for _, ref := range []string{first.Raw.File, first.Spool.File, second.Raw.File, second.Spool.File} {
		if !validSessionRelativePath(ref) || strings.Contains(ref, "\\") {
			t.Fatalf("unsafe durable path %q", ref)
		}
	}
	stagesMu.Lock()
	gotStages := append([]SpoolStage(nil), stages...)
	stagesMu.Unlock()
	wantPrefix := []SpoolStage{
		SpoolStageRawWritten,
		SpoolStageWALWritten,
		SpoolStageRawWritten,
		SpoolStageWALWritten,
	}
	if len(gotStages) < len(wantPrefix) {
		t.Fatalf("stages = %v", gotStages)
	}
	for i, want := range wantPrefix {
		if got := gotStages[i]; got != want {
			t.Fatalf("write stage %d = %s, want %s (%v)", i, got, want, gotStages)
		}
	}
	wantSuffix := []SpoolStage{
		SpoolStageRawFlushed,
		SpoolStageWALFlushed,
		SpoolStageRawSynced,
		SpoolStageWALSynced,
		SpoolStageDurable,
	}
	if len(gotStages) < len(wantSuffix) {
		t.Fatalf("stages = %v", gotStages)
	}
	for i, want := range wantSuffix {
		if got := gotStages[len(gotStages)-len(wantSuffix)+i]; got != want {
			t.Fatalf("stage %d = %s, want %s (%v)", i, got, want, gotStages)
		}
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var replayed []SpoolRecord
	var cursors []SpoolPosition
	if err := ReplaySpool(options.Root, SpoolPosition{}, func(record SpoolRecord, cursor SpoolPosition) error {
		replayed = append(replayed, record)
		cursors = append(cursors, cursor)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 2 || replayed[0].Envelope.Sequence != 1 || string(replayed[0].Envelope.Payload) != "one" ||
		replayed[1].Envelope.Sequence != 3 || string(replayed[1].Envelope.Payload) != "three" {
		t.Fatalf("replayed = %+v", replayed)
	}
	var after []int64
	if err := ReplaySpool(options.Root, cursors[0], func(record SpoolRecord, _ SpoolPosition) error {
		after = append(after, record.Envelope.Sequence)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 || after[0] != 3 {
		t.Fatalf("checkpoint replay = %v", after)
	}
}

func TestSpoolAppendBatchUsesOneBarrierAndAllowsSequenceGaps(t *testing.T) {
	options := spoolTestOptions(t)
	var mu sync.Mutex
	syncCount := 0
	options.Observe = func(stage SpoolStage) {
		if stage == SpoolStageRawSynced || stage == SpoolStageWALSynced {
			mu.Lock()
			syncCount++
			mu.Unlock()
		}
	}
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	results, err := spool.AppendBatch(context.Background(), []IngestEnvelope{
		spoolTestEnvelope(10, "a"),
		spoolTestEnvelope(20, "b"),
		spoolTestEnvelope(21, "c"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("result count = %d", len(results))
	}
	mu.Lock()
	gotSyncCount := syncCount
	mu.Unlock()
	if gotSyncCount != 2 {
		t.Fatalf("sync count = %d, want raw+wal once", gotSyncCount)
	}
	if _, err := spool.Append(context.Background(), spoolTestEnvelope(20, "duplicate")); !errors.Is(err, ErrSequenceOrder) {
		t.Fatalf("out of order append = %v", err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolCallerCancellationDoesNotCancelAcceptedWriteOrSharedClose(t *testing.T) {
	options := spoolTestOptions(t)
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	future, err := spool.Submit(context.Background(), spoolTestEnvelope(1, "accepted"))
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := spool.Close(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("first close = %v", err)
	}
	result := waitDurable(t, future)
	if result.Record.Envelope.Sequence != 1 {
		t.Fatalf("durable result = %+v", result)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Submit(context.Background(), spoolTestEnvelope(2, "late")); !errors.Is(err, ErrSpoolClosed) {
		t.Fatalf("late submit = %v", err)
	}
	var sequences []int64
	if err := ReplaySpool(options.Root, SpoolPosition{}, func(record SpoolRecord, _ SpoolPosition) error {
		sequences = append(sequences, record.Envelope.Sequence)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sequences) != 1 || sequences[0] != 1 {
		t.Fatalf("replayed sequences = %v", sequences)
	}
}

func TestSpoolAppendWaitCancellationDoesNotOwnDurability(t *testing.T) {
	options := spoolTestOptions(t)
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := spool.Append(ctx, spoolTestEnvelope(1, "background")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("append wait = %v", err)
	}
	if err := spool.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := ReplaySpool(options.Root, SpoolPosition{}, func(record SpoolRecord, _ SpoolPosition) error {
		count++
		if string(record.Envelope.Payload) != "background" {
			t.Fatalf("payload = %q", record.Envelope.Payload)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("replay count = %d", count)
	}
}

func TestSpoolRotatesBySizeAndUTCHour(t *testing.T) {
	options := spoolTestOptions(t)
	var clockMu sync.Mutex
	now := time.Date(2026, 7, 17, 18, 30, 0, 0, time.FixedZone("offset", 8*60*60))
	options.Now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	options.RawSegmentBytes = 1
	options.WALSegmentBytes = 1
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	first, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(1, "one")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(2, "two")})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Raw.File == second[0].Raw.File || first[0].Spool.File == second[0].Spool.File {
		t.Fatalf("size rotation did not occur: %+v %+v", first[0], second[0])
	}
	clockMu.Lock()
	now = now.Add(time.Hour)
	clockMu.Unlock()
	third, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(3, "three")})
	if err != nil {
		t.Fatal(err)
	}
	if third[0].Raw.File == second[0].Raw.File || third[0].Spool.File == second[0].Spool.File {
		t.Fatalf("hour rotation did not occur: %+v %+v", second[0], third[0])
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolConfiguredTotalLimitFailsExplicitly(t *testing.T) {
	options := spoolTestOptions(t)
	options.MaxTotalBytes = 1
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(1, "payload")}); !errors.Is(err, ErrSpoolLimit) {
		t.Fatalf("limit result = %v", err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := ReplaySpool(options.Root, SpoolPosition{}, func(SpoolRecord, SpoolPosition) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("limited append became durable: %d records", count)
	}
}

type spoolFaultController struct {
	mu        sync.Mutex
	kind      string
	operation string
	fired     bool
	err       error
}

type spoolFaultFile struct {
	SpoolFile
	kind       string
	controller *spoolFaultController
}

func (f *spoolFaultFile) Write(data []byte) (int, error) {
	if f.controller.shouldFail(f.kind, "write") {
		return 0, f.controller.err
	}
	return f.SpoolFile.Write(data)
}

func (f *spoolFaultFile) Sync() error {
	if f.controller.shouldFail(f.kind, "sync") {
		return f.controller.err
	}
	return f.SpoolFile.Sync()
}

func (c *spoolFaultController) shouldFail(kind, operation string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fired && c.kind == kind && c.operation == operation {
		c.fired = true
		return true
	}
	return false
}

func TestSpoolFileAndSyncFaultMatrixNeverPublishesDurableCursor(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		kind       string
		operation  string
		bufferSize int
		replays    bool
	}{
		{name: "raw write", kind: "raw", operation: "write", bufferSize: 16},
		{name: "wal write", kind: "wal", operation: "write", bufferSize: 16},
		{name: "raw flush", kind: "raw", operation: "write", bufferSize: 4096},
		{name: "wal flush", kind: "wal", operation: "write", bufferSize: 4096},
		{name: "raw sync", kind: "raw", operation: "sync", bufferSize: 4096},
		{name: "wal sync", kind: "wal", operation: "sync", bufferSize: 4096},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			options := spoolTestOptions(t)
			options.BufferBytes = testCase.bufferSize
			injected := errors.New("injected " + testCase.name)
			controller := &spoolFaultController{kind: testCase.kind, operation: testCase.operation, err: injected}
			options.OpenFile = func(name string, flag int, mode fs.FileMode) (SpoolFile, error) {
				file, err := os.OpenFile(name, flag, mode)
				if err != nil {
					return nil, err
				}
				kind := "wal"
				if strings.HasSuffix(name, ".binpack") {
					kind = "raw"
				}
				return &spoolFaultFile{SpoolFile: file, kind: kind, controller: controller}, nil
			}
			spool, err := OpenSpool(options)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(1, strings.Repeat("x", 512))})
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, injected) {
					t.Fatalf("append error = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("faulted append hung")
			}
			if err := spool.Close(context.Background()); !errors.Is(err, injected) {
				t.Fatalf("close error = %v", err)
			}
			controller.mu.Lock()
			fired := controller.fired
			controller.mu.Unlock()
			if !fired {
				t.Fatal("fault was not reached")
			}
			replayed := 0
			if err := ReplaySpool(options.Root, SpoolPosition{}, func(record SpoolRecord, _ SpoolPosition) error {
				replayed++
				if record.Envelope.Sequence != 1 {
					t.Fatalf("replayed sequence = %d", record.Envelope.Sequence)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			wantReplay := 0
			if testCase.replays {
				wantReplay = 1
			}
			if replayed != wantReplay {
				t.Fatalf("replayed after %s = %d, want %d", testCase.name, replayed, wantReplay)
			}
		})
	}
}

func TestRepairSpoolTruncatesOnlyIncompleteFinalTailsAndReplayRepairs(t *testing.T) {
	options := spoolTestOptions(t)
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
	rawPath := filepath.Join(options.Root, filepath.FromSlash(results[1].Raw.File))
	walPath := filepath.Join(options.Root, filepath.FromSlash(results[1].Spool.File))
	rawBefore, _ := os.Stat(rawPath)
	walBefore, _ := os.Stat(walPath)
	for _, name := range []string{rawPath, walPath} {
		file, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte{1, 2, 3, 4, 5}); err != nil {
			t.Fatal(err)
		}
		file.Close()
	}
	count := 0
	if err := ReplaySpool(options.Root, SpoolPosition{}, func(SpoolRecord, SpoolPosition) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("replay count = %d", count)
	}
	rawAfter, _ := os.Stat(rawPath)
	walAfter, _ := os.Stat(walPath)
	if rawAfter.Size() != rawBefore.Size() || walAfter.Size() != walBefore.Size() {
		t.Fatalf("tails not restored: raw %d/%d wal %d/%d", rawAfter.Size(), rawBefore.Size(), walAfter.Size(), walBefore.Size())
	}
}

func TestRepairSpoolRejectsCRCAndIncompleteNonTailSegment(t *testing.T) {
	t.Run("crc", func(t *testing.T) {
		options := spoolTestOptions(t)
		spool, err := OpenSpool(options)
		if err != nil {
			t.Fatal(err)
		}
		results, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(1, "one")})
		if err != nil {
			t.Fatal(err)
		}
		if err := spool.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(options.Root, filepath.FromSlash(results[0].Spool.File))
		file, err := os.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Seek(-1, 2); err != nil {
			t.Fatal(err)
		}
		var value [1]byte
		if _, err := file.Read(value[:]); err != nil {
			t.Fatal(err)
		}
		value[0] ^= 1
		if _, err := file.Seek(-1, 2); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(value[:]); err != nil {
			t.Fatal(err)
		}
		file.Close()
		if err := RepairSpool(options.Root); !errors.Is(err, ErrFrameCorrupt) {
			t.Fatalf("crc repair = %v", err)
		}
	})

	t.Run("non-tail", func(t *testing.T) {
		options := spoolTestOptions(t)
		options.WALSegmentBytes = 1
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
		if results[0].Spool.File == results[1].Spool.File {
			t.Fatal("test did not rotate WAL")
		}
		first := filepath.Join(options.Root, filepath.FromSlash(results[0].Spool.File))
		info, _ := os.Stat(first)
		if err := os.Truncate(first, info.Size()-1); err != nil {
			t.Fatal(err)
		}
		if err := RepairSpool(options.Root); !errors.Is(err, ErrFrameCorrupt) {
			t.Fatalf("non-tail repair = %v", err)
		}
	})
}
func TestSpoolReplayUsesGlobalIndexAcrossClockRollback(t *testing.T) {
	options := spoolTestOptions(t)
	var clockMu sync.Mutex
	now := time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC)
	options.Now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	first, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(1, "before rollback")})
	if err != nil {
		t.Fatal(err)
	}
	clockMu.Lock()
	now = now.Add(-time.Hour)
	clockMu.Unlock()
	second, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(2, "after rollback")})
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !(second[0].Spool.File < first[0].Spool.File) {
		t.Fatalf("test did not produce lexically reversed hour names: %q %q", first[0].Spool.File, second[0].Spool.File)
	}
	var sequences []int64
	if err := ReplaySpool(options.Root, SpoolPosition{}, func(record SpoolRecord, _ SpoolPosition) error {
		sequences = append(sequences, record.Envelope.Sequence)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 {
		t.Fatalf("rollback replay order = %v", sequences)
	}

	reopened, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(3, "continued")}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestRepairSpoolRejectsEmptyRootWithoutPanic(t *testing.T) {
	if err := RepairSpool(""); err == nil {
		t.Fatal("empty repair root was accepted")
	}
}

func TestSpoolObserverPanicIsIsolatedAndWorkerCloses(t *testing.T) {
	options := spoolTestOptions(t)
	options.Observe = func(SpoolStage) {
		panic("observer must not own durability")
	}
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	results, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(1, "safe")})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Record.Envelope.Sequence != 1 {
		t.Fatalf("results = %+v", results)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSpoolWorkersExitAcrossRepeatedOpenClose(t *testing.T) {
	for i := 0; i < 12; i++ {
		options := spoolTestOptions(t)
		spool, err := OpenSpool(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(1, "x")}); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err = spool.Close(ctx)
		cancel()
		if err != nil {
			t.Fatalf("close iteration %d: %v", i, err)
		}
	}
}
func TestSpoolReplayWhileOpenUsesQuiescentDurableBarrier(t *testing.T) {
	options := spoolTestOptions(t)
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	future, err := spool.Submit(context.Background(), spoolTestEnvelope(1, "live replay"))
	if err != nil {
		t.Fatal(err)
	}
	var replayed []int64
	if err := spool.Replay(context.Background(), SpoolPosition{}, func(record SpoolRecord, _ SpoolPosition) error {
		replayed = append(replayed, record.Envelope.Sequence)
		if string(record.Envelope.Payload) != "live replay" {
			t.Fatalf("payload = %q", record.Envelope.Payload)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 1 || replayed[0] != 1 {
		t.Fatalf("open replay = %v", replayed)
	}
	if result := waitDurable(t, future); result.Record.Envelope.Sequence != 1 {
		t.Fatalf("future = %+v", result)
	}
	if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{spoolTestEnvelope(2, "continued")}); err != nil {
		t.Fatal(err)
	}
	if err := spool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type spoolBlockingSyncFile struct {
	SpoolFile
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (f *spoolBlockingSyncFile) Sync() error {
	f.once.Do(func() { close(f.entered) })
	<-f.release
	return f.SpoolFile.Sync()
}

func TestSpoolCloseTimeoutJoinsOneBackgroundDrain(t *testing.T) {
	options := spoolTestOptions(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	options.OpenFile = func(name string, flag int, mode fs.FileMode) (SpoolFile, error) {
		file, err := os.OpenFile(name, flag, mode)
		if err != nil {
			return nil, err
		}
		if strings.HasSuffix(name, ".binpack") && flag&os.O_CREATE != 0 {
			return &spoolBlockingSyncFile{SpoolFile: file, entered: entered, release: release}, nil
		}
		return file, nil
	}
	spool, err := OpenSpool(options)
	if err != nil {
		t.Fatal(err)
	}
	future, err := spool.Submit(context.Background(), spoolTestEnvelope(1, "drain"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := spool.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed close = %v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("background drain did not reach raw Sync")
	}
	second := make(chan error, 1)
	go func() { second <- spool.Close(context.Background()) }()
	select {
	case err := <-second:
		t.Fatalf("second close returned before shared drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-second:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shared close did not finish")
	}
	if result := waitDurable(t, future); result.Record.Envelope.Sequence != 1 {
		t.Fatalf("future = %+v", result)
	}
}
func TestRepairSpoolRollsBackWALSuffixWhoseRawTailWasTruncated(t *testing.T) {
	root := t.TempDir()
	spoolDir := filepath.Join(root, "spool")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	rawName := "spool/raw-20260717T18-000001.binpack"
	walName := "spool/wal-20260717T18-000001.wal"
	rawOne, infoOne, err := EncodeRawFrame([]byte("one"), DefaultMaxPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	rawTwo, infoTwo, err := EncodeRawFrame([]byte("two"), DefaultMaxPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	rawBytes := append(append([]byte(nil), rawOne...), rawTwo[:len(rawTwo)-3]...)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rawName)), rawBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	firstRecord := SpoolRecord{
		Version:  ContractVersion,
		Envelope: spoolTestEnvelope(1, ""),
		Raw: RawRef{
			File: rawName, Offset: 0, Length: infoOne.EncodedLength, CRC32C: infoOne.CRC32C,
		},
	}
	firstRecord.Envelope.Payload = nil
	secondRecord := SpoolRecord{
		Version:  ContractVersion,
		Envelope: spoolTestEnvelope(2, ""),
		Raw: RawRef{
			File: rawName, Offset: infoOne.EncodedLength, Length: infoTwo.EncodedLength, CRC32C: infoTwo.CRC32C,
		},
	}
	secondRecord.Envelope.Payload = nil
	walOne, _, err := EncodeWALFrame(firstRecord, DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	walTwo, _, err := EncodeWALFrame(secondRecord, DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(walName)), append(walOne, walTwo...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RepairSpool(root); err != nil {
		t.Fatal(err)
	}
	rawStat, err := os.Stat(filepath.Join(root, filepath.FromSlash(rawName)))
	if err != nil {
		t.Fatal(err)
	}
	walStat, err := os.Stat(filepath.Join(root, filepath.FromSlash(walName)))
	if err != nil {
		t.Fatal(err)
	}
	if rawStat.Size() != int64(len(rawOne)) || walStat.Size() != int64(len(walOne)) {
		t.Fatalf("repaired sizes raw=%d/%d wal=%d/%d", rawStat.Size(), len(rawOne), walStat.Size(), len(walOne))
	}
	var sequences []int64
	if err := ReplaySpool(root, SpoolPosition{}, func(record SpoolRecord, _ SpoolPosition) error {
		sequences = append(sequences, record.Envelope.Sequence)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(sequences) != 1 || sequences[0] != 1 {
		t.Fatalf("replayed suffix = %v", sequences)
	}
}
func spoolTestEncodedAppendBytes(t *testing.T, envelope IngestEnvelope, rawFile string) int64 {
	t.Helper()
	rawFrame, rawInfo, err := EncodeRawFrame(envelope.Payload, DefaultMaxPayloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	record := SpoolRecord{
		Version:  ContractVersion,
		Envelope: cloneEnvelope(envelope),
		Raw: RawRef{
			File: rawFile, Offset: 0, Length: rawInfo.EncodedLength, CRC32C: rawInfo.CRC32C,
		},
	}
	record.Envelope.Payload = nil
	walFrame, _, err := EncodeWALFrame(record, DefaultMaxWALFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(rawFrame) + len(walFrame))
}

func TestSpoolDefaultTotalByteLimitSemantics(t *testing.T) {
	if DefaultSpoolMaxTotalBytes != int64(4<<30) {
		t.Fatalf("default total limit = %d", DefaultSpoolMaxTotalBytes)
	}
	defaults := DefaultSpoolOptions(t.TempDir())
	if defaults.MaxTotalBytes != DefaultSpoolMaxTotalBytes {
		t.Fatalf("default options total limit = %d", defaults.MaxTotalBytes)
	}
	normalized, err := normalizeSpoolOptions(SpoolOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.MaxTotalBytes != DefaultSpoolMaxTotalBytes {
		t.Fatalf("zero-value normalized limit = %d", normalized.MaxTotalBytes)
	}
}

func TestSpoolTotalByteLimitExactBoundaryAndRecoveredUsage(t *testing.T) {
	fixedNow := time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC)
	firstEnvelope := spoolTestEnvelope(1, "budget")
	firstBytes := spoolTestEncodedAppendBytes(t, firstEnvelope, "spool/raw-20260717T18-000001.binpack")

	t.Run("exact boundary", func(t *testing.T) {
		options := spoolTestOptions(t)
		options.Now = func() time.Time { return fixedNow }
		options.MaxTotalBytes = firstBytes
		spool, err := OpenSpool(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{firstEnvelope}); err != nil {
			t.Fatal(err)
		}
		if err := spool.Close(context.Background()); err != nil {
			t.Fatal(err)
		}

		tooSmall := options
		tooSmall.MaxTotalBytes = firstBytes - 1
		if _, err := OpenSpool(tooSmall); !errors.Is(err, ErrSpoolLimit) {
			t.Fatalf("open above configured limit = %v", err)
		}
	})

	t.Run("one byte below", func(t *testing.T) {
		options := spoolTestOptions(t)
		options.Now = func() time.Time { return fixedNow }
		options.MaxTotalBytes = firstBytes - 1
		spool, err := OpenSpool(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{firstEnvelope}); !errors.Is(err, ErrSpoolLimit) {
			t.Fatalf("one-byte-below append = %v", err)
		}
		if err := spool.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("restart counts existing segments", func(t *testing.T) {
		secondEnvelope := spoolTestEnvelope(2, "budget")
		secondBytes := spoolTestEncodedAppendBytes(t, secondEnvelope, "spool/raw-20260717T18-000002.binpack")
		options := spoolTestOptions(t)
		options.Now = func() time.Time { return fixedNow }
		options.MaxTotalBytes = firstBytes + secondBytes - 1
		spool, err := OpenSpool(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := spool.AppendBatch(context.Background(), []IngestEnvelope{firstEnvelope}); err != nil {
			t.Fatal(err)
		}
		if err := spool.Close(context.Background()); err != nil {
			t.Fatal(err)
		}

		reopened, err := OpenSpool(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reopened.AppendBatch(context.Background(), []IngestEnvelope{secondEnvelope}); !errors.Is(err, ErrSpoolLimit) {
			t.Fatalf("restart cumulative append = %v", err)
		}
		if err := reopened.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
