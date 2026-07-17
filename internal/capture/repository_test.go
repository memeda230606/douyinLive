package capture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

func TestSQLiteRepositoryCreateIsIdempotentAndWritesManifest(t *testing.T) {
	ctx := context.Background()
	repo, store, layout, roomID, now := openRepository(t)
	defer store.Close()
	operationID := newV7(t)
	created, err := repo.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: operationID, PlatformRoomID: "platform-room",
		Title: "测试场次", Recording: RecordingPending, StartedAt: now,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if parsed, err := uuid.Parse(created.ID); err != nil || parsed.Version() != 7 {
		t.Fatalf("created ID %q is not UUIDv7", created.ID)
	}
	wantPath := "rooms/" + roomID + "/sessions/2026/07/" + created.ID
	if created.DataPath != wantPath {
		t.Fatalf("DataPath = %q, want %q", created.DataPath, wantPath)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(created.DataPath), "session.json")
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile(session.json) error = %v", err)
	}
	var manifest LiveSession
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("decode session.json: %v", err)
	}
	if manifest.ID != created.ID || manifest.OperationID != operationID || manifest.SchemaVersion != SessionManifestSchemaVersion {
		t.Fatalf("manifest = %+v, want created session", manifest)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("remove manifest before Get repair: %v", err)
	}
	got, err := repo.Get(ctx, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("Get() = (%+v, %v)", got, err)
	}
	assertManifest(t, manifestPath, created.ID, operationID, RecordingPending)
	if err := os.WriteFile(manifestPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write stale manifest: %v", err)
	}

	retried, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil {
		t.Fatalf("idempotent Create() error = %v", err)
	}
	if retried.ID != created.ID {
		t.Fatalf("idempotent Create() ID = %q, want %q", retried.ID, created.ID)
	}
	assertManifest(t, manifestPath, created.ID, operationID, RecordingPending)
	if _, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: newV7(t)}); !errors.Is(err, ErrActiveSessionExists) {
		t.Fatalf("conflicting Create() error = %v, want ErrActiveSessionExists", err)
	}
}

func TestSQLiteRepositoryCreateCommitsBeforeManifestAndRetryRepairs(t *testing.T) {
	ctx := context.Background()
	repo, store, layout, roomID, _ := openRepository(t)
	defer store.Close()
	promoteErr := errors.New("injected manifest promote failure")
	recorder := &manifestHealthRecorder{}
	repo.manifestReporter = ManifestHealthReporterFunc(func(event ManifestHealthEvent) error {
		_ = repo.LastManifestError()
		return recorder.ReportManifestHealth(event)
	})
	repo.promoteManifest = func(*stagedManifest) error { return promoteErr }
	operationID := newV7(t)
	committed, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil {
		t.Fatalf("Create() returned error after committed promote failure: %v", err)
	}
	var databaseID string
	if scanErr := store.Reader().QueryRow(`SELECT id FROM live_sessions WHERE operation_id = ?`, operationID).Scan(&databaseID); scanErr != nil {
		t.Fatalf("committed session missing after manifest error: %v", scanErr)
	}
	if committed.ID != databaseID {
		t.Fatalf("Create() returned session ID %q, database has %q", committed.ID, databaseID)
	}
	issue := repo.LastManifestError()
	if !errors.Is(issue, ErrManifestRepairRequired) {
		t.Fatalf("LastManifestError() = %v, want ErrManifestRepairRequired", issue)
	}
	if errors.Is(issue, promoteErr) || strings.Contains(issue.Error(), promoteErr.Error()) {
		t.Fatalf("LastManifestError() leaked underlying error: %v", issue)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(committed.DataPath), "session.json")
	if _, err := os.Stat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest exists after injected promote failure: %v", err)
	}
	repo.promoteManifest = func(staged *stagedManifest) error { return staged.promote() }
	retried, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil {
		t.Fatalf("idempotent Create() repair error = %v", err)
	}
	if retried.ID != databaseID {
		t.Fatalf("retry created ID %q, want committed ID %q", retried.ID, databaseID)
	}
	assertManifest(t, manifestPath, databaseID, operationID, RecordingPending)
	if repo.LastManifestError() != nil {
		t.Fatalf("LastManifestError() after repair = %v", repo.LastManifestError())
	}
	assertManifestHealthEvents(t, recorder, []ManifestHealthEvent{
		{SessionID: committed.ID, State: ManifestHealthRepairRequired, ErrorCode: ManifestRepairRequiredErrorCode, Outstanding: 1},
		{SessionID: committed.ID, State: ManifestHealthRepairCleared, ErrorCode: ManifestRepairClearedErrorCode, Outstanding: 0},
	})
}

