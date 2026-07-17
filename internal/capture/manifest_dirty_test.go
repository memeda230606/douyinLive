package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

func TestSQLiteRepositoryReporterFailureLeavesMarkerDirtyAndRestartClosesIt(t *testing.T) {
	ctx := context.Background()
	repository, store, layout, roomID, _ := openRepository(t)
	var firstEvents []ManifestHealthEvent
	repository.manifestReporter = ManifestHealthReporterFunc(func(event ManifestHealthEvent) error {
		firstEvents = append(firstEvents, event)
		if event.State == ManifestHealthRepairCleared {
			return errors.New("injected durable health log failure")
		}
		return nil
	})
	operationID := newV7(t)
	created, err := repository.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := manifestDirtyValue(t, store, created.ID); got != 1 {
		t.Fatalf("manifest_dirty after reporter failure = %d, want 1", got)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(created.DataPath), "session.json")
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read already-promoted manifest: %v", err)
	}
	if bytes.Contains(payload, []byte("manifestDirty")) {
		t.Fatalf("durability marker leaked into derivative manifest: %s", payload)
	}
	if len(firstEvents) != 2 || firstEvents[0].State != ManifestHealthRepairCleared || firstEvents[1].State != ManifestHealthRepairRequired {
		t.Fatalf("initial health events = %+v, want failed CLEARED then REQUIRED", firstEvents)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close original store: %v", err)
	}
	restartedStore, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer restartedStore.Close()
	recorder := &batchManifestHealthRecorder{}
	restarted, err := NewSQLiteRepositoryWithOptions(restartedStore.Writer(), restartedStore.Reader(), layout.Root, SQLiteRepositoryOptions{ManifestHealthReporter: recorder})
	if err != nil {
		t.Fatalf("NewSQLiteRepositoryWithOptions() error = %v", err)
	}
	report, err := restarted.RepairManifests(ctx)
	if err != nil || report != (ManifestRepairReport{Scanned: 1, Repaired: 1}) {
		t.Fatalf("RepairManifests() = (%+v, %v)", report, err)
	}
	if got := manifestDirtyValue(t, restartedStore, created.ID); got != 0 {
		t.Fatalf("manifest_dirty after restart acknowledgement = %d, want 0", got)
	}
	assertBatchHealthEvents(t, recorder.snapshot(), []ManifestHealthEvent{
		{SessionID: created.ID, State: ManifestHealthRepairRequired, ErrorCode: ManifestRepairRequiredErrorCode, Outstanding: 1},
		{SessionID: created.ID, State: ManifestHealthRepairCleared, ErrorCode: ManifestRepairClearedErrorCode, Outstanding: 0},
	})
}

func TestSQLiteRepositoryMarkerClearFailureKeepsDirtyUntilRetry(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	recorder := &batchManifestHealthRecorder{}
	repository.manifestReporter = recorder
	repository.beforeManifestMarkerClear = func() error { return errors.New("injected marker clear failure") }
	created, err := repository.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: newV7(t)})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got := manifestDirtyValue(t, store, created.ID); got != 1 {
		t.Fatalf("manifest_dirty after marker clear failure = %d, want 1", got)
	}
	repository.beforeManifestMarkerClear = nil
	if _, err := repository.Get(ctx, created.ID); err != nil {
		t.Fatalf("Get() retry error = %v", err)
	}
	if got := manifestDirtyValue(t, store, created.ID); got != 0 {
		t.Fatalf("manifest_dirty after retry = %d, want 0", got)
	}
	events := recorder.snapshot()
	if len(events) != 3 || events[0].State != ManifestHealthRepairCleared || events[1].State != ManifestHealthRepairRequired || events[2].State != ManifestHealthRepairCleared {
		t.Fatalf("marker clear retry events = %+v", events)
	}
}

func TestSQLiteRepositoryStaleCASDoesNotEmitCleared(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: newV7(t)})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := store.Writer().ExecContext(ctx, `UPDATE live_sessions SET manifest_dirty = 1, updated_at = ? WHERE id = ?`, created.UpdatedAt+1, created.ID); err != nil {
		t.Fatalf("advance database version: %v", err)
	}
	acknowledged := false
	cleared, err := repository.clearManifestDirty(ctx, created, func() error {
		acknowledged = true
		return nil
	})
	if err != nil || cleared {
		t.Fatalf("stale clearManifestDirty() = (%v, %v), want false,nil", cleared, err)
	}
	if acknowledged {
		t.Fatal("stale CAS emitted an old CLEARED acknowledgement")
	}
	if got := manifestDirtyValue(t, store, created.ID); got != 1 {
		t.Fatalf("newer manifest marker = %d, want 1", got)
	}
}

