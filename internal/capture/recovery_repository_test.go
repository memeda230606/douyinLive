package capture

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestSQLiteRepositoryListRecoverablePageUsesFixedCutoffAndKeyset(t *testing.T) {
	ctx := context.Background()
	repository, store, layout, _, _ := openRepository(t)
	defer store.Close()
	store.Reader().SetMaxOpenConns(1)

	const (
		recoverableCount = 130
		scanCutoffMS     = int64(2_000)
	)
	wantIDs := insertRecoverableSessions(
		t, store.Writer(), recoverableCount, 1_000, true,
	)
	insertRecoverySessionRow(
		t, store.Writer(), 4_000, SessionCompleted, RecordingCompleted, false,
	)
	insertRecoverySessionRow(
		t, store.Writer(), scanCutoffMS+1, SessionStarting, RecordingStarting, false,
	)

	var gotIDs []string
	afterID := ""
	for {
		page, err := repository.ListRecoverablePage(ctx, RecoverablePageQuery{
			ScanCutoffMS: scanCutoffMS,
			AfterID:      afterID,
			Limit:        17,
		})
		if err != nil {
			t.Fatalf("ListRecoverablePage() error = %v", err)
		}
		if len(gotIDs) == 0 {
			// A session created after the fixed startup cutoff remains outside
			// this pass even when it appears between page calls.
			insertRecoverySessionRow(
				t, store.Writer(), scanCutoffMS+2,
				SessionRecording, RecordingActive, false,
			)
		}
		for _, session := range page.Sessions {
			if session.CreatedAt > scanCutoffMS {
				t.Fatalf("page included post-cutoff session: %+v", session)
			}
			gotIDs = append(gotIDs, session.ID)
		}
		if page.NextID == "" {
			break
		}
		if afterID != "" && page.NextID <= afterID {
			t.Fatalf("cursor did not advance: previous=%q next=%q", afterID, page.NextID)
		}
		afterID = page.NextID
	}

	sort.Strings(wantIDs)
	if len(gotIDs) != recoverableCount {
		t.Fatalf("recoverable count = %d, want %d", len(gotIDs), recoverableCount)
	}
	for index := range wantIDs {
		if gotIDs[index] != wantIDs[index] {
			t.Fatalf("recoverable ID %d = %q, want %q", index, gotIDs[index], wantIDs[index])
		}
	}
	var one int
	if err := store.Reader().QueryRow("SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("reader unavailable after page rows closed: (%d, %v)", one, err)
	}
	validPath := filepath.Join(
		layout.Root,
		filepath.FromSlash("rooms/no-manifest/sessions/2026/07/no-manifest"),
		"session.json",
	)
	if _, err := os.Stat(validPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pagination materialized a manifest: %v", err)
	}
}

func TestSQLiteRepositoryListRecoverablePageValidatesBounds(t *testing.T) {
	repository, store, _, _, _ := openRepository(t)
	defer store.Close()
	tests := []RecoverablePageQuery{
		{ScanCutoffMS: 1, Limit: 0},
		{ScanCutoffMS: 1, Limit: maximumRecoverablePageSize + 1},
		{ScanCutoffMS: 0, Limit: 1},
		{ScanCutoffMS: 1, AfterID: "not-a-uuid", Limit: 1},
	}
	for _, input := range tests {
		if _, err := repository.ListRecoverablePage(
			context.Background(), input,
		); !errors.Is(err, ErrRecoveryContractInvalid) {
			t.Fatalf("ListRecoverablePage(%+v) error = %v, want contract error", input, err)
		}
	}
}

