package capture

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recorderRecoveryFixture struct {
	repository *SQLiteRepository
	writer     *sql.DB
	session    LiveSession
	attempt    MediaAttempt
	now        time.Time
}

func TestSQLiteRecorderRecoveryBeginCompleteIsAtomicAndIdempotent(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	ctx := context.Background()
	beginInput := fixture.beginInput()

	entry, err := fixture.repository.BeginRecorderRecovery(ctx, beginInput)
	if err != nil {
		t.Fatalf("BeginRecorderRecovery() error = %v", err)
	}
	if entry.Session.RecordingStatus != RecordingReconnecting ||
		entry.Session.Status != fixture.session.Status ||
		entry.Session.OperationID != fixture.session.OperationID ||
		validateUUIDv7("gap id", entry.GapID) != nil {
		t.Fatalf("BeginRecorderRecovery() = %+v", entry)
	}
	firstUpdatedAt := entry.Session.UpdatedAt
	replayed, err := fixture.repository.BeginRecorderRecovery(ctx, beginInput)
	if err != nil {
		t.Fatalf("BeginRecorderRecovery(replay) error = %v", err)
	}
	if replayed.GapID != entry.GapID || replayed.Session.UpdatedAt != firstUpdatedAt {
		t.Fatalf("BeginRecorderRecovery(replay) = %+v, want same gap/version", replayed)
	}
	if count := fixture.gapCount(t); count != 1 {
		t.Fatalf("gap count after replay = %d, want 1", count)
	}
	openGap := fixture.gap(t, entry.GapID)
	if openGap.EndedAtMS != nil || openGap.Recovered ||
		openGap.DedupeKey != recorderRecoveryDedupeKey(fixture.attempt.ID) ||
		openGap.Details.SourceOperationID != fixture.session.OperationID ||
		strings.Contains(openGap.DetailsJSON, "://") ||
		strings.Contains(openGap.DetailsJSON, fixture.session.DataPath) ||
		strings.Contains(openGap.DetailsJSON, "clockUncertain") {
		t.Fatalf("open recovery gap = %+v", openGap)
	}

	completeInput := CompleteRecorderRecoveryInput{
		SessionID:               fixture.session.ID,
		GapID:                   entry.GapID,
		ExpectedStatus:          fixture.session.Status,
		ExpectedRecordingStatus: RecordingReconnecting,
		ExpectedOperationID:     fixture.session.OperationID,
		RestartAttempts:         2,
		CompletedAt:             fixture.now.Add(5 * time.Second),
	}
	completed, err := fixture.repository.CompleteRecorderRecovery(ctx, completeInput)
	if err != nil {
		t.Fatalf("CompleteRecorderRecovery() error = %v", err)
	}
	if completed.RecordingStatus != RecordingActive ||
		completed.Status != fixture.session.Status ||
		completed.OperationID != fixture.session.OperationID {
		t.Fatalf("CompleteRecorderRecovery() = %+v", completed)
	}
	completedVersion := completed.UpdatedAt
	completedReplay, err := fixture.repository.CompleteRecorderRecovery(ctx, completeInput)
	if err != nil {
		t.Fatalf("CompleteRecorderRecovery(replay) error = %v", err)
	}
	if completedReplay.UpdatedAt != completedVersion {
		t.Fatalf("CompleteRecorderRecovery(replay) updated_at = %d, want %d", completedReplay.UpdatedAt, completedVersion)
	}
	closedGap := fixture.gap(t, entry.GapID)
	if closedGap.EndedAtMS == nil ||
		*closedGap.EndedAtMS != completeInput.CompletedAt.UnixMilli() ||
		!closedGap.Recovered ||
		closedGap.Details.RestartAttempts != completeInput.RestartAttempts {
		t.Fatalf("completed recovery gap = %+v", closedGap)
	}
	manifestPath, err := secureManifestPath(fixture.repository.dataRoot, completed.DataPath)
	if err != nil {
		t.Fatalf("secureManifestPath() error = %v", err)
	}
	assertManifest(t, manifestPath, completed.ID, completed.OperationID, RecordingActive)
}