func TestSQLiteRepositoryTransitionUpdatedAtIsStrictlyMonotonic(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	operationID := newV7(t)
	created, err := repository.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	offset := int64(1)
	transitioned, err := repository.Transition(ctx, TransitionSessionInput{
		ID: created.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: operationID, Status: SessionStarting, RecordingStatus: RecordingPending,
		CaptureOffsetMS: &offset,
	})
	if err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	if transitioned.UpdatedAt != created.UpdatedAt+1 {
		t.Fatalf("UpdatedAt = %d, want %d", transitioned.UpdatedAt, created.UpdatedAt+1)
	}
}

func TestSQLiteRepositoryOldRepairPromotionCannotOverwriteNewTransition(t *testing.T) {
	ctx := context.Background()
	repository, store, layout, roomID, _ := openRepository(t)
	defer store.Close()
	operationID := newV7(t)
	created, err := repository.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(created.DataPath), "session.json")
	if err := os.WriteFile(manifestPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("make old manifest stale: %v", err)
	}
	promotionEntered := make(chan struct{})
	promotionRelease := make(chan struct{})
	var promotionCalls atomic.Int32
	repository.promoteManifest = func(staged *stagedManifest) error {
		if promotionCalls.Add(1) == 1 {
			close(promotionEntered)
			<-promotionRelease
		}
		return staged.promote()
	}
	repairResult := make(chan error, 1)
	go func() {
		_, getErr := repository.Get(ctx, created.ID)
		repairResult <- getErr
	}()
	select {
	case <-promotionEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("old repair did not reach promotion barrier")
	}
	nextOperationID := newV7(t)
	transitionStarted := make(chan struct{})
	transitionResult := make(chan error, 1)
	go func() {
		close(transitionStarted)
		_, transitionErr := repository.Transition(ctx, TransitionSessionInput{
			ID: created.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
			ExpectedOperationID: operationID, Status: SessionRecording, RecordingStatus: RecordingActive,
			NextOperationID: nextOperationID,
		})
		transitionResult <- transitionErr
	}()
	<-transitionStarted
	select {
	case err := <-transitionResult:
		t.Fatalf("new transition bypassed the session lock: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(promotionRelease)
	if err := <-repairResult; err != nil {
		t.Fatalf("old repair error = %v", err)
	}
	if err := <-transitionResult; err != nil {
		t.Fatalf("new transition error = %v", err)
	}
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read final manifest: %v", err)
	}
	var manifest LiveSession
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("decode final manifest: %v", err)
	}
	if manifest.OperationID != nextOperationID || manifest.Status != SessionRecording || manifest.RecordingStatus != RecordingActive {
		t.Fatalf("final manifest regressed to old repair: %+v", manifest)
	}
	if got := manifestDirtyValue(t, store, created.ID); got != 0 {
		t.Fatalf("manifest_dirty after serialized transition = %d, want 0", got)
	}
}

func TestSQLiteRepositoryBatchOutstandingCountsReachZero(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, now := openRepository(t)
	defer store.Close()
	sessions := insertDirtyTerminalSessions(t, store, roomID, now, 2)
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	recorder := &batchManifestHealthRecorder{}
	repository.manifestReporter = recorder
	report, err := repository.RepairManifests(ctx)
	if err != nil || report != (ManifestRepairReport{Scanned: 2, Repaired: 2}) {
		t.Fatalf("RepairManifests() = (%+v, %v)", report, err)
	}
	assertBatchHealthEvents(t, recorder.snapshot(), []ManifestHealthEvent{
		{SessionID: sessions[0].ID, State: ManifestHealthRepairRequired, ErrorCode: ManifestRepairRequiredErrorCode, Outstanding: 1},
		{SessionID: sessions[1].ID, State: ManifestHealthRepairRequired, ErrorCode: ManifestRepairRequiredErrorCode, Outstanding: 2},
		{SessionID: sessions[0].ID, State: ManifestHealthRepairCleared, ErrorCode: ManifestRepairClearedErrorCode, Outstanding: 1},
		{SessionID: sessions[1].ID, State: ManifestHealthRepairCleared, ErrorCode: ManifestRepairClearedErrorCode, Outstanding: 0},
	})
	begin, end := recorder.batchCounts()
	if begin != 1 || end != 1 {
		t.Fatalf("batch begin/end = %d/%d, want 1/1", begin, end)
	}
}