func TestSQLiteRepositoryRecoverAndCloseCommitsSessionGapsAndManifest(t *testing.T) {
	ctx := context.Background()
	repository, store, layout, roomID, _ := openRepository(t)
	defer store.Close()
	operationID := newV7(t)
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  operationID,
		Recording:    RecordingStarting,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	input := recoveryCloseInput(t, created)
	closed, err := repository.RecoverAndClose(ctx, input)
	if err != nil {
		t.Fatalf("RecoverAndClose() error = %v", err)
	}
	if closed.Status != SessionInterrupted ||
		closed.RecordingStatus != RecordingIncomplete ||
		closed.OperationID != input.RecoveryOperationID ||
		closed.EndedAt == nil || *closed.EndedAt != input.EndedAtMS ||
		closed.IntegrityScore != input.IntegrityScore {
		t.Fatalf("RecoverAndClose() = %+v", closed)
	}

	var (
		kind, reason, details  string
		startedAt, endedAt     int64
		startOffset, endOffset int64
		recovered              int
	)
	err = store.Reader().QueryRow(
		"SELECT kind, started_at, ended_at, start_offset_ms, end_offset_ms, "+
			"recovered, reason_code, details_json FROM capture_gaps "+
			"WHERE session_id = ? AND dedupe_key = ?",
		created.ID, input.Gaps[0].DedupeKey,
	).Scan(
		&kind, &startedAt, &endedAt, &startOffset, &endOffset,
		&recovered, &reason, &details,
	)
	if err != nil {
		t.Fatalf("read recovery gap: %v", err)
	}
	if kind != "process_crash" ||
		startedAt != input.Gaps[0].StartedAtMS ||
		endedAt != *input.Gaps[0].EndedAtMS ||
		startOffset != input.Gaps[0].StartedAtMS-created.StartedAt ||
		endOffset != *input.Gaps[0].EndedAtMS-created.StartedAt ||
		recovered != 0 || reason != input.Gaps[0].ReasonCode ||
		details != "{\"source\":\"startup_recovery\"}" {
		t.Fatalf(
			"recovery gap = (%q,%d,%d,%d,%d,%d,%q,%q)",
			kind, startedAt, endedAt, startOffset, endOffset,
			recovered, reason, details,
		)
	}
	manifestPath := filepath.Join(
		layout.Root, filepath.FromSlash(created.DataPath), "session.json",
	)
	manifest := readRecoveryManifest(t, manifestPath)
	if manifest.Status != SessionInterrupted ||
		manifest.OperationID != input.RecoveryOperationID ||
		manifest.RecordingStatus != RecordingIncomplete {
		t.Fatalf("recovery manifest = %+v", manifest)
	}

	if _, err := repository.Transition(ctx, TransitionSessionInput{
		ID:                      created.ID,
		ExpectedStatus:          created.Status,
		ExpectedRecordingStatus: created.RecordingStatus,
		ExpectedOperationID:     created.OperationID,
		Status:                  SessionRecording,
		RecordingStatus:         RecordingActive,
	}); !errors.Is(err, ErrStaleTransition) {
		t.Fatalf("old operation Transition() error = %v, want ErrStaleTransition", err)
	}
	next, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingDisabled,
	})
	if err != nil {
		t.Fatalf("Create() after recovery error = %v", err)
	}
	if next.ID == created.ID {
		t.Fatal("post-recovery session reused the old session ID")
	}
}

