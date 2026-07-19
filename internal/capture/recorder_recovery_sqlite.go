package capture

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	recorderRecoveryGapKind       = "recording_restart"
	recorderRecoveryGapSeverity   = "warning"
	recorderRecoveryDedupePrefix  = "recorder-recovery:"
	recorderRecoveryDetailsSchema = 1
)

const recorderRecoveryGapSelectSQL = `SELECT
	id, session_id, media_segment_id, kind, started_at, ended_at,
	start_offset_ms, end_offset_ms, severity, recovered, reason_code,
	details_json, dedupe_key
	FROM capture_gaps`

type recorderRecoveryGapDetails struct {
	Version           int    `json:"version"`
	SourceAttemptID   string `json:"sourceAttemptId"`
	SourceOperationID string `json:"sourceOperationId"`
	SourceErrorCode   string `json:"sourceErrorCode"`
	RestartAttempts   int    `json:"restartAttempts"`
	LastErrorCode     string `json:"lastErrorCode"`
	LastOccurredAtMS  int64  `json:"lastOccurredAtMs"`
	ClockUncertain    bool   `json:"clockUncertain,omitempty"`
}

type recorderRecoveryGap struct {
	ID               string
	SessionID        string
	StartedAtMS      int64
	EndedAtMS        *int64
	StartOffsetMS    int64
	EndOffsetMS      *int64
	Recovered        bool
	ReasonCode       string
	DetailsJSON      string
	DedupeKey        string
	Details          recorderRecoveryGapDetails
	mediaSegmentIDOK bool
	kind             string
	severity         string
}

var _ RecorderRecoveryJournal = (*SQLiteRepository)(nil)

func (r *SQLiteRepository) BeginRecorderRecovery(
	ctx context.Context,
	input BeginRecorderRecoveryInput,
) (RecorderRecoveryJournalEntry, error) {
	occurredAtMS, err := validateBeginRecorderRecoveryInput(ctx, input)
	if err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}

	unlock := r.lockManifestSession(input.SessionID)
	defer unlock()
	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return RecorderRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
	}
	defer tx.Rollback()

	current, err := loadRecorderRecoverySession(ctx, tx, input.SessionID)
	if err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}
	if current.Status != input.ExpectedStatus ||
		current.OperationID != input.ExpectedOperationID ||
		(current.RecordingStatus != input.ExpectedRecordingStatus &&
			current.RecordingStatus != RecordingReconnecting) {
		return RecorderRecoveryJournalEntry{}, ErrStaleRecovery
	}
	if occurredAtMS < current.StartedAt {
		return RecorderRecoveryJournalEntry{}, ErrRecoveryGapConflict
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		tx,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND dedupe_key = ?`,
		input.SessionID,
		recorderRecoveryDedupeKey(input.SourceAttemptID),
	)
	if err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}
	if current.Status == input.ExpectedStatus &&
		current.RecordingStatus == RecordingReconnecting &&
		current.OperationID == input.ExpectedOperationID {
		if !found || !recorderRecoveryGapMatchesBegin(gap, current, input, occurredAtMS) {
			return RecorderRecoveryJournalEntry{}, ErrRecoveryGapConflict
		}
		if err := requireRecorderRecoverySourceAttemptWithClock(
			ctx,
			tx,
			input.SessionID,
			input.SourceAttemptID,
			occurredAtMS,
			true,
			input.ClockUncertain,
		); err != nil {
			return RecorderRecoveryJournalEntry{}, err
		}
		_ = tx.Rollback()
		current = r.materializeCommittedLocked(ctx, current)
		return RecorderRecoveryJournalEntry{Session: current, GapID: gap.ID}, nil
	}
	if current.RecordingStatus != input.ExpectedRecordingStatus {
		return RecorderRecoveryJournalEntry{}, ErrStaleRecovery
	}
	if found {
		return RecorderRecoveryJournalEntry{}, ErrRecoveryGapConflict
	}
	if err := requireRecorderRecoverySourceAttemptWithClock(
		ctx,
		tx,
		input.SessionID,
		input.SourceAttemptID,
		occurredAtMS,
		true,
		input.ClockUncertain,
	); err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}
	var gapCount int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM capture_gaps WHERE session_id = ? AND kind = ?`,
		input.SessionID,
		recorderRecoveryGapKind,
	).Scan(&gapCount); err != nil {
		return RecorderRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
	}
	if gapCount >= maximumRecoveryGaps {
		return RecorderRecoveryJournalEntry{}, ErrRecoveryGapConflict
	}

	gapID, err := uuid.NewV7()
	if err != nil {
		return RecorderRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
	}
	details := recorderRecoveryGapDetails{
		Version:           recorderRecoveryDetailsSchema,
		SourceAttemptID:   input.SourceAttemptID,
		SourceOperationID: input.ExpectedOperationID,
		SourceErrorCode:   input.ErrorCode,
		RestartAttempts:   0,
		LastErrorCode:     input.ErrorCode,
		LastOccurredAtMS:  occurredAtMS,
		ClockUncertain:    input.ClockUncertain,
	}
	detailsJSON, err := encodeRecorderRecoveryGapDetails(details)
	if err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}
	next := recorderRecoveryNextSession(r, current, RecordingReconnecting)
	staged, stageErr := r.stageManifest(next)
	if stageErr != nil {
		return RecorderRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
	}
	defer staged.discard()

	inserted, err := tx.ExecContext(ctx, `INSERT INTO capture_gaps(
		id, session_id, media_segment_id, kind, started_at, ended_at,
		start_offset_ms, end_offset_ms, severity, recovered, reason_code,
		details_json, dedupe_key
	) VALUES (?, ?, NULL, ?, ?, NULL, ?, NULL, ?, 0, ?, ?, ?)
	ON CONFLICT(session_id, dedupe_key) DO NOTHING`,
		gapID.String(),
		input.SessionID,
		recorderRecoveryGapKind,
		occurredAtMS,
		occurredAtMS-current.StartedAt,
		recorderRecoveryGapSeverity,
		input.ErrorCode,
		detailsJSON,
		recorderRecoveryDedupeKey(input.SourceAttemptID),
	)
	if err != nil {
		return RecorderRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
	}
	affected, err := inserted.RowsAffected()
	if err != nil {
		return RecorderRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
	}
	if affected != 1 {
		_ = tx.Rollback()
		resolved, resolveErr := r.resolveBeginRecorderRecoveryOutcome(input, occurredAtMS)
		if resolveErr != nil {
			return RecorderRecoveryJournalEntry{}, ErrRecoveryGapConflict
		}
		resolved.Session = r.materializeCommittedLocked(ctx, resolved.Session)
		return resolved, nil
	}
	if err := updateRecorderRecoverySession(
		ctx,
		tx,
		current,
		next,
		input.ExpectedRecordingStatus,
	); err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		resolved, resolveErr := r.resolveBeginRecorderRecoveryOutcome(input, occurredAtMS)
		if resolveErr != nil {
			return RecorderRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
		}
		resolved.Session = r.materializeRecorderRecoveryCommitLocked(
			ctx,
			staged,
			next,
			resolved.Session,
		)
		return resolved, nil
	}
	next = r.promoteCommittedLocked(ctx, staged, next)
	return RecorderRecoveryJournalEntry{Session: next, GapID: gapID.String()}, nil
}