func TestSQLiteRecorderRecoveryPersistsClockUncertaintyIdempotently(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	input := fixture.beginInput()
	input.OccurredAt = time.UnixMilli(fixture.session.StartedAt).UTC()
	input.ClockUncertain = true
	input.ErrorCode = RecorderNetworkFailureErrorCode

	entry, err := fixture.repository.BeginRecorderRecovery(context.Background(), input)
	if err != nil {
		t.Fatalf("BeginRecorderRecovery(clock uncertain) error = %v", err)
	}
	gap := fixture.gap(t, entry.GapID)
	if gap.StartedAtMS != fixture.session.StartedAt || gap.StartOffsetMS != 0 ||
		!gap.Details.ClockUncertain ||
		gap.Details.SourceErrorCode != RecorderNetworkFailureErrorCode ||
		gap.ReasonCode != RecorderNetworkFailureErrorCode ||
		!strings.Contains(gap.DetailsJSON, `"clockUncertain":true`) {
		t.Fatalf("clock-uncertain recovery gap = %+v", gap)
	}
	replayed, err := fixture.repository.BeginRecorderRecovery(context.Background(), input)
	if err != nil {
		t.Fatalf("BeginRecorderRecovery(clock uncertain replay) error = %v", err)
	}
	if replayed.GapID != entry.GapID || fixture.gapCount(t) != 1 {
		t.Fatalf("clock-uncertain replay = %+v count=%d", replayed, fixture.gapCount(t))
	}

	conflict := input
	conflict.ClockUncertain = false
	if _, err := fixture.repository.BeginRecorderRecovery(
		context.Background(), conflict,
	); !errors.Is(err, ErrRecoveryGapConflict) {
		t.Fatalf("clock certainty semantic conflict error = %v", err)
	}
	afterConflict := fixture.gap(t, entry.GapID)
	if !afterConflict.Details.ClockUncertain || afterConflict.DetailsJSON != gap.DetailsJSON ||
		fixture.gapCount(t) != 1 {
		t.Fatalf("semantic conflict changed durable gap: %+v", afterConflict)
	}
}

func TestRecorderRecoveryGapDetailsRemainBackwardCompatibleWithoutClockField(t *testing.T) {
	details := recorderRecoveryGapDetails{
		Version: recorderRecoveryDetailsSchema, SourceAttemptID: newV7(t),
		SourceOperationID: newV7(t), SourceErrorCode: RecorderProcessExitedErrorCode,
		LastErrorCode: RecorderProcessExitedErrorCode, LastOccurredAtMS: 1,
	}
	encoded, err := encodeRecorderRecoveryGapDetails(details)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "clockUncertain") {
		t.Fatalf("legacy details gained non-canonical false field: %s", encoded)
	}
	decoded, err := decodeRecorderRecoveryGapDetails(encoded)
	if err != nil || decoded.ClockUncertain {
		t.Fatalf("decode legacy details = (%+v, %v)", decoded, err)
	}
}