func TestSQLiteRepositoryRecoverAndCloseClosesExistingOpenGapsAndPreservesClosed(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingActive,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	input := recoveryCloseInput(t, created)
	openRecordingID := insertExistingRecoveryGap(
		t, store.Writer(), created, "recording_restart",
		created.StartedAt+200, nil, false,
		"RECORDER_NETWORK_FAILURE", "{\"attempts\":2}",
		"existing-recording-restart:"+created.ID,
	)
	openEventID := insertExistingRecoveryGap(
		t, store.Writer(), created, "event_persistence",
		created.StartedAt+300, nil, true,
		"EVENT_PERSISTENCE_DEGRADED", "{\"batches\":1}",
		"existing-event-gap:"+created.ID,
	)
	closedEndedAt := created.StartedAt + 400
	closedID := insertExistingRecoveryGap(
		t, store.Writer(), created, "message_disconnect",
		created.StartedAt+350, &closedEndedAt, true,
		"MESSAGE_CONNECTION_RESTORED", "{\"closed\":true}",
		"existing-closed-gap:"+created.ID,
	)

	if _, err := repository.RecoverAndClose(ctx, input); err != nil {
		t.Fatalf("RecoverAndClose() error = %v", err)
	}
	wantEndOffset := input.EndedAtMS - created.StartedAt
	openGaps := []struct {
		id, kind, reason, details, dedupeKey string
	}{
		{
			openRecordingID, "recording_restart", "RECORDER_NETWORK_FAILURE",
			"{\"attempts\":2}", "existing-recording-restart:" + created.ID,
		},
		{
			openEventID, "event_persistence", "EVENT_PERSISTENCE_DEGRADED",
			"{\"batches\":1}", "existing-event-gap:" + created.ID,
		},
	}
	for _, want := range openGaps {
		var (
			endedAt, endOffset int64
			recovered          int
			kind, severity     string
			reason, details    string
			dedupeKey          string
		)
		if err := store.Reader().QueryRow(
			"SELECT ended_at, end_offset_ms, recovered, kind, severity, "+
				"reason_code, details_json, dedupe_key FROM capture_gaps WHERE id = ?",
			want.id,
		).Scan(
			&endedAt, &endOffset, &recovered, &kind, &severity,
			&reason, &details, &dedupeKey,
		); err != nil {
			t.Fatalf("read closed existing gap %q: %v", want.id, err)
		}
		if endedAt != input.EndedAtMS || endOffset != wantEndOffset || recovered != 0 ||
			kind != want.kind || severity != "warning" || reason != want.reason ||
			details != want.details || dedupeKey != want.dedupeKey {
			t.Fatalf(
				"closed existing gap %q = ended:%d offset:%d recovered:%d kind:%q severity:%q reason:%q details:%q dedupe:%q",
				want.id, endedAt, endOffset, recovered, kind, severity,
				reason, details, dedupeKey,
			)
		}
	}
	var (
		closedStoredEndedAt, closedStoredEndOffset int64
		closedRecovered                            int
		closedReason, closedDetails                string
	)
	if err := store.Reader().QueryRow(
		"SELECT ended_at, end_offset_ms, recovered, reason_code, details_json "+
			"FROM capture_gaps WHERE id = ?",
		closedID,
	).Scan(
		&closedStoredEndedAt, &closedStoredEndOffset, &closedRecovered,
		&closedReason, &closedDetails,
	); err != nil {
		t.Fatalf("read pre-closed gap: %v", err)
	}
	if closedStoredEndedAt != closedEndedAt ||
		closedStoredEndOffset != closedEndedAt-created.StartedAt ||
		closedRecovered != 1 || closedReason != "MESSAGE_CONNECTION_RESTORED" ||
		closedDetails != "{\"closed\":true}" {
		t.Fatalf(
			"pre-closed gap changed: ended=%d offset=%d recovered=%d reason=%q details=%q",
			closedStoredEndedAt, closedStoredEndOffset, closedRecovered,
			closedReason, closedDetails,
		)
	}
	if _, err := repository.RecoverAndClose(ctx, input); err != nil {
		t.Fatalf("idempotent RecoverAndClose() error = %v", err)
	}
	assertRecoveryOpenGapCount(t, store.Reader(), created.ID, 0)
}

func TestSQLiteRepositoryRecoverAndCloseRejectsInvertedExistingOpenGapAtomically(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingActive,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	input := recoveryCloseInput(t, created)
	openID := insertExistingRecoveryGap(
		t, store.Writer(), created, "recording_restart",
		input.EndedAtMS+1, nil, false,
		"RECORDER_PROCESS_EXITED", "{}", "inverted-open-gap:"+created.ID,
	)
	if _, err := repository.RecoverAndClose(
		ctx, input,
	); !errors.Is(err, ErrRecoveryContractInvalid) {
		t.Fatalf("RecoverAndClose() error = %v, want contract error", err)
	}
	current, err := querySession(
		ctx, store.Reader(), sessionSelectSQL+" WHERE id = ?", created.ID,
	)
	if err != nil {
		t.Fatalf("read rolled-back session: %v", err)
	}
	if current.Status != created.Status ||
		current.RecordingStatus != created.RecordingStatus ||
		current.OperationID != created.OperationID || current.EndedAt != nil {
		t.Fatalf("inverted gap changed session: %+v", current)
	}
	var endedAt sql.NullInt64
	if err := store.Reader().QueryRow(
		"SELECT ended_at FROM capture_gaps WHERE id = ?", openID,
	).Scan(&endedAt); err != nil {
		t.Fatalf("read inverted gap: %v", err)
	}
	if endedAt.Valid {
		t.Fatalf("inverted gap was closed at %d", endedAt.Int64)
	}
	var startupGapCount int
	if err := store.Reader().QueryRow(
		"SELECT COUNT(*) FROM capture_gaps WHERE session_id = ? AND dedupe_key = ?",
		created.ID, input.Gaps[0].DedupeKey,
	).Scan(&startupGapCount); err != nil {
		t.Fatalf("count rolled-back startup gap: %v", err)
	}
	if startupGapCount != 0 {
		t.Fatalf("inverted recovery persisted %d startup gaps", startupGapCount)
	}
}