func TestSQLiteRepositoryManifestReporterPanicDoesNotChangeCommittedOutcome(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	repo.manifestReporter = ManifestHealthReporterFunc(func(ManifestHealthEvent) error {
		panic("injected reporter panic")
	})
	repo.promoteManifest = func(*stagedManifest) error {
		return errors.New("injected promote failure")
	}
	operationID := newV7(t)
	committed, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil || committed.ID == "" {
		t.Fatalf("Create() after reporter panic = (%+v, %v)", committed, err)
	}
	if !errors.Is(repo.LastManifestError(), ErrManifestRepairRequired) {
		t.Fatalf("LastManifestError() = %v, want ErrManifestRepairRequired", repo.LastManifestError())
	}
	var databaseID string
	if err := store.Reader().QueryRow(`SELECT id FROM live_sessions WHERE operation_id = ?`, operationID).Scan(&databaseID); err != nil {
		t.Fatalf("read committed session: %v", err)
	}
	if databaseID != committed.ID {
		t.Fatalf("database session ID = %q, want %q", databaseID, committed.ID)
	}
}

func TestSQLiteRepositoryManifestIssuesAreTrackedPerSession(t *testing.T) {
	ctx := context.Background()
	repo, store, _, firstRoomID, _ := openRepository(t)
	defer store.Close()
	secondRoomID := insertRoom(t, store, "second-manifest-issue")
	firstOperationID := newV7(t)
	secondOperationID := newV7(t)
	firstErr := errors.New("first session promote failure")
	secondErr := errors.New("second session promote failure")
	recorder := &manifestHealthRecorder{}
	repo.manifestReporter = recorder

	repo.promoteManifest = func(*stagedManifest) error { return firstErr }
	first, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: firstRoomID, OperationID: firstOperationID})
	if err != nil {
		t.Fatalf("Create(first) returned committed promote error: %v", err)
	}
	repo.promoteManifest = func(*stagedManifest) error { return secondErr }
	second, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: secondRoomID, OperationID: secondOperationID})
	if err != nil {
		t.Fatalf("Create(second) returned committed promote error: %v", err)
	}
	if issue := repo.LastManifestError(); !errors.Is(issue, ErrManifestRepairRequired) || !strings.Contains(issue.Error(), first.ID) || !strings.Contains(issue.Error(), second.ID) || strings.Contains(issue.Error(), firstErr.Error()) || strings.Contains(issue.Error(), secondErr.Error()) {
		t.Fatalf("LastManifestError() is not a sanitized aggregate: %v", issue)
	}

	repo.promoteManifest = func(staged *stagedManifest) error { return staged.promote() }
	if _, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: secondRoomID, OperationID: secondOperationID}); err != nil {
		t.Fatalf("repair second manifest: %v", err)
	}
	if issue := repo.LastManifestError(); !errors.Is(issue, ErrManifestRepairRequired) || !strings.Contains(issue.Error(), first.ID) || strings.Contains(issue.Error(), second.ID) {
		t.Fatalf("repairing second session changed first issue incorrectly: %v", issue)
	}
	if _, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: firstRoomID, OperationID: firstOperationID}); err != nil {
		t.Fatalf("repair first manifest: %v", err)
	}
	if issue := repo.LastManifestError(); issue != nil {
		t.Fatalf("LastManifestError() after both repairs = %v", issue)
	}
	assertManifestHealthEvents(t, recorder, []ManifestHealthEvent{
		{SessionID: first.ID, State: ManifestHealthRepairRequired, ErrorCode: ManifestRepairRequiredErrorCode, Outstanding: 1},
		{SessionID: second.ID, State: ManifestHealthRepairRequired, ErrorCode: ManifestRepairRequiredErrorCode, Outstanding: 2},
		{SessionID: second.ID, State: ManifestHealthRepairCleared, ErrorCode: ManifestRepairClearedErrorCode, Outstanding: 1},
		{SessionID: first.ID, State: ManifestHealthRepairCleared, ErrorCode: ManifestRepairClearedErrorCode, Outstanding: 0},
	})
}