func (r *SQLiteRepository) CompleteRecorderRecovery(
	ctx context.Context,
	input CompleteRecorderRecoveryInput,
) (LiveSession, error) {
	completedAtMS, err := validateCompleteRecorderRecoveryInput(ctx, input)
	if err != nil {
		return LiveSession{}, err
	}

	unlock := r.lockManifestSession(input.SessionID)
	defer unlock()
	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return LiveSession{}, recorderRecoveryPersistenceError(ctx)
	}
	defer tx.Rollback()
	current, err := loadRecorderRecoverySession(ctx, tx, input.SessionID)
	if err != nil {
		return LiveSession{}, err
	}
	if current.Status != input.ExpectedStatus ||
		current.OperationID != input.ExpectedOperationID ||
		(current.RecordingStatus != input.ExpectedRecordingStatus &&
			current.RecordingStatus != RecordingActive) {
		return LiveSession{}, ErrStaleRecovery
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		tx,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil {
		return LiveSession{}, err
	}
	if !found {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	if current.RecordingStatus == RecordingActive {
		if !recorderRecoveryGapMatchesComplete(gap, current, input, completedAtMS) {
			return LiveSession{}, ErrRecoveryGapConflict
		}
		_ = tx.Rollback()
		return r.materializeCommittedLocked(ctx, current), nil
	}
	if current.RecordingStatus != input.ExpectedRecordingStatus {
		return LiveSession{}, ErrStaleRecovery
	}
	if !recorderRecoveryGapReadyForTerminalMutation(gap, current) ||
		completedAtMS < gap.StartedAtMS {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	if err := requireRecorderRecoverySourceAttempt(
		ctx,
		tx,
		input.SessionID,
		gap.Details.SourceAttemptID,
		completedAtMS,
		true,
	); err != nil {
		return LiveSession{}, err
	}

	details := gap.Details
	details.RestartAttempts = input.RestartAttempts
	details.LastOccurredAtMS = completedAtMS
	detailsJSON, err := encodeRecorderRecoveryGapDetails(details)
	if err != nil {
		return LiveSession{}, err
	}
	next := recorderRecoveryNextSession(r, current, RecordingActive)
	staged, stageErr := r.stageManifest(next)
	if stageErr != nil {
		return LiveSession{}, recorderRecoveryPersistenceError(ctx)
	}
	defer staged.discard()
	updated, err := tx.ExecContext(ctx, `UPDATE capture_gaps SET
		ended_at = ?, end_offset_ms = ?, recovered = 1, details_json = ?
		WHERE id = ? AND session_id = ? AND media_segment_id IS NULL
		AND kind = ? AND severity = ? AND ended_at IS NULL AND end_offset_ms IS NULL
		AND recovered = 0 AND reason_code = ? AND details_json = ? AND dedupe_key = ?`,
		completedAtMS,
		completedAtMS-current.StartedAt,
		detailsJSON,
		gap.ID,
		gap.SessionID,
		recorderRecoveryGapKind,
		recorderRecoveryGapSeverity,
		gap.ReasonCode,
		gap.DetailsJSON,
		gap.DedupeKey,
	)
	if err != nil {
		return LiveSession{}, recorderRecoveryPersistenceError(ctx)
	}
	if affected, rowsErr := updated.RowsAffected(); rowsErr != nil || affected != 1 {
		_ = tx.Rollback()
		resolved, resolveErr := r.resolveCompleteRecorderRecoveryOutcome(input, completedAtMS)
		if resolveErr == nil {
			return r.materializeCommittedLocked(ctx, resolved), nil
		}
		return LiveSession{}, ErrRecoveryGapConflict
	}
	if err := updateRecorderRecoverySession(
		ctx,
		tx,
		current,
		next,
		input.ExpectedRecordingStatus,
	); err != nil {
		return LiveSession{}, err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		resolved, resolveErr := r.resolveCompleteRecorderRecoveryOutcome(input, completedAtMS)
		if resolveErr != nil {
			return LiveSession{}, recorderRecoveryPersistenceError(ctx)
		}
		return r.materializeRecorderRecoveryCommitLocked(ctx, staged, next, resolved), nil
	}
	return r.promoteCommittedLocked(ctx, staged, next), nil
}

func (r *SQLiteRepository) ExhaustRecorderRecovery(
	ctx context.Context,
	input ExhaustRecorderRecoveryInput,
) (LiveSession, error) {
	exhaustedAtMS, err := validateExhaustRecorderRecoveryInput(ctx, input)
	if err != nil {
		return LiveSession{}, err
	}

	unlock := r.lockManifestSession(input.SessionID)
	defer unlock()
	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return LiveSession{}, recorderRecoveryPersistenceError(ctx)
	}
	defer tx.Rollback()
	current, err := loadRecorderRecoverySession(ctx, tx, input.SessionID)
	if err != nil {
		return LiveSession{}, err
	}
	if current.Status != input.ExpectedStatus ||
		current.OperationID != input.ExpectedOperationID ||
		(current.RecordingStatus != input.ExpectedRecordingStatus &&
			current.RecordingStatus != RecordingUnavailable) {
		return LiveSession{}, ErrStaleRecovery
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		tx,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil {
		return LiveSession{}, err
	}
	if !found {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	if current.RecordingStatus == RecordingUnavailable {
		if !recorderRecoveryGapMatchesExhaust(gap, current, input, exhaustedAtMS) {
			return LiveSession{}, ErrRecoveryGapConflict
		}
		_ = tx.Rollback()
		return r.materializeCommittedLocked(ctx, current), nil
	}
	if current.RecordingStatus != input.ExpectedRecordingStatus {
		return LiveSession{}, ErrStaleRecovery
	}
	if !recorderRecoveryGapReadyForTerminalMutation(gap, current) ||
		exhaustedAtMS < gap.StartedAtMS {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	if err := requireRecorderRecoverySourceAttempt(
		ctx,
		tx,
		input.SessionID,
		gap.Details.SourceAttemptID,
		exhaustedAtMS,
		true,
	); err != nil {
		return LiveSession{}, err
	}

	details := gap.Details
	details.RestartAttempts = input.RestartAttempts
	details.LastErrorCode = input.ErrorCode
	details.LastOccurredAtMS = exhaustedAtMS
	detailsJSON, err := encodeRecorderRecoveryGapDetails(details)
	if err != nil {
		return LiveSession{}, err
	}
	next := recorderRecoveryNextSession(r, current, RecordingUnavailable)
	staged, stageErr := r.stageManifest(next)
	if stageErr != nil {
		return LiveSession{}, recorderRecoveryPersistenceError(ctx)
	}
	defer staged.discard()
	updated, err := tx.ExecContext(ctx, `UPDATE capture_gaps SET
		reason_code = ?, details_json = ?
		WHERE id = ? AND session_id = ? AND media_segment_id IS NULL
		AND kind = ? AND severity = ? AND ended_at IS NULL AND end_offset_ms IS NULL
		AND recovered = 0 AND reason_code = ? AND details_json = ? AND dedupe_key = ?`,
		input.ErrorCode,
		detailsJSON,
		gap.ID,
		gap.SessionID,
		recorderRecoveryGapKind,
		recorderRecoveryGapSeverity,
		gap.ReasonCode,
		gap.DetailsJSON,
		gap.DedupeKey,
	)
	if err != nil {
		return LiveSession{}, recorderRecoveryPersistenceError(ctx)
	}
	if affected, rowsErr := updated.RowsAffected(); rowsErr != nil || affected != 1 {
		_ = tx.Rollback()
		resolved, resolveErr := r.resolveExhaustRecorderRecoveryOutcome(input, exhaustedAtMS)
		if resolveErr == nil {
			return r.materializeCommittedLocked(ctx, resolved), nil
		}
		return LiveSession{}, ErrRecoveryGapConflict
	}
	if err := updateRecorderRecoverySession(
		ctx,
		tx,
		current,
		next,
		input.ExpectedRecordingStatus,
	); err != nil {
		return LiveSession{}, err
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		resolved, resolveErr := r.resolveExhaustRecorderRecoveryOutcome(input, exhaustedAtMS)
		if resolveErr != nil {
			return LiveSession{}, recorderRecoveryPersistenceError(ctx)
		}
		return r.materializeRecorderRecoveryCommitLocked(ctx, staged, next, resolved), nil
	}
	return r.promoteCommittedLocked(ctx, staged, next), nil
}

func (r *SQLiteRepository) CloseRecorderRecovery(
	ctx context.Context,
	input CloseRecorderRecoveryInput,
) error {
	closedAtMS, err := validateCloseRecorderRecoveryInput(ctx, input)
	if err != nil {
		return err
	}

	unlock := r.lockManifestSession(input.SessionID)
	defer unlock()
	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return recorderRecoveryPersistenceError(ctx)
	}
	defer tx.Rollback()
	current, err := loadRecorderRecoverySession(ctx, tx, input.SessionID)
	if err != nil {
		return err
	}
	if current.Status != input.ExpectedStatus ||
		current.RecordingStatus != input.ExpectedRecordingStatus ||
		current.OperationID != input.ExpectedOperationID {
		return ErrStaleRecovery
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		tx,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil {
		return err
	}
	if !found || closedAtMS < gap.StartedAtMS {
		return ErrRecoveryGapConflict
	}
	if err := requireRecorderRecoverySourceAttempt(
		ctx,
		tx,
		input.SessionID,
		gap.Details.SourceAttemptID,
		closedAtMS,
		false,
	); err != nil {
		return err
	}
	if recorderRecoveryGapMatchesClose(gap, current, input, closedAtMS) {
		_ = tx.Rollback()
		return nil
	}
	if gap.EndedAtMS != nil || gap.EndOffsetMS != nil || gap.Recovered {
		return ErrRecoveryGapConflict
	}

	details := gap.Details
	details.LastErrorCode = input.ErrorCode
	details.LastOccurredAtMS = closedAtMS
	detailsJSON, err := encodeRecorderRecoveryGapDetails(details)
	if err != nil {
		return err
	}
	recovered := 0
	if input.Recovered {
		recovered = 1
	}
	updated, err := tx.ExecContext(ctx, `UPDATE capture_gaps SET
		ended_at = ?, end_offset_ms = ?, recovered = ?, reason_code = ?, details_json = ?
		WHERE id = ? AND session_id = ? AND media_segment_id IS NULL
		AND kind = ? AND severity = ? AND ended_at IS NULL AND end_offset_ms IS NULL
		AND recovered = 0 AND reason_code = ? AND details_json = ? AND dedupe_key = ?`,
		closedAtMS,
		closedAtMS-current.StartedAt,
		recovered,
		input.ErrorCode,
		detailsJSON,
		gap.ID,
		gap.SessionID,
		recorderRecoveryGapKind,
		recorderRecoveryGapSeverity,
		gap.ReasonCode,
		gap.DetailsJSON,
		gap.DedupeKey,
	)
	if err != nil {
		return recorderRecoveryPersistenceError(ctx)
	}
	if affected, rowsErr := updated.RowsAffected(); rowsErr != nil || affected != 1 {
		_ = tx.Rollback()
		if resolveErr := r.resolveCloseRecorderRecoveryOutcome(input, closedAtMS); resolveErr == nil {
			return nil
		}
		return ErrRecoveryGapConflict
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		if resolveErr := r.resolveCloseRecorderRecoveryOutcome(input, closedAtMS); resolveErr == nil {
			return nil
		}
		return recorderRecoveryPersistenceError(ctx)
	}
	return nil
}

func validateBeginRecorderRecoveryInput(
	ctx context.Context,
	input BeginRecorderRecoveryInput,
) (int64, error) {
	if err := validateRecorderRecoveryContext(ctx); err != nil {
		return 0, err
	}
	if validateUUIDv7("session id", input.SessionID) != nil ||
		validateUUIDv7("expected operation id", input.ExpectedOperationID) != nil ||
		validateUUIDv7("source attempt id", input.SourceAttemptID) != nil ||
		!recorderRecoveryActiveSessionStatus(input.ExpectedStatus) ||
		input.ExpectedRecordingStatus != RecordingActive ||
		!validRecorderRecoveryErrorCode(input.ErrorCode) {
		return 0, ErrRecoveryContractInvalid
	}
	return recorderRecoveryTimeMS(input.OccurredAt)
}

func validateCompleteRecorderRecoveryInput(
	ctx context.Context,
	input CompleteRecorderRecoveryInput,
) (int64, error) {
	if err := validateRecorderRecoveryContext(ctx); err != nil {
		return 0, err
	}
	if validateUUIDv7("session id", input.SessionID) != nil ||
		validateUUIDv7("gap id", input.GapID) != nil ||
		validateUUIDv7("expected operation id", input.ExpectedOperationID) != nil ||
		!recorderRecoveryActiveSessionStatus(input.ExpectedStatus) ||
		input.ExpectedRecordingStatus != RecordingReconnecting ||
		input.RestartAttempts < 0 ||
		input.RestartAttempts > defaultRecorderRecoveryMaximumAttempts {
		return 0, ErrRecoveryContractInvalid
	}
	return recorderRecoveryTimeMS(input.CompletedAt)
}

func validateExhaustRecorderRecoveryInput(
	ctx context.Context,
	input ExhaustRecorderRecoveryInput,
) (int64, error) {
	if err := validateRecorderRecoveryContext(ctx); err != nil {
		return 0, err
	}
	if validateUUIDv7("session id", input.SessionID) != nil ||
		validateUUIDv7("gap id", input.GapID) != nil ||
		validateUUIDv7("expected operation id", input.ExpectedOperationID) != nil ||
		!recorderRecoveryActiveSessionStatus(input.ExpectedStatus) ||
		input.ExpectedRecordingStatus != RecordingReconnecting ||
		input.RestartAttempts < 0 ||
		input.RestartAttempts > defaultRecorderRecoveryMaximumAttempts ||
		!validRecorderRecoveryErrorCode(input.ErrorCode) {
		return 0, ErrRecoveryContractInvalid
	}
	return recorderRecoveryTimeMS(input.ExhaustedAt)
}

func validateCloseRecorderRecoveryInput(
	ctx context.Context,
	input CloseRecorderRecoveryInput,
) (int64, error) {
	if err := validateRecorderRecoveryContext(ctx); err != nil {
		return 0, err
	}
	if validateUUIDv7("session id", input.SessionID) != nil ||
		validateUUIDv7("gap id", input.GapID) != nil ||
		validateUUIDv7("expected operation id", input.ExpectedOperationID) != nil ||
		!validSessionStatus(input.ExpectedStatus) ||
		!validRecordingStatus(input.ExpectedRecordingStatus) ||
		!validRecorderRecoveryErrorCode(input.ErrorCode) {
		return 0, ErrRecoveryContractInvalid
	}
	return recorderRecoveryTimeMS(input.ClosedAt)
}

func validateRecorderRecoveryContext(ctx context.Context) error {
	if ctx == nil {
		return ErrRecoveryContractInvalid
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrRecoveryPersistence, err)
	}
	return nil
}

func recorderRecoveryTimeMS(value time.Time) (int64, error) {
	if value.IsZero() {
		return 0, ErrRecoveryContractInvalid
	}
	valueMS := value.UTC().UnixMilli()
	if valueMS < 0 {
		return 0, ErrRecoveryContractInvalid
	}
	return valueMS, nil
}

func recorderRecoveryActiveSessionStatus(status SessionStatus) bool {
	return status == SessionStarting || status == SessionRecording
}

func validRecorderRecoveryErrorCode(code string) bool {
	return code != "" && normalizeRecorderRecoveryErrorCode(code) == code
}

func recorderRecoveryDedupeKey(sourceAttemptID string) string {
	return recorderRecoveryDedupePrefix + sourceAttemptID
}

func encodeRecorderRecoveryGapDetails(details recorderRecoveryGapDetails) (string, error) {
	if !validRecorderRecoveryGapDetails(details) {
		return "", ErrRecoveryContractInvalid
	}
	payload, err := json.Marshal(details)
	if err != nil || len(payload) > maximumRecoveryDetailsJSON {
		return "", ErrRecoveryContractInvalid
	}
	return string(payload), nil
}

func decodeRecorderRecoveryGapDetails(payload string) (recorderRecoveryGapDetails, error) {
	if payload == "" || len(payload) > maximumRecoveryDetailsJSON {
		return recorderRecoveryGapDetails{}, ErrRecoveryGapConflict
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	var details recorderRecoveryGapDetails
	if err := decoder.Decode(&details); err != nil {
		return recorderRecoveryGapDetails{}, ErrRecoveryGapConflict
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return recorderRecoveryGapDetails{}, ErrRecoveryGapConflict
	}
	encoded, err := json.Marshal(details)
	if err != nil || string(encoded) != payload || !validRecorderRecoveryGapDetails(details) {
		return recorderRecoveryGapDetails{}, ErrRecoveryGapConflict
	}
	return details, nil
}

func validRecorderRecoveryGapDetails(details recorderRecoveryGapDetails) bool {
	return details.Version == recorderRecoveryDetailsSchema &&
		validateUUIDv7("source attempt id", details.SourceAttemptID) == nil &&
		validateUUIDv7("source operation id", details.SourceOperationID) == nil &&
		validRecorderRecoveryErrorCode(details.SourceErrorCode) &&
		details.RestartAttempts >= 0 &&
		details.RestartAttempts <= defaultRecorderRecoveryMaximumAttempts &&
		validRecorderRecoveryErrorCode(details.LastErrorCode) &&
		details.LastOccurredAtMS >= 0
}

func queryRecorderRecoveryGap(
	ctx context.Context,
	queryer queryRower,
	query string,
	args ...any,
) (recorderRecoveryGap, bool, error) {
	var gap recorderRecoveryGap
	var mediaSegmentID sql.NullString
	var endedAtMS, endOffsetMS sql.NullInt64
	var recovered int
	err := queryer.QueryRowContext(ctx, query, args...).Scan(
		&gap.ID,
		&gap.SessionID,
		&mediaSegmentID,
		&gap.kind,
		&gap.StartedAtMS,
		&endedAtMS,
		&gap.StartOffsetMS,
		&endOffsetMS,
		&gap.severity,
		&recovered,
		&gap.ReasonCode,
		&gap.DetailsJSON,
		&gap.DedupeKey,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return recorderRecoveryGap{}, false, nil
	}
	if err != nil {
		return recorderRecoveryGap{}, false, recorderRecoveryPersistenceError(ctx)
	}
	gap.mediaSegmentIDOK = mediaSegmentID.Valid
	if endedAtMS.Valid {
		value := endedAtMS.Int64
		gap.EndedAtMS = &value
	}
	if endOffsetMS.Valid {
		value := endOffsetMS.Int64
		gap.EndOffsetMS = &value
	}
	gap.Recovered = recovered == 1
	gap.Details, err = decodeRecorderRecoveryGapDetails(gap.DetailsJSON)
	if err != nil || !validRecorderRecoveryGap(gap, recovered) {
		return recorderRecoveryGap{}, false, ErrRecoveryGapConflict
	}
	return gap, true, nil
}

func validRecorderRecoveryGap(gap recorderRecoveryGap, recovered int) bool {
	if validateUUIDv7("gap id", gap.ID) != nil ||
		validateUUIDv7("gap session id", gap.SessionID) != nil ||
		gap.mediaSegmentIDOK ||
		gap.kind != recorderRecoveryGapKind ||
		gap.severity != recorderRecoveryGapSeverity ||
		gap.StartedAtMS < 0 ||
		gap.StartOffsetMS < 0 ||
		recovered < 0 ||
		recovered > 1 ||
		!validRecorderRecoveryErrorCode(gap.ReasonCode) ||
		gap.ReasonCode != gap.Details.LastErrorCode ||
		gap.DedupeKey != recorderRecoveryDedupeKey(gap.Details.SourceAttemptID) ||
		gap.Details.LastOccurredAtMS < gap.StartedAtMS {
		return false
	}
	if gap.EndedAtMS == nil {
		return gap.EndOffsetMS == nil && !gap.Recovered
	}
	return gap.EndOffsetMS != nil &&
		*gap.EndedAtMS >= gap.StartedAtMS &&
		*gap.EndOffsetMS >= gap.StartOffsetMS
}

func loadRecorderRecoverySession(
	ctx context.Context,
	queryer queryRower,
	sessionID string,
) (LiveSession, error) {
	session, err := querySession(ctx, queryer, sessionSelectSQL+` WHERE id = ?`, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, ErrStaleRecovery
	}
	if err != nil {
		return LiveSession{}, recorderRecoveryPersistenceError(ctx)
	}
	return session, nil
}

func requireRecorderRecoverySourceAttempt(
	ctx context.Context,
	queryer queryRower,
	sessionID string,
	sourceAttemptID string,
	eventAtMS int64,
	requireOpen bool,
) error {
	return requireRecorderRecoverySourceAttemptWithClock(
		ctx,
		queryer,
		sessionID,
		sourceAttemptID,
		eventAtMS,
		requireOpen,
		false,
	)
}

func requireRecorderRecoverySourceAttemptWithClock(
	ctx context.Context,
	queryer queryRower,
	sessionID string,
	sourceAttemptID string,
	eventAtMS int64,
	requireOpen bool,
	clockUncertain bool,
) error {
	var state SessionMediaState
	var attemptsJSON string
	err := queryer.QueryRowContext(
		ctx,
		`SELECT state, attempts_json FROM session_media WHERE session_id = ?`,
		sessionID,
	).Scan(&state, &attemptsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRecoveryGapConflict
	}
	if err != nil {
		return recorderRecoveryPersistenceError(ctx)
	}
	attempts, err := decodeMediaAttempts(attemptsJSON)
	if err != nil {
		return ErrRecoveryGapConflict
	}
	for _, attempt := range attempts {
		if attempt.ID != sourceAttemptID {
			continue
		}
		if !attempt.Committed ||
			(eventAtMS < attempt.StartedAt && !clockUncertain) {
			return ErrRecoveryGapConflict
		}
		if requireOpen && (state != SessionMediaOpen || attempt.Clean) {
			return ErrRecoveryGapConflict
		}
		return nil
	}
	return ErrRecoveryGapConflict
}

func recorderRecoveryNextSession(
	r *SQLiteRepository,
	current LiveSession,
	recordingStatus RecordingStatus,
) LiveSession {
	next := current
	next.RecordingStatus = recordingStatus
	next.ManifestDirty = true
	next.UpdatedAt = max(r.now().UTC().UnixMilli(), current.UpdatedAt+1)
	return next
}

func (r *SQLiteRepository) materializeRecorderRecoveryCommitLocked(
	ctx context.Context,
	staged *stagedManifest,
	stagedSession LiveSession,
	resolved LiveSession,
) LiveSession {
	if sameManifestVersion(stagedSession, resolved) {
		return r.promoteCommittedLocked(ctx, staged, resolved)
	}
	staged.discard()
	return r.materializeCommittedLocked(ctx, resolved)
}

func updateRecorderRecoverySession(
	ctx context.Context,
	tx *sql.Tx,
	current LiveSession,
	next LiveSession,
	expectedRecordingStatus RecordingStatus,
) error {
	result, err := tx.ExecContext(ctx, `UPDATE live_sessions SET
		recording_status = ?, manifest_dirty = 1, updated_at = ?
		WHERE id = ? AND status = ? AND recording_status = ?
		AND operation_id = ? AND updated_at = ?`,
		next.RecordingStatus,
		next.UpdatedAt,
		current.ID,
		current.Status,
		expectedRecordingStatus,
		current.OperationID,
		current.UpdatedAt,
	)
	if err != nil {
		return recorderRecoveryPersistenceError(ctx)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return recorderRecoveryPersistenceError(ctx)
	}
	if affected != 1 {
		return ErrStaleRecovery
	}
	return nil
}

func recorderRecoveryGapMatchesBegin(
	gap recorderRecoveryGap,
	session LiveSession,
	input BeginRecorderRecoveryInput,
	occurredAtMS int64,
) bool {
	return gap.SessionID == input.SessionID &&
		gap.EndedAtMS == nil &&
		gap.EndOffsetMS == nil &&
		!gap.Recovered &&
		gap.StartedAtMS == occurredAtMS &&
		gap.StartOffsetMS == occurredAtMS-session.StartedAt &&
		gap.ReasonCode == input.ErrorCode &&
		gap.Details.SourceAttemptID == input.SourceAttemptID &&
		gap.Details.SourceOperationID == input.ExpectedOperationID &&
		gap.Details.SourceErrorCode == input.ErrorCode &&
		gap.Details.RestartAttempts == 0 &&
		gap.Details.LastErrorCode == input.ErrorCode &&
		gap.Details.LastOccurredAtMS == occurredAtMS &&
		gap.Details.ClockUncertain == input.ClockUncertain
}

func recorderRecoveryGapReadyForTerminalMutation(
	gap recorderRecoveryGap,
	session LiveSession,
) bool {
	return gap.SessionID == session.ID &&
		gap.EndedAtMS == nil &&
		gap.EndOffsetMS == nil &&
		!gap.Recovered &&
		gap.StartOffsetMS == gap.StartedAtMS-session.StartedAt &&
		gap.Details.RestartAttempts == 0 &&
		gap.Details.LastErrorCode == gap.Details.SourceErrorCode &&
		gap.Details.LastOccurredAtMS == gap.StartedAtMS
}

func recorderRecoveryGapMatchesComplete(
	gap recorderRecoveryGap,
	session LiveSession,
	input CompleteRecorderRecoveryInput,
	completedAtMS int64,
) bool {
	return gap.SessionID == input.SessionID &&
		gap.EndedAtMS != nil &&
		*gap.EndedAtMS == completedAtMS &&
		gap.EndOffsetMS != nil &&
		*gap.EndOffsetMS == completedAtMS-session.StartedAt &&
		gap.Recovered &&
		gap.StartOffsetMS == gap.StartedAtMS-session.StartedAt &&
		gap.Details.RestartAttempts == input.RestartAttempts &&
		gap.Details.LastErrorCode == gap.Details.SourceErrorCode &&
		gap.Details.LastOccurredAtMS == completedAtMS
}

func recorderRecoveryGapMatchesExhaust(
	gap recorderRecoveryGap,
	session LiveSession,
	input ExhaustRecorderRecoveryInput,
	exhaustedAtMS int64,
) bool {
	return gap.SessionID == input.SessionID &&
		gap.EndedAtMS == nil &&
		gap.EndOffsetMS == nil &&
		!gap.Recovered &&
		gap.StartOffsetMS == gap.StartedAtMS-session.StartedAt &&
		gap.ReasonCode == input.ErrorCode &&
		gap.Details.RestartAttempts == input.RestartAttempts &&
		gap.Details.LastErrorCode == input.ErrorCode &&
		gap.Details.LastOccurredAtMS == exhaustedAtMS
}

func recorderRecoveryGapMatchesClose(
	gap recorderRecoveryGap,
	session LiveSession,
	input CloseRecorderRecoveryInput,
	closedAtMS int64,
) bool {
	return gap.SessionID == input.SessionID &&
		gap.EndedAtMS != nil &&
		*gap.EndedAtMS == closedAtMS &&
		gap.EndOffsetMS != nil &&
		*gap.EndOffsetMS == closedAtMS-session.StartedAt &&
		gap.Recovered == input.Recovered &&
		gap.StartOffsetMS == gap.StartedAtMS-session.StartedAt &&
		gap.ReasonCode == input.ErrorCode &&
		gap.Details.LastErrorCode == input.ErrorCode &&
		gap.Details.LastOccurredAtMS == closedAtMS
}

func (r *SQLiteRepository) resolveBeginRecorderRecoveryOutcome(
	input BeginRecorderRecoveryInput,
	occurredAtMS int64,
) (RecorderRecoveryJournalEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := loadRecorderRecoverySession(ctx, r.writer, input.SessionID)
	if err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		r.writer,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND dedupe_key = ?`,
		input.SessionID,
		recorderRecoveryDedupeKey(input.SourceAttemptID),
	)
	if err != nil {
		return RecorderRecoveryJournalEntry{}, err
	}
	if !found ||
		session.Status != input.ExpectedStatus ||
		session.RecordingStatus != RecordingReconnecting ||
		session.OperationID != input.ExpectedOperationID ||
		!recorderRecoveryGapMatchesBegin(gap, session, input, occurredAtMS) {
		return RecorderRecoveryJournalEntry{}, ErrRecoveryGapConflict
	}
	return RecorderRecoveryJournalEntry{Session: session, GapID: gap.ID}, nil
}

func (r *SQLiteRepository) resolveCompleteRecorderRecoveryOutcome(
	input CompleteRecorderRecoveryInput,
	completedAtMS int64,
) (LiveSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := loadRecorderRecoverySession(ctx, r.writer, input.SessionID)
	if err != nil {
		return LiveSession{}, err
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		r.writer,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil {
		return LiveSession{}, err
	}
	if !found ||
		session.Status != input.ExpectedStatus ||
		session.RecordingStatus != RecordingActive ||
		session.OperationID != input.ExpectedOperationID ||
		!recorderRecoveryGapMatchesComplete(gap, session, input, completedAtMS) {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	return session, nil
}

func (r *SQLiteRepository) resolveExhaustRecorderRecoveryOutcome(
	input ExhaustRecorderRecoveryInput,
	exhaustedAtMS int64,
) (LiveSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := loadRecorderRecoverySession(ctx, r.writer, input.SessionID)
	if err != nil {
		return LiveSession{}, err
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		r.writer,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil {
		return LiveSession{}, err
	}
	if !found ||
		session.Status != input.ExpectedStatus ||
		session.RecordingStatus != RecordingUnavailable ||
		session.OperationID != input.ExpectedOperationID ||
		!recorderRecoveryGapMatchesExhaust(gap, session, input, exhaustedAtMS) {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	return session, nil
}

func (r *SQLiteRepository) resolveCloseRecorderRecoveryOutcome(
	input CloseRecorderRecoveryInput,
	closedAtMS int64,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := loadRecorderRecoverySession(ctx, r.writer, input.SessionID)
	if err != nil {
		return err
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		r.writer,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil {
		return err
	}
	if !found || !recorderRecoveryGapMatchesClose(gap, session, input, closedAtMS) {
		return ErrRecoveryGapConflict
	}
	return nil
}

func recorderRecoveryPersistenceError(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrRecoveryPersistence, err)
		}
	}
	return ErrRecoveryPersistence
}