func TestSQLiteRepositoryRecoverAndCloseAllowsEndedAtAfterScanCutoff(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingActive,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	input := recoveryCloseInput(t, created)
	input.ScanCutoffMS = created.CreatedAt - 1
	input.EndedAtMS = max(created.StartedAt, created.CreatedAt) + 5_000
	if _, err := repository.RecoverAndClose(
		ctx, input,
	); !errors.Is(err, ErrStaleRecovery) {
		t.Fatalf("RecoverAndClose(pre-creation scan cutoff) error = %v, want stale", err)
	}
	input.ScanCutoffMS = created.CreatedAt
	closed, err := repository.RecoverAndClose(ctx, input)
	if err != nil {
		t.Fatalf("RecoverAndClose(future timeline cutoff) error = %v", err)
	}
	if closed.EndedAt == nil || *closed.EndedAt != input.EndedAtMS ||
		*closed.EndedAt <= input.ScanCutoffMS {
		t.Fatalf("future timeline cutoff was not preserved: %+v", closed)
	}
}

func TestSQLiteRepositoryRecoverAndClosePreservesDisabledRecording(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingDisabled,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	input := recoveryCloseInput(t, created)
	input.Gaps = nil
	closed, err := repository.RecoverAndClose(ctx, input)
	if err != nil {
		t.Fatalf("RecoverAndClose() error = %v", err)
	}
	if closed.RecordingStatus != RecordingDisabled {
		t.Fatalf("recording status = %q, want disabled", closed.RecordingStatus)
	}
}

func TestSQLiteRepositoryRecoverAndCloseIsIdempotentAndRepairsManifest(t *testing.T) {
	ctx := context.Background()
	repository, store, layout, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingActive,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	input := recoveryCloseInput(t, created)
	if _, err := repository.RecoverAndClose(ctx, input); err != nil {
		t.Fatalf("initial RecoverAndClose() error = %v", err)
	}
	manifestPath := filepath.Join(
		layout.Root, filepath.FromSlash(created.DataPath), "session.json",
	)
	if err := os.Remove(manifestPath); err != nil {
		t.Fatalf("remove recovery manifest: %v", err)
	}
	replay := input
	replay.Gaps = append([]RecoveryGapInput(nil), input.Gaps...)
	replay.Gaps[0].ID = newV7(t)
	replay.Gaps[0].DetailsJSON = "{\"source\" : \"startup_recovery\"}"
	recovered, err := repository.RecoverAndClose(ctx, replay)
	if err != nil {
		t.Fatalf("idempotent RecoverAndClose() error = %v", err)
	}
	if recovered.OperationID != input.RecoveryOperationID {
		t.Fatalf("idempotent recovery operation = %q", recovered.OperationID)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("idempotent recovery did not repair manifest: %v", err)
	}
	var gapCount int
	if err := store.Reader().QueryRow(
		"SELECT COUNT(*) FROM capture_gaps WHERE session_id = ?",
		created.ID,
	).Scan(&gapCount); err != nil {
		t.Fatalf("count recovery gaps: %v", err)
	}
	if gapCount != 1 {
		t.Fatalf("recovery gap count = %d, want 1", gapCount)
	}

	conflict := replay
	conflict.Gaps = append([]RecoveryGapInput(nil), replay.Gaps...)
	conflict.Gaps[0].ReasonCode = "DIFFERENT_RECOVERY_REASON"
	if _, err := repository.RecoverAndClose(
		ctx, conflict,
	); !errors.Is(err, ErrRecoveryGapConflict) {
		t.Fatalf("conflicting idempotent recovery error = %v", err)
	}
}