func TestSQLiteRepositoryRepairManifestsAfterRestart(t *testing.T) {
	ctx := context.Background()
	firstRepository, store, layout, roomID, _ := openRepository(t)
	operationID := newV7(t)
	firstRepository.promoteManifest = func(*stagedManifest) error {
		return errors.New(`sensitive promote error at C:\Users\private\session.json`)
	}
	committed, err := firstRepository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: operationID,
	})
	if err != nil {
		store.Close()
		t.Fatalf("Create() returned committed promote error: %v", err)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(committed.DataPath), "session.json")
	if _, err := os.Stat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		store.Close()
		t.Fatalf("manifest exists before restart repair: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close original store: %v", err)
	}

	restartedStore, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer restartedStore.Close()
	recorder := &manifestHealthRecorder{}
	restartedRepository, err := NewSQLiteRepositoryWithOptions(
		restartedStore.Writer(), restartedStore.Reader(), layout.Root,
		SQLiteRepositoryOptions{ManifestHealthReporter: recorder},
	)
	if err != nil {
		t.Fatalf("NewSQLiteRepositoryWithOptions() error = %v", err)
	}
	report, err := restartedRepository.RepairManifests(ctx)
	if err != nil {
		t.Fatalf("RepairManifests() error = %v", err)
	}
	if want := (ManifestRepairReport{Scanned: 1, Repaired: 1}); report != want {
		t.Fatalf("RepairManifests() report = %+v, want %+v", report, want)
	}
	assertManifest(t, manifestPath, committed.ID, operationID, RecordingPending)
	if issue := restartedRepository.LastManifestError(); issue != nil {
		t.Fatalf("LastManifestError() after restart repair = %v", issue)
	}
	assertManifestHealthEvents(t, recorder, []ManifestHealthEvent{
		{SessionID: committed.ID, State: ManifestHealthRepairRequired, ErrorCode: ManifestRepairRequiredErrorCode, Outstanding: 1},
		{SessionID: committed.ID, State: ManifestHealthRepairCleared, ErrorCode: ManifestRepairClearedErrorCode, Outstanding: 0},
	})

	secondReport, err := restartedRepository.RepairManifests(ctx)
	if err != nil || secondReport != (ManifestRepairReport{Scanned: 1}) {
		t.Fatalf("second RepairManifests() = (%+v, %v)", secondReport, err)
	}
	if got := len(recorder.snapshot()); got != 2 {
		t.Fatalf("healthy rescan emitted events: %+v", recorder.snapshot())
	}
}