func TestSQLiteRepositoryRepairPaginates129DirtyTerminalSessions(t *testing.T) {
	ctx := context.Background()
	repository, store, layout, roomID, now := openRepository(t)
	defer store.Close()
	sessions := insertDirtyTerminalSessions(t, store, roomID, now, manifestRepairPageSize+1)
	recorder := &batchManifestHealthRecorder{}
	repository.manifestReporter = recorder
	want := ManifestRepairReport{Scanned: manifestRepairPageSize + 1, Repaired: manifestRepairPageSize + 1}
	report, err := repository.RepairManifests(ctx)
	if err != nil || report != want {
		t.Fatalf("RepairManifests() = (%+v, %v), want %+v", report, err, want)
	}
	begin, end := recorder.batchCounts()
	if begin != 2 || end != 2 {
		t.Fatalf("batch pages = %d/%d, want 2/2", begin, end)
	}
	var dirty int
	if err := store.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM live_sessions WHERE manifest_dirty = 1`).Scan(&dirty); err != nil || dirty != 0 {
		t.Fatalf("dirty rows after paged repair = %d, err=%v", dirty, err)
	}
	sample := sessions[len(sessions)-1]
	assertManifest(t, filepath.Join(layout.Root, filepath.FromSlash(sample.DataPath), "session.json"), sample.ID, sample.OperationID, RecordingCompleted)
	second, err := repository.RepairManifests(ctx)
	if err != nil || second != (ManifestRepairReport{}) {
		t.Fatalf("second RepairManifests() = (%+v, %v), want empty scan", second, err)
	}
	begin, end = recorder.batchCounts()
	if begin != 2 || end != 2 {
		t.Fatalf("healthy terminal rescan opened more batches: %d/%d", begin, end)
	}
}

func insertDirtyTerminalSessions(t *testing.T, store *storage.Store, roomID string, now time.Time, count int) []LiveSession {
	t.Helper()
	tx, err := store.Writer().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin dirty session seed: %v", err)
	}
	defer tx.Rollback()
	sessions := make([]LiveSession, 0, count)
	for index := 0; index < count; index++ {
		id := newV7(t)
		operationID := newV7(t)
		startedAt := now.Add(time.Duration(index) * time.Millisecond).UTC()
		endedAt := startedAt.Add(time.Millisecond).UnixMilli()
		dataPath := sessionDataPath(roomID, startedAt, id)
		updatedAt := startedAt.UnixMilli()
		if _, err := tx.Exec(`INSERT INTO live_sessions(
			id, room_config_id, operation_id, platform_room_id, title, status, recording_status, manifest_dirty,
			started_at, ended_at, media_epoch_at, capture_offset_ms, clock_source, integrity_score,
			data_path, schema_version, created_at, updated_at
		) VALUES (?, ?, ?, NULL, '', ?, ?, 1, ?, ?, NULL, 0, ?, 1, ?, ?, ?, ?)`,
			id, roomID, operationID, SessionCompleted, RecordingCompleted, startedAt.UnixMilli(), endedAt,
			ClockReceived, dataPath, SessionManifestSchemaVersion, updatedAt, updatedAt); err != nil {
			t.Fatalf("insert dirty terminal session %d: %v", index, err)
		}
		sessions = append(sessions, LiveSession{
			ID: id, RoomConfigID: roomID, OperationID: operationID, Status: SessionCompleted,
			RecordingStatus: RecordingCompleted, ManifestDirty: true, StartedAt: startedAt.UnixMilli(),
			EndedAt: &endedAt, ClockSource: ClockReceived, IntegrityScore: 1, DataPath: dataPath,
			SchemaVersion: SessionManifestSchemaVersion, CreatedAt: updatedAt, UpdatedAt: updatedAt,
		})
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit dirty session seed: %v", err)
	}
	return sessions
}

func manifestDirtyValue(t *testing.T, store *storage.Store, sessionID string) int {
	t.Helper()
	var dirty int
	if err := store.Reader().QueryRow(`SELECT manifest_dirty FROM live_sessions WHERE id = ?`, sessionID).Scan(&dirty); err != nil {
		t.Fatalf("read manifest_dirty: %v", err)
	}
	return dirty
}

type batchManifestHealthRecorder struct {
	mu     sync.Mutex
	events []ManifestHealthEvent
	begin  int
	end    int
}

func (r *batchManifestHealthRecorder) ReportManifestHealth(event ManifestHealthEvent) error {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
	return nil
}

func (r *batchManifestHealthRecorder) BeginManifestHealthBatch() error {
	r.mu.Lock()
	r.begin++
	r.mu.Unlock()
	return nil
}

func (r *batchManifestHealthRecorder) EndManifestHealthBatch() error {
	r.mu.Lock()
	r.end++
	r.mu.Unlock()
	return nil
}

func (r *batchManifestHealthRecorder) snapshot() []ManifestHealthEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ManifestHealthEvent(nil), r.events...)
}

func (r *batchManifestHealthRecorder) batchCounts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.begin, r.end
}

func assertBatchHealthEvents(t *testing.T, got, want []ManifestHealthEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("health event count = %d, want %d: %+v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("health event %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}

var _ ManifestHealthBatchReporter = (*batchManifestHealthRecorder)(nil)