func TestSQLiteRepositoryRecoverAndCloseIdempotenceRejectsUnexpectedOpenGap(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID, OperationID: newV7(t), Recording: RecordingActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := recoveryCloseInput(t, created)
	if _, err := repository.RecoverAndClose(ctx, input); err != nil {
		t.Fatal(err)
	}
	insertExistingRecoveryGap(t, store.Writer(), created, "event_persistence",
		input.EndedAtMS, nil, false, "EVENT_PERSISTENCE_DEGRADED", "{}",
		"post-commit-open-gap:"+created.ID)
	if _, err := repository.RecoverAndClose(ctx, input); !errors.Is(err, ErrRecoveryGapConflict) {
		t.Fatalf("idempotent recovery error = %v, want open-gap conflict", err)
	}
}

func TestSQLiteRepositoryRecoverAndCloseGapConflictRollsBackSession(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingStarting,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	input := recoveryCloseInput(t, created)
	gap := input.Gaps[0]
	startOffset := gap.StartedAtMS - created.StartedAt
	endOffset := *gap.EndedAtMS - created.StartedAt
	if _, err := store.Writer().Exec(
		"INSERT INTO capture_gaps("+
			"id, session_id, kind, started_at, ended_at, start_offset_ms, "+
			"end_offset_ms, severity, recovered, reason_code, details_json, dedupe_key"+
			") VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)",
		newV7(t), created.ID, gap.Kind, gap.StartedAtMS, *gap.EndedAtMS,
		startOffset, endOffset, gap.Severity, "DIFFERENT_RECOVERY_REASON",
		"{\"source\":\"startup_recovery\"}", gap.DedupeKey,
	); err != nil {
		t.Fatalf("insert conflicting recovery gap: %v", err)
	}
	if _, err := repository.RecoverAndClose(
		ctx, input,
	); !errors.Is(err, ErrRecoveryGapConflict) {
		t.Fatalf("RecoverAndClose() error = %v, want gap conflict", err)
	}
	current, err := querySession(
		ctx, store.Reader(), sessionSelectSQL+" WHERE id = ?", created.ID,
	)
	if err != nil {
		t.Fatalf("read rolled-back session: %v", err)
	}
	if current.Status != created.Status ||
		current.RecordingStatus != created.RecordingStatus ||
		current.OperationID != created.OperationID || current.EndedAt != nil {
		t.Fatalf("gap conflict committed session mutation: %+v", current)
	}
}

func TestSQLiteRepositoryRecoverAndCloseRejectsStaleOperation(t *testing.T) {
	ctx := context.Background()
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingStarting,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := store.Writer().Exec(
		"UPDATE live_sessions SET operation_id = ?, updated_at = updated_at + 1 WHERE id = ?",
		newV7(t), created.ID,
	); err != nil {
		t.Fatalf("mutate operation before recovery: %v", err)
	}
	if _, err := repository.RecoverAndClose(
		ctx, recoveryCloseInput(t, created),
	); !errors.Is(err, ErrStaleRecovery) {
		t.Fatalf("RecoverAndClose() error = %v, want ErrStaleRecovery", err)
	}
}

func TestSQLiteRepositoryRecoverAndCloseResolvesCommitOutcome(t *testing.T) {
	ctx := context.Background()
	t.Run("committed_then_error", func(t *testing.T) {
		repository, store, _, roomID, _ := openRepository(t)
		defer store.Close()
		created, err := repository.Create(ctx, CreateSessionInput{
			RoomConfigID: roomID,
			OperationID:  newV7(t),
			Recording:    RecordingActive,
		})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		input := recoveryCloseInput(t, created)
		insertExistingRecoveryGap(
			t, store.Writer(), created, "recording_restart",
			created.StartedAt+250, nil, false, "RECORDER_PROCESS_EXITED", "{}",
			"ambiguous-commit-open-gap:"+created.ID,
		)
		injected := errors.New("injected ambiguous commit result")
		closed, err := repository.recoverAndClose(
			ctx, input, func(transaction *sql.Tx) error {
				if err := transaction.Commit(); err != nil {
					return err
				}
				return injected
			},
		)
		if err != nil {
			t.Fatalf("ambiguous committed recovery error = %v", err)
		}
		if closed.Status != SessionInterrupted ||
			closed.OperationID != input.RecoveryOperationID {
			t.Fatalf("ambiguous committed recovery = %+v", closed)
		}
		assertRecoveryOpenGapCount(t, store.Reader(), created.ID, 0)
	})

	t.Run("rolled_back_then_error", func(t *testing.T) {
		repository, store, _, roomID, _ := openRepository(t)
		defer store.Close()
		created, err := repository.Create(ctx, CreateSessionInput{
			RoomConfigID: roomID,
			OperationID:  newV7(t),
			Recording:    RecordingActive,
		})
		if err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		input := recoveryCloseInput(t, created)
		insertExistingRecoveryGap(
			t, store.Writer(), created, "event_persistence",
			created.StartedAt+250, nil, false,
			"EVENT_PERSISTENCE_DEGRADED", "{}",
			"rolled-back-open-gap:"+created.ID,
		)
		_, err = repository.recoverAndClose(
			ctx, input, func(transaction *sql.Tx) error {
				_ = transaction.Rollback()
				return errors.New("injected failed commit")
			},
		)
		if !errors.Is(err, ErrRecoveryPersistence) {
			t.Fatalf("rolled-back recovery error = %v, want persistence error", err)
		}
		current, loadErr := querySession(
			ctx, store.Reader(), sessionSelectSQL+" WHERE id = ?", created.ID,
		)
		if loadErr != nil {
			t.Fatalf("read rolled-back recovery session: %v", loadErr)
		}
		if current.Status != created.Status ||
			current.OperationID != created.OperationID {
			t.Fatalf("failed commit changed session: %+v", current)
		}
		assertRecoveryOpenGapCount(t, store.Reader(), created.ID, 1)
	})
}