func TestSQLiteRepositoryCreateStageFailureLeavesDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	repo, store, layout, roomID, _ := openRepository(t)
	defer store.Close()
	if err := os.RemoveAll(layout.RoomsDir); err != nil {
		t.Fatalf("remove rooms directory: %v", err)
	}
	if err := os.WriteFile(layout.RoomsDir, []byte("block manifest staging"), 0o600); err != nil {
		t.Fatalf("block rooms directory: %v", err)
	}
	operationID := newV7(t)
	if _, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID}); err == nil {
		t.Fatal("Create() succeeded when manifest staging failed")
	}
	var count int
	if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM live_sessions WHERE operation_id = ?`, operationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("session count after stage failure = %d, err = %v", count, err)
	}
}

func TestSQLiteRepositoryConcurrentCreateHasOneActiveSession(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	const workers = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		operationID := newV7(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			session, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
			if err != nil {
				errs <- err
				return
			}
			ids <- session.ID
		}()
	}
	close(start)
	wg.Wait()
	close(ids)
	close(errs)
	if got := len(ids); got != 1 {
		t.Fatalf("successful Create() count = %d, want 1", got)
	}
	for err := range errs {
		if !errors.Is(err, ErrActiveSessionExists) {
			t.Fatalf("concurrent Create() error = %v, want ErrActiveSessionExists", err)
		}
	}
	active, found, err := repo.ActiveForRoom(ctx, roomID)
	if err != nil || !found || active.ID == "" {
		t.Fatalf("ActiveForRoom() = (%+v, %v, %v)", active, found, err)
	}
}

func TestSQLiteRepositoryTransitionCASAndOperationSwitch(t *testing.T) {
	ctx := context.Background()
	repo, store, layout, roomID, now := openRepository(t)
	defer store.Close()
	operationID := newV7(t)
	created, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	nextOperationID := newV7(t)
	clock := ClockMedia
	mediaEpoch := now.Add(time.Second)
	offset := int64(250)
	integrity := 0.95
	transitioned, err := repo.Transition(ctx, TransitionSessionInput{
		ID: created.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: operationID, Status: SessionRecording, RecordingStatus: RecordingUnavailable,
		NextOperationID: nextOperationID, MediaEpochAt: &mediaEpoch, CaptureOffsetMS: &offset,
		ClockSource: &clock, IntegrityScore: &integrity,
	})
	if err != nil {
		t.Fatalf("Transition() error = %v", err)
	}
	if transitioned.OperationID != nextOperationID || transitioned.Status != SessionRecording || transitioned.RecordingStatus != RecordingUnavailable {
		t.Fatalf("Transition() = %+v", transitioned)
	}
	if _, err := repo.Transition(ctx, TransitionSessionInput{
		ID: created.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: operationID, Status: SessionRecording, RecordingStatus: RecordingActive,
	}); !errors.Is(err, ErrStaleTransition) {
		t.Fatalf("stale Transition() error = %v, want ErrStaleTransition", err)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(created.DataPath), "session.json")
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read transitioned manifest: %v", err)
	}
	var manifest LiveSession
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("decode transitioned manifest: %v", err)
	}
	if manifest.OperationID != nextOperationID || manifest.RecordingStatus != RecordingUnavailable {
		t.Fatalf("transitioned manifest = %+v", manifest)
	}
	if err := os.WriteFile(manifestPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write stale transitioned manifest: %v", err)
	}
	replayed, err := repo.Transition(ctx, TransitionSessionInput{
		ID: created.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: operationID, Status: SessionRecording, RecordingStatus: RecordingUnavailable,
		NextOperationID: nextOperationID, MediaEpochAt: &mediaEpoch, CaptureOffsetMS: &offset,
		ClockSource: &clock, IntegrityScore: &integrity,
	})
	if err != nil {
		t.Fatalf("idempotent Transition() replay error = %v", err)
	}
	assertManifest(t, manifestPath, replayed.ID, nextOperationID, RecordingUnavailable)
}

func TestSQLiteRepositoryTransitionCommitSurvivesManifestFailureAndReplayRepairs(t *testing.T) {
	ctx := context.Background()
	repo, store, layout, roomID, _ := openRepository(t)
	defer store.Close()
	operationID := newV7(t)
	created, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: operationID})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(created.DataPath), "session.json")
	promoteErr := errors.New("injected transition promote failure")
	repo.promoteManifest = func(*stagedManifest) error { return promoteErr }
	nextOperationID := newV7(t)
	committed, err := repo.Transition(ctx, TransitionSessionInput{
		ID: created.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: operationID, Status: SessionRecording, RecordingStatus: RecordingActive,
		NextOperationID: nextOperationID,
	})
	if err != nil {
		t.Fatalf("Transition() returned error after committed promote failure: %v", err)
	}
	if committed.OperationID != nextOperationID || committed.RecordingStatus != RecordingActive {
		t.Fatalf("Transition() did not return committed state: %+v", committed)
	}
	database, scanErr := querySession(ctx, store.Reader(), sessionSelectSQL+` WHERE id = ?`, created.ID)
	if scanErr != nil {
		t.Fatalf("read committed transition: %v", scanErr)
	}
	if database.OperationID != nextOperationID || database.RecordingStatus != RecordingActive {
		t.Fatalf("database transition = %+v", database)
	}
	issue := repo.LastManifestError()
	if !errors.Is(issue, ErrManifestRepairRequired) {
		t.Fatalf("LastManifestError() = %v, want ErrManifestRepairRequired", issue)
	}
	if errors.Is(issue, promoteErr) || strings.Contains(issue.Error(), promoteErr.Error()) {
		t.Fatalf("LastManifestError() leaked underlying error: %v", issue)
	}
	repo.promoteManifest = func(staged *stagedManifest) error { return staged.promote() }
	replayed, err := repo.Transition(ctx, TransitionSessionInput{
		ID: created.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: operationID, Status: SessionRecording, RecordingStatus: RecordingActive,
		NextOperationID: nextOperationID,
	})
	if err != nil {
		t.Fatalf("replay committed Transition() error = %v", err)
	}
	assertManifest(t, manifestPath, replayed.ID, nextOperationID, RecordingActive)
	if repo.LastManifestError() != nil {
		t.Fatalf("LastManifestError() after transition repair = %v", repo.LastManifestError())
	}
}

func TestSQLiteRepositoryEmptyExpectedOperationRequiresClaimID(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: newV7(t)})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := repo.Transition(ctx, TransitionSessionInput{
		ID: created.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: "", Status: SessionInterrupted, RecordingStatus: RecordingIncomplete,
	}); err == nil {
		t.Fatal("empty expected operation without next claim ID succeeded")
	}
}

func TestSQLiteRepositoryTerminalSessionAllowsNewSessionAndRecoveryQuery(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, now := openRepository(t)
	defer store.Close()
	firstOperation := newV7(t)
	first, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: firstOperation})
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	endedAt := now.Add(5 * time.Minute)
	completed, err := repo.Transition(ctx, TransitionSessionInput{
		ID: first.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: firstOperation, Status: SessionCompleted, RecordingStatus: RecordingCompleted,
		EndedAt: &endedAt,
	})
	if err != nil {
		t.Fatalf("complete first session: %v", err)
	}
	if completed.EndedAt == nil || *completed.EndedAt != endedAt.UnixMilli() {
		t.Fatalf("completed EndedAt = %v", completed.EndedAt)
	}
	second, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingDisabled})
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("new active session reused terminal session ID")
	}
	secondManifest := filepath.Join(repo.dataRoot, filepath.FromSlash(second.DataPath), "session.json")
	if err := os.WriteFile(secondManifest, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write stale active manifest: %v", err)
	}
	if _, found, err := repo.ActiveForRoom(ctx, roomID); err != nil || !found {
		t.Fatalf("ActiveForRoom() repair = (found %v, err %v)", found, err)
	}
	assertManifest(t, secondManifest, second.ID, second.OperationID, RecordingDisabled)
	if err := os.WriteFile(secondManifest, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write stale recoverable manifest: %v", err)
	}
	recoverable, err := repo.ListRecoverable(ctx)
	if err != nil {
		t.Fatalf("ListRecoverable() error = %v", err)
	}
	if len(recoverable) != 1 || recoverable[0].ID != second.ID {
		t.Fatalf("ListRecoverable() = %+v, want second session only", recoverable)
	}
	assertManifest(t, secondManifest, second.ID, second.OperationID, RecordingDisabled)
}

func TestSQLiteRepositoryOperationIDIsGloballyUnique(t *testing.T) {
	ctx := context.Background()
	repo, store, _, roomID, now := openRepository(t)
	defer store.Close()
	sharedOperation := newV7(t)
	first, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: roomID, OperationID: sharedOperation})
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	endedAt := now.Add(time.Minute)
	if _, err := repo.Transition(ctx, TransitionSessionInput{
		ID: first.ID, ExpectedStatus: SessionStarting, ExpectedRecordingStatus: RecordingPending,
		ExpectedOperationID: sharedOperation, Status: SessionCompleted, RecordingStatus: RecordingCompleted,
		EndedAt: &endedAt,
	}); err != nil {
		t.Fatalf("complete first: %v", err)
	}
	otherRoom := insertRoom(t, store, "other")
	if _, err := repo.Create(ctx, CreateSessionInput{RoomConfigID: otherRoom, OperationID: sharedOperation}); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("reused operation for another room error = %v, want ErrOperationConflict", err)
	}
}

func TestSecureManifestPathRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	for _, value := range []string{"", "../escape", "rooms/../escape", `/absolute`, `rooms\escape`} {
		if _, err := secureManifestPath(root, value); err == nil {
			t.Fatalf("secureManifestPath(%q) error = nil", value)
		}
	}
}

func openRepository(t *testing.T) (*SQLiteRepository, *storage.Store, storage.Layout, string, time.Time) {
	t.Helper()
	ctx := context.Background()
	layout, err := storage.PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	now := time.Date(2026, 7, 17, 8, 30, 0, 0, time.UTC)
	repo, err := newSQLiteRepository(store.Writer(), store.Reader(), layout.Root, func() time.Time { return now })
	if err != nil {
		store.Close()
		t.Fatalf("newSQLiteRepository() error = %v", err)
	}
	roomID := insertRoom(t, store, "primary")
	return repo, store, layout, roomID, now
}

func insertRoom(t *testing.T, store *storage.Store, suffix string) string {
	t.Helper()
	roomID := newV7(t)
	if _, err := store.Writer().Exec(`INSERT INTO rooms(
		id, live_id, alias, created_at, updated_at
	) VALUES (?, ?, ?, 1, 1)`, roomID, "live-"+suffix+"-"+roomID, "room-"+suffix); err != nil {
		t.Fatalf("insert room: %v", err)
	}
	return roomID
}

func newV7(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7() error = %v", err)
	}
	return id.String()
}

func assertManifest(t *testing.T, manifestPath, sessionID, operationID string, recordingStatus RecordingStatus) {
	t.Helper()
	payload, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read session manifest: %v", err)
	}
	var manifest LiveSession
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatalf("decode session manifest: %v", err)
	}
	if manifest.ID != sessionID || manifest.OperationID != operationID || manifest.RecordingStatus != recordingStatus {
		t.Fatalf("manifest = %+v, want id=%q operation=%q recording=%q", manifest, sessionID, operationID, recordingStatus)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(manifestPath), ".session-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("manifest temporary files = %v, err = %v", matches, err)
	}
}

type manifestHealthRecorder struct {
	mu     sync.Mutex
	events []ManifestHealthEvent
}

func (recorder *manifestHealthRecorder) ReportManifestHealth(event ManifestHealthEvent) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.events = append(recorder.events, event)
	return nil
}

func (recorder *manifestHealthRecorder) snapshot() []ManifestHealthEvent {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]ManifestHealthEvent(nil), recorder.events...)
}

func assertManifestHealthEvents(t *testing.T, recorder *manifestHealthRecorder, want []ManifestHealthEvent) {
	t.Helper()
	got := recorder.snapshot()
	if len(got) != len(want) {
		t.Fatalf("manifest health event count = %d, want %d: %+v", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("manifest health event %d = %+v, want %+v", index, got[index], want[index])
		}
	}
}