func TestSQLiteRecorderRecoveryExhaustAndCloseAfterOperationRotation(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	ctx := context.Background()
	entry, err := fixture.repository.BeginRecorderRecovery(ctx, fixture.beginInput())
	if err != nil {
		t.Fatalf("BeginRecorderRecovery() error = %v", err)
	}
	exhaustInput := ExhaustRecorderRecoveryInput{
		SessionID:               fixture.session.ID,
		GapID:                   entry.GapID,
		ExpectedStatus:          fixture.session.Status,
		ExpectedRecordingStatus: RecordingReconnecting,
		ExpectedOperationID:     fixture.session.OperationID,
		RestartAttempts:         3,
		ErrorCode:               RecorderRecoveryRetryExhaustedErrorCode,
		ExhaustedAt:             fixture.now.Add(10 * time.Second),
	}
	exhausted, err := fixture.repository.ExhaustRecorderRecovery(ctx, exhaustInput)
	if err != nil {
		t.Fatalf("ExhaustRecorderRecovery() error = %v", err)
	}
	if exhausted.RecordingStatus != RecordingUnavailable {
		t.Fatalf("ExhaustRecorderRecovery() recording status = %q", exhausted.RecordingStatus)
	}
	exhaustedVersion := exhausted.UpdatedAt
	replayed, err := fixture.repository.ExhaustRecorderRecovery(ctx, exhaustInput)
	if err != nil {
		t.Fatalf("ExhaustRecorderRecovery(replay) error = %v", err)
	}
	if replayed.UpdatedAt != exhaustedVersion {
		t.Fatalf("ExhaustRecorderRecovery(replay) updated_at = %d, want %d", replayed.UpdatedAt, exhaustedVersion)
	}
	openGap := fixture.gap(t, entry.GapID)
	if openGap.EndedAtMS != nil || openGap.Recovered ||
		openGap.ReasonCode != exhaustInput.ErrorCode ||
		openGap.Details.RestartAttempts != exhaustInput.RestartAttempts {
		t.Fatalf("exhausted recovery gap = %+v", openGap)
	}

	finalizeOperationID := newV7(t)
	finalizing, err := fixture.repository.Transition(ctx, TransitionSessionInput{
		ID:                      exhausted.ID,
		ExpectedStatus:          exhausted.Status,
		ExpectedRecordingStatus: exhausted.RecordingStatus,
		ExpectedOperationID:     exhausted.OperationID,
		Status:                  SessionFinalizing,
		RecordingStatus:         RecordingFinalizing,
		NextOperationID:         finalizeOperationID,
	})
	if err != nil {
		t.Fatalf("Transition(finalizing) error = %v", err)
	}
	closeInput := CloseRecorderRecoveryInput{
		SessionID:               finalizing.ID,
		GapID:                   entry.GapID,
		ExpectedStatus:          finalizing.Status,
		ExpectedRecordingStatus: finalizing.RecordingStatus,
		ExpectedOperationID:     finalizing.OperationID,
		Recovered:               false,
		ErrorCode:               RecorderRecoveryRetryExhaustedErrorCode,
		ClosedAt:                fixture.now.Add(15 * time.Second),
	}
	if err := fixture.repository.CloseRecorderRecovery(ctx, closeInput); err != nil {
		t.Fatalf("CloseRecorderRecovery() error = %v", err)
	}
	if err := fixture.repository.CloseRecorderRecovery(ctx, closeInput); err != nil {
		t.Fatalf("CloseRecorderRecovery(replay) error = %v", err)
	}
	afterClose, err := fixture.repository.Get(ctx, finalizing.ID)
	if err != nil {
		t.Fatalf("Get(after close) error = %v", err)
	}
	if afterClose.Status != finalizing.Status ||
		afterClose.RecordingStatus != finalizing.RecordingStatus ||
		afterClose.OperationID != finalizing.OperationID ||
		afterClose.UpdatedAt != finalizing.UpdatedAt {
		t.Fatalf("CloseRecorderRecovery changed session: before=%+v after=%+v", finalizing, afterClose)
	}
	closedGap := fixture.gap(t, entry.GapID)
	if closedGap.EndedAtMS == nil ||
		*closedGap.EndedAtMS != closeInput.ClosedAt.UnixMilli() ||
		closedGap.Recovered ||
		closedGap.Details.SourceOperationID != fixture.session.OperationID {
		t.Fatalf("closed recovery gap = %+v", closedGap)
	}
	conflict := closeInput
	conflict.Recovered = true
	if err := fixture.repository.CloseRecorderRecovery(ctx, conflict); !errors.Is(err, ErrRecoveryGapConflict) {
		t.Fatalf("CloseRecorderRecovery(conflict) error = %v, want ErrRecoveryGapConflict", err)
	}
}