func TestSQLiteRepositoryRecoverAndCloseValidatesGapSet(t *testing.T) {
	repository, store, _, roomID, _ := openRepository(t)
	defer store.Close()
	created, err := repository.Create(context.Background(), CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  newV7(t),
		Recording:    RecordingStarting,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	input := recoveryCloseInput(t, created)
	openGap := input
	openGap.Gaps = append([]RecoveryGapInput(nil), input.Gaps...)
	openGap.Gaps[0].EndedAtMS = nil
	if _, err := repository.RecoverAndClose(
		context.Background(), openGap,
	); !errors.Is(err, ErrRecoveryContractInvalid) {
		t.Fatalf("open terminal recovery gap error = %v", err)
	}
	input.Gaps = append(input.Gaps, input.Gaps[0])
	input.Gaps[1].ID = newV7(t)
	if _, err := repository.RecoverAndClose(
		context.Background(), input,
	); !errors.Is(err, ErrRecoveryContractInvalid) {
		t.Fatalf("duplicate recovery gap error = %v", err)
	}
}

type recoveryTestExecer interface {
	Exec(string, ...any) (sql.Result, error)
}

func insertRecoverableSessions(
	t *testing.T,
	database *sql.DB,
	count int,
	createdAt int64,
	badFirstManifest bool,
) []string {
	t.Helper()
	transaction, err := database.Begin()
	if err != nil {
		t.Fatalf("begin recoverable fixture: %v", err)
	}
	defer transaction.Rollback()
	ids := make([]string, 0, count)
	for index := 0; index < count; index++ {
		badManifest := badFirstManifest && index == 0
		session := insertRecoverySessionRow(
			t, transaction, createdAt,
			[]SessionStatus{
				SessionStarting,
				SessionRecording,
				SessionFinalizing,
			}[index%3],
			[]RecordingStatus{
				RecordingStarting,
				RecordingActive,
				RecordingFinalizing,
			}[index%3],
			badManifest,
		)
		ids = append(ids, session.ID)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatalf("commit recoverable fixture: %v", err)
	}
	return ids
}

func insertRecoverySessionRow(
	t *testing.T,
	execer recoveryTestExecer,
	createdAt int64,
	status SessionStatus,
	recording RecordingStatus,
	badManifest bool,
) LiveSession {
	t.Helper()
	roomID := newV7(t)
	sessionID := newV7(t)
	operationID := newV7(t)
	if _, err := execer.Exec(
		"INSERT INTO rooms(id, live_id, alias, created_at, updated_at) "+
			"VALUES (?, ?, ?, 1, 1)",
		roomID, "recovery-live-"+roomID, "recovery-room",
	); err != nil {
		t.Fatalf("insert recovery room: %v", err)
	}
	dataPath := "rooms/no-manifest/sessions/2026/07/" + sessionID
	if badManifest {
		dataPath = "../invalid-recovery-manifest"
	}
	const startedAt = int64(100)
	if _, err := execer.Exec(
		"INSERT INTO live_sessions("+
			"id, room_config_id, operation_id, status, recording_status, "+
			"manifest_dirty, started_at, clock_source, integrity_score, "+
			"data_path, schema_version, created_at, updated_at"+
			") VALUES (?, ?, ?, ?, ?, 1, ?, 'received', 1, ?, ?, ?, ?)",
		sessionID, roomID, operationID, status, recording, startedAt,
		dataPath, SessionManifestSchemaVersion, createdAt, createdAt,
	); err != nil {
		t.Fatalf("insert recovery session: %v", err)
	}
	return LiveSession{
		ID: sessionID, RoomConfigID: roomID, OperationID: operationID,
		Status: status, RecordingStatus: recording, ManifestDirty: true,
		StartedAt: startedAt, ClockSource: ClockReceived, IntegrityScore: 1,
		DataPath: dataPath, SchemaVersion: SessionManifestSchemaVersion,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}

func recoveryCloseInput(t *testing.T, session LiveSession) RecoverAndCloseInput {
	t.Helper()
	endedAt := session.StartedAt + int64(time.Second/time.Millisecond)
	scanCutoff := max(session.CreatedAt, endedAt) + int64(time.Second/time.Millisecond)
	gapStartedAt := session.StartedAt + 100
	gapEndedAt := session.StartedAt + 500
	return RecoverAndCloseInput{
		SessionID:               session.ID,
		ExpectedStatus:          session.Status,
		ExpectedRecordingStatus: session.RecordingStatus,
		ExpectedOperationID:     session.OperationID,
		RecoveryOperationID:     newV7(t),
		ScanCutoffMS:            scanCutoff,
		EndedAtMS:               endedAt,
		IntegrityScore:          0.5,
		Gaps: []RecoveryGapInput{{
			ID:          newV7(t),
			Kind:        "process_crash",
			StartedAtMS: gapStartedAt,
			EndedAtMS:   &gapEndedAt,
			Severity:    "error",
			Recovered:   false,
			ReasonCode:  "STARTUP_PROCESS_CRASH",
			DetailsJSON: "{ \"source\" : \"startup_recovery\" }",
			DedupeKey:   "startup-recovery:" + session.ID + ":process-crash",
		}},
	}
}

func insertExistingRecoveryGap(
	t *testing.T,
	database *sql.DB,
	session LiveSession,
	kind string,
	startedAt int64,
	endedAt *int64,
	recovered bool,
	reason string,
	details string,
	dedupeKey string,
) string {
	t.Helper()
	gapID := newV7(t)
	var endedValue, endOffsetValue any
	if endedAt != nil {
		endedValue = *endedAt
		endOffsetValue = *endedAt - session.StartedAt
	}
	recoveredValue := 0
	if recovered {
		recoveredValue = 1
	}
	if _, err := database.Exec(
		"INSERT INTO capture_gaps("+
			"id, session_id, kind, started_at, ended_at, start_offset_ms, "+
			"end_offset_ms, severity, recovered, reason_code, details_json, dedupe_key"+
			") VALUES (?, ?, ?, ?, ?, ?, ?, 'warning', ?, ?, ?, ?)",
		gapID,
		session.ID,
		kind,
		startedAt,
		endedValue,
		startedAt-session.StartedAt,
		endOffsetValue,
		recoveredValue,
		reason,
		details,
		dedupeKey,
	); err != nil {
		t.Fatalf("insert existing recovery gap: %v", err)
	}
	return gapID
}

func assertRecoveryOpenGapCount(
	t *testing.T,
	database *sql.DB,
	sessionID string,
	want int,
) {
	t.Helper()
	var count int
	if err := database.QueryRow(
		"SELECT COUNT(*) FROM capture_gaps WHERE session_id = ? AND ended_at IS NULL",
		sessionID,
	).Scan(&count); err != nil {
		t.Fatalf("count open recovery gaps: %v", err)
	}
	if count != want {
		t.Fatalf("open recovery gap count = %d, want %d", count, want)
	}
}

func readRecoveryManifest(t *testing.T, path string) LiveSession {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recovery manifest: %v", err)
	}
	var session LiveSession
	if err := json.Unmarshal(payload, &session); err != nil {
		t.Fatalf("decode recovery manifest: %v", err)
	}
	return session
}