func TestSQLiteRecorderRecoveryBeginRollsBackGapWhenSessionCASFails(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	if _, err := fixture.writer.Exec(`CREATE TRIGGER fail_recovery_begin
		BEFORE UPDATE OF recording_status ON live_sessions
		WHEN NEW.recording_status = 'reconnecting'
		BEGIN SELECT RAISE(ABORT, 'forced recovery rollback'); END`); err != nil {
		t.Fatalf("create rollback trigger: %v", err)
	}
	if _, err := fixture.repository.BeginRecorderRecovery(context.Background(), fixture.beginInput()); !errors.Is(err, ErrRecoveryPersistence) {
		t.Fatalf("BeginRecorderRecovery() error = %v, want ErrRecoveryPersistence", err)
	}
	fixture.assertRecordingStatus(t, RecordingActive)
	if count := fixture.gapCount(t); count != 0 {
		t.Fatalf("gap count after rolled-back begin = %d, want 0", count)
	}
	manifestPath, err := secureManifestPath(fixture.repository.dataRoot, fixture.session.DataPath)
	if err != nil {
		t.Fatalf("secureManifestPath() error = %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(manifestPath), ".session-*.tmp"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("staged manifest leftovers = %v, err = %v", matches, err)
	}
}

func TestSQLiteRecorderRecoveryTerminalMutationRollsBackWithSession(t *testing.T) {
	tests := []struct {
		name   string
		target RecordingStatus
		run    func(*recorderRecoveryFixture, RecorderRecoveryJournalEntry) error
	}{
		{
			name:   "complete",
			target: RecordingActive,
			run: func(fixture *recorderRecoveryFixture, entry RecorderRecoveryJournalEntry) error {
				_, err := fixture.repository.CompleteRecorderRecovery(context.Background(), CompleteRecorderRecoveryInput{
					SessionID: fixture.session.ID, GapID: entry.GapID,
					ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
					ExpectedOperationID: fixture.session.OperationID, RestartAttempts: 1,
					CompletedAt: fixture.now.Add(5 * time.Second),
				})
				return err
			},
		},
		{
			name:   "exhaust",
			target: RecordingUnavailable,
			run: func(fixture *recorderRecoveryFixture, entry RecorderRecoveryJournalEntry) error {
				_, err := fixture.repository.ExhaustRecorderRecovery(context.Background(), ExhaustRecorderRecoveryInput{
					SessionID: fixture.session.ID, GapID: entry.GapID,
					ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
					ExpectedOperationID: fixture.session.OperationID, RestartAttempts: 2,
					ErrorCode:   RecorderRecoveryRetryExhaustedErrorCode,
					ExhaustedAt: fixture.now.Add(5 * time.Second),
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := openRecorderRecoveryFixture(t)
			entry, err := fixture.repository.BeginRecorderRecovery(context.Background(), fixture.beginInput())
			if err != nil {
				t.Fatalf("BeginRecorderRecovery() error = %v", err)
			}
			trigger := "CREATE TRIGGER fail_recovery_terminal BEFORE UPDATE OF recording_status ON live_sessions " +
				"WHEN NEW.recording_status = '" + string(test.target) +
				"' BEGIN SELECT RAISE(ABORT, 'forced recovery rollback'); END"
			if _, err := fixture.writer.Exec(trigger); err != nil {
				t.Fatalf("create rollback trigger: %v", err)
			}
			if err := test.run(fixture, entry); !errors.Is(err, ErrRecoveryPersistence) {
				t.Fatalf("terminal recovery error = %v, want ErrRecoveryPersistence", err)
			}
			fixture.assertRecordingStatus(t, RecordingReconnecting)
			gap := fixture.gap(t, entry.GapID)
			if gap.EndedAtMS != nil || gap.Recovered ||
				gap.Details.RestartAttempts != 0 ||
				gap.Details.LastOccurredAtMS != fixture.beginInput().OccurredAt.UnixMilli() {
				t.Fatalf("gap after rolled-back terminal mutation = %+v", gap)
			}
		})
	}
}

func TestSQLiteRecorderRecoveryRejectsStaleOperation(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	staleBegin := fixture.beginInput()
	staleBegin.ExpectedOperationID = newV7(t)
	if _, err := fixture.repository.BeginRecorderRecovery(context.Background(), staleBegin); !errors.Is(err, ErrStaleRecovery) {
		t.Fatalf("BeginRecorderRecovery(stale) error = %v, want ErrStaleRecovery", err)
	}
	if fixture.gapCount(t) != 0 {
		t.Fatal("stale begin persisted a gap")
	}

	entry, err := fixture.repository.BeginRecorderRecovery(context.Background(), fixture.beginInput())
	if err != nil {
		t.Fatalf("BeginRecorderRecovery() error = %v", err)
	}
	newOperationID := newV7(t)
	if _, err := fixture.repository.Transition(context.Background(), TransitionSessionInput{
		ID: fixture.session.ID, ExpectedStatus: fixture.session.Status,
		ExpectedRecordingStatus: RecordingReconnecting, ExpectedOperationID: fixture.session.OperationID,
		Status: fixture.session.Status, RecordingStatus: RecordingReconnecting,
		NextOperationID: newOperationID,
	}); err != nil {
		t.Fatalf("Transition(operation rotation) error = %v", err)
	}
	if _, err := fixture.repository.CompleteRecorderRecovery(context.Background(), CompleteRecorderRecoveryInput{
		SessionID: fixture.session.ID, GapID: entry.GapID,
		ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
		ExpectedOperationID: fixture.session.OperationID, RestartAttempts: 1,
		CompletedAt: fixture.now.Add(5 * time.Second),
	}); !errors.Is(err, ErrStaleRecovery) {
		t.Fatalf("CompleteRecorderRecovery(old operation) error = %v, want ErrStaleRecovery", err)
	}
	gap := fixture.gap(t, entry.GapID)
	if gap.EndedAtMS != nil || gap.Recovered {
		t.Fatalf("old operation changed gap = %+v", gap)
	}
	completed, err := fixture.repository.CompleteRecorderRecovery(context.Background(), CompleteRecorderRecoveryInput{
		SessionID: fixture.session.ID, GapID: entry.GapID,
		ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
		ExpectedOperationID: newOperationID, RestartAttempts: 0,
		CompletedAt: fixture.now.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("CompleteRecorderRecovery(new operation) error = %v", err)
	}
	if completed.OperationID != newOperationID || completed.RecordingStatus != RecordingActive {
		t.Fatalf("CompleteRecorderRecovery(new operation) = %+v", completed)
	}
	gap = fixture.gap(t, entry.GapID)
	if gap.Details.SourceOperationID != fixture.session.OperationID || !gap.Recovered {
		t.Fatalf("rotated operation lost source audit = %+v", gap)
	}
}

func TestSQLiteRecorderRecoveryExhaustsAndClosesAcrossOperationRotation(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	ctx := context.Background()
	entry, err := fixture.repository.BeginRecorderRecovery(ctx, fixture.beginInput())
	if err != nil {
		t.Fatalf("BeginRecorderRecovery() error = %v", err)
	}
	rebindOperationID := newV7(t)
	reconnecting, err := fixture.repository.Transition(ctx, TransitionSessionInput{
		ID: fixture.session.ID, ExpectedStatus: fixture.session.Status,
		ExpectedRecordingStatus: RecordingReconnecting, ExpectedOperationID: fixture.session.OperationID,
		Status: fixture.session.Status, RecordingStatus: RecordingReconnecting,
		NextOperationID: rebindOperationID,
	})
	if err != nil {
		t.Fatalf("Transition(external rebind) error = %v", err)
	}
	exhaustInput := ExhaustRecorderRecoveryInput{
		SessionID: reconnecting.ID, GapID: entry.GapID,
		ExpectedStatus: reconnecting.Status, ExpectedRecordingStatus: reconnecting.RecordingStatus,
		ExpectedOperationID: reconnecting.OperationID, RestartAttempts: 0,
		ErrorCode:   RecorderNetworkFailureErrorCode,
		ExhaustedAt: fixture.now.Add(5 * time.Second),
	}
	exhausted, err := fixture.repository.ExhaustRecorderRecovery(ctx, exhaustInput)
	if err != nil {
		t.Fatalf("ExhaustRecorderRecovery(rotated operation) error = %v", err)
	}
	if exhausted.OperationID != rebindOperationID || exhausted.RecordingStatus != RecordingUnavailable {
		t.Fatalf("ExhaustRecorderRecovery(rotated operation) = %+v", exhausted)
	}
	if _, err := fixture.repository.resolveExhaustRecorderRecoveryOutcome(
		exhaustInput,
		exhaustInput.ExhaustedAt.UnixMilli(),
	); err != nil {
		t.Fatalf("resolveExhaustRecorderRecoveryOutcome() error = %v", err)
	}
	gap := fixture.gap(t, entry.GapID)
	if gap.Details.SourceOperationID != fixture.session.OperationID ||
		gap.EndedAtMS != nil ||
		gap.Details.RestartAttempts != 0 {
		t.Fatalf("rotated exhaust gap = %+v", gap)
	}

	finalizeOperationID := newV7(t)
	finalizing, err := fixture.repository.Transition(ctx, TransitionSessionInput{
		ID: exhausted.ID, ExpectedStatus: exhausted.Status,
		ExpectedRecordingStatus: exhausted.RecordingStatus, ExpectedOperationID: exhausted.OperationID,
		Status: SessionFinalizing, RecordingStatus: RecordingFinalizing,
		NextOperationID: finalizeOperationID,
	})
	if err != nil {
		t.Fatalf("Transition(finalizing) error = %v", err)
	}
	closeInput := CloseRecorderRecoveryInput{
		SessionID: finalizing.ID, GapID: entry.GapID,
		ExpectedStatus: finalizing.Status, ExpectedRecordingStatus: finalizing.RecordingStatus,
		ExpectedOperationID: finalizing.OperationID, Recovered: false,
		ErrorCode: RecorderNetworkFailureErrorCode,
		ClosedAt:  fixture.now.Add(10 * time.Second),
	}
	if err := fixture.repository.CloseRecorderRecovery(ctx, closeInput); err != nil {
		t.Fatalf("CloseRecorderRecovery(rotated operation) error = %v", err)
	}
	if err := fixture.repository.resolveCloseRecorderRecoveryOutcome(
		closeInput,
		closeInput.ClosedAt.UnixMilli(),
	); err != nil {
		t.Fatalf("resolveCloseRecorderRecoveryOutcome() error = %v", err)
	}
}

func TestSQLiteRecorderRecoveryRequiresGapAuditForProgress(t *testing.T) {
	tests := []struct {
		name string
		run  func(*recorderRecoveryFixture) error
	}{
		{
			name: "complete",
			run: func(fixture *recorderRecoveryFixture) error {
				_, err := fixture.repository.CompleteRecorderRecovery(context.Background(), CompleteRecorderRecoveryInput{
					SessionID: fixture.session.ID, GapID: newV7(t),
					ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
					ExpectedOperationID: fixture.session.OperationID, RestartAttempts: 1,
					CompletedAt: fixture.now.Add(5 * time.Second),
				})
				return err
			},
		},
		{
			name: "exhaust",
			run: func(fixture *recorderRecoveryFixture) error {
				_, err := fixture.repository.ExhaustRecorderRecovery(context.Background(), ExhaustRecorderRecoveryInput{
					SessionID: fixture.session.ID, GapID: newV7(t),
					ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
					ExpectedOperationID: fixture.session.OperationID, RestartAttempts: 1,
					ErrorCode:   RecorderRecoveryRetryExhaustedErrorCode,
					ExhaustedAt: fixture.now.Add(5 * time.Second),
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := openRecorderRecoveryFixture(t)
			if _, err := fixture.repository.Transition(context.Background(), TransitionSessionInput{
				ID: fixture.session.ID, ExpectedStatus: fixture.session.Status,
				ExpectedRecordingStatus: RecordingActive, ExpectedOperationID: fixture.session.OperationID,
				Status: fixture.session.Status, RecordingStatus: RecordingReconnecting,
			}); err != nil {
				t.Fatalf("Transition(reconnecting without audit) error = %v", err)
			}
			if err := test.run(fixture); !errors.Is(err, ErrRecoveryGapConflict) {
				t.Fatalf("terminal mutation without gap error = %v, want ErrRecoveryGapConflict", err)
			}
			fixture.assertRecordingStatus(t, RecordingReconnecting)
		})
	}
}

func TestSQLiteRecorderRecoveryConflictsOnDedupeSemanticChange(t *testing.T) {
	tests := []struct {
		name   string
		change func(*BeginRecorderRecoveryInput)
	}{
		{
			name: "error code",
			change: func(input *BeginRecorderRecoveryInput) {
				input.ErrorCode = RecorderNetworkFailureErrorCode
			},
		},
		{
			name: "occurred at",
			change: func(input *BeginRecorderRecoveryInput) {
				input.OccurredAt = input.OccurredAt.Add(time.Millisecond)
			},
		},
		{
			name: "clock certainty",
			change: func(input *BeginRecorderRecoveryInput) {
				input.ClockUncertain = true
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := openRecorderRecoveryFixture(t)
			input := fixture.beginInput()
			entry, err := fixture.repository.BeginRecorderRecovery(context.Background(), input)
			if err != nil {
				t.Fatalf("BeginRecorderRecovery() error = %v", err)
			}
			test.change(&input)
			if _, err := fixture.repository.BeginRecorderRecovery(context.Background(), input); !errors.Is(err, ErrRecoveryGapConflict) {
				t.Fatalf("BeginRecorderRecovery(conflict) error = %v, want ErrRecoveryGapConflict", err)
			}
			if fixture.gapCount(t) != 1 || fixture.gap(t, entry.GapID).EndedAtMS != nil {
				t.Fatal("conflicting replay changed durable gap")
			}
		})
	}
}

func TestSQLiteRecorderRecoveryRequiresDurableSourceAttempt(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*recorderRecoveryFixture)
	}{
		{
			name: "missing",
			mutate: func(fixture *recorderRecoveryFixture) {
				if _, err := fixture.writer.Exec(`UPDATE session_media SET attempts_json = '[]' WHERE session_id = ?`, fixture.session.ID); err != nil {
					t.Fatalf("remove source attempt: %v", err)
				}
			},
		},
		{
			name: "corrupt",
			mutate: func(fixture *recorderRecoveryFixture) {
				if _, err := fixture.writer.Exec(`UPDATE session_media SET attempts_json = '{}' WHERE session_id = ?`, fixture.session.ID); err != nil {
					t.Fatalf("corrupt source attempt audit: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := openRecorderRecoveryFixture(t)
			test.mutate(fixture)
			if _, err := fixture.repository.BeginRecorderRecovery(context.Background(), fixture.beginInput()); !errors.Is(err, ErrRecoveryGapConflict) {
				t.Fatalf("BeginRecorderRecovery(no source audit) error = %v, want ErrRecoveryGapConflict", err)
			}
			fixture.assertRecordingStatus(t, RecordingActive)
			if fixture.gapCount(t) != 0 {
				t.Fatal("missing source audit still persisted recovery gap")
			}
		})
	}
}

func TestSQLiteRecorderRecoveryOutcomeResolversVerifyBothSides(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	beginInput := fixture.beginInput()
	entry, err := fixture.repository.BeginRecorderRecovery(context.Background(), beginInput)
	if err != nil {
		t.Fatalf("BeginRecorderRecovery() error = %v", err)
	}
	resolvedBegin, err := fixture.repository.resolveBeginRecorderRecoveryOutcome(beginInput, beginInput.OccurredAt.UnixMilli())
	if err != nil || resolvedBegin.GapID != entry.GapID {
		t.Fatalf("resolveBeginRecorderRecoveryOutcome() = (%+v, %v)", resolvedBegin, err)
	}
	completeInput := CompleteRecorderRecoveryInput{
		SessionID: fixture.session.ID, GapID: entry.GapID,
		ExpectedStatus: fixture.session.Status, ExpectedRecordingStatus: RecordingReconnecting,
		ExpectedOperationID: fixture.session.OperationID, RestartAttempts: 2,
		CompletedAt: fixture.now.Add(5 * time.Second),
	}
	if _, err := fixture.repository.CompleteRecorderRecovery(context.Background(), completeInput); err != nil {
		t.Fatalf("CompleteRecorderRecovery() error = %v", err)
	}
	if _, err := fixture.repository.resolveCompleteRecorderRecoveryOutcome(
		completeInput,
		completeInput.CompletedAt.UnixMilli(),
	); err != nil {
		t.Fatalf("resolveCompleteRecorderRecoveryOutcome() error = %v", err)
	}
	wrong := completeInput
	wrong.RestartAttempts++
	if _, err := fixture.repository.resolveCompleteRecorderRecoveryOutcome(
		wrong,
		wrong.CompletedAt.UnixMilli(),
	); !errors.Is(err, ErrRecoveryGapConflict) {
		t.Fatalf("resolver accepted mismatched audit: %v", err)
	}
}

func TestSQLiteRecorderRecoveryRejectsInvalidContractWithoutMutation(t *testing.T) {
	fixture := openRecorderRecoveryFixture(t)
	tests := []BeginRecorderRecoveryInput{
		func() BeginRecorderRecoveryInput {
			input := fixture.beginInput()
			input.SourceAttemptID = "not-a-v7"
			return input
		}(),
		func() BeginRecorderRecoveryInput {
			input := fixture.beginInput()
			input.ErrorCode = "UNKNOWN_RECOVERY_ERROR"
			return input
		}(),
		func() BeginRecorderRecoveryInput {
			input := fixture.beginInput()
			input.OccurredAt = time.Time{}
			return input
		}(),
	}
	for index, input := range tests {
		if _, err := fixture.repository.BeginRecorderRecovery(context.Background(), input); !errors.Is(err, ErrRecoveryContractInvalid) {
			t.Fatalf("invalid input %d error = %v, want ErrRecoveryContractInvalid", index, err)
		}
	}
	fixture.assertRecordingStatus(t, RecordingActive)
	if fixture.gapCount(t) != 0 {
		t.Fatal("invalid contracts mutated recovery audit")
	}
}

func openRecorderRecoveryFixture(t *testing.T) *recorderRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	repository, store, _, roomID, now := openRepository(t)
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close storage: %v", err)
		}
	})
	startedAt := now.Add(-time.Minute)
	operationID := newV7(t)
	session, err := repository.Create(ctx, CreateSessionInput{
		RoomConfigID: roomID,
		OperationID:  operationID,
		Recording:    RecordingPending,
		StartedAt:    startedAt,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	opened, err := repository.OpenSessionMedia(ctx, OpenSessionMediaInput{
		SessionID: session.ID, RelativePath: "recovery/" + session.ID,
		StartedAt: startedAt.UnixMilli(),
	})
	if err != nil {
		t.Fatalf("OpenSessionMedia() error = %v", err)
	}
	attempt := MediaAttempt{
		ID: newV7(t), Ordinal: 1, StartedAt: startedAt.Add(time.Second).UnixMilli(),
		SegmentSeconds: 300, Committed: true, Clean: false,
		Protocol: "flv", Codec: "h264",
	}
	if _, err := repository.PersistMediaSnapshot(ctx, PersistMediaSnapshotInput{
		SessionID: session.ID, ExpectedRevision: opened.Session.ManifestRevision,
		State: SessionMediaOpen, Attempts: []MediaAttempt{attempt},
		UpdatedAt: now.Add(-time.Second).UnixMilli(),
	}); err != nil {
		t.Fatalf("PersistMediaSnapshot() error = %v", err)
	}
	session, err = repository.Transition(ctx, TransitionSessionInput{
		ID: session.ID, ExpectedStatus: session.Status,
		ExpectedRecordingStatus: session.RecordingStatus, ExpectedOperationID: session.OperationID,
		Status: SessionRecording, RecordingStatus: RecordingActive,
	})
	if err != nil {
		t.Fatalf("Transition(recording) error = %v", err)
	}
	return &recorderRecoveryFixture{
		repository: repository,
		writer:     store.Writer(),
		session:    session,
		attempt:    attempt,
		now:        now,
	}
}

func (fixture *recorderRecoveryFixture) beginInput() BeginRecorderRecoveryInput {
	return BeginRecorderRecoveryInput{
		SessionID:               fixture.session.ID,
		ExpectedStatus:          fixture.session.Status,
		ExpectedRecordingStatus: RecordingActive,
		ExpectedOperationID:     fixture.session.OperationID,
		SourceAttemptID:         fixture.attempt.ID,
		ErrorCode:               RecorderProcessExitedErrorCode,
		OccurredAt:              fixture.now,
	}
}

func (fixture *recorderRecoveryFixture) gap(t *testing.T, gapID string) recorderRecoveryGap {
	t.Helper()
	gap, found, err := queryRecorderRecoveryGap(
		context.Background(),
		fixture.writer,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		fixture.session.ID,
		gapID,
	)
	if err != nil || !found {
		t.Fatalf("query recovery gap = (%+v, %t, %v)", gap, found, err)
	}
	return gap
}

func (fixture *recorderRecoveryFixture) gapCount(t *testing.T) int {
	t.Helper()
	var count int
	if err := fixture.writer.QueryRow(
		`SELECT COUNT(*) FROM capture_gaps WHERE session_id = ? AND kind = ?`,
		fixture.session.ID,
		recorderRecoveryGapKind,
	).Scan(&count); err != nil {
		t.Fatalf("count recovery gaps: %v", err)
	}
	return count
}

func (fixture *recorderRecoveryFixture) assertRecordingStatus(t *testing.T, want RecordingStatus) {
	t.Helper()
	var got RecordingStatus
	if err := fixture.writer.QueryRow(
		`SELECT recording_status FROM live_sessions WHERE id = ?`,
		fixture.session.ID,
	).Scan(&got); err != nil {
		t.Fatalf("query recording status: %v", err)
	}
	if got != want {
		t.Fatalf("recording status = %q, want %q", got, want)
	}
}
