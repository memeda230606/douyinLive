package capture

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/google/uuid"
)

const (
	messageRecoveryGapKind       = "message_disconnect"
	messageRecoveryGapSeverity   = "warning"
	messageRecoveryDedupePrefix  = "message-recovery:"
	messageRecoveryDetailsSchema = 1
	maximumMessageRecoveryBegins = 1000000
)

const messageRecoveryGapSelectSQL = `SELECT
	id, session_id, media_segment_id, kind, started_at, ended_at,
	start_offset_ms, end_offset_ms, severity, recovered, reason_code,
	details_json, dedupe_key
	FROM capture_gaps`

type messageRecoveryGapDetails struct {
	Version           int    `json:"version"`
	OpenedOperationID string `json:"openedOperationId"`
	LastOperationID   string `json:"lastOperationId"`
	BeginAttempts     int    `json:"beginAttempts"`
	FirstErrorCode    string `json:"firstErrorCode"`
	LastErrorCode     string `json:"lastErrorCode"`
	LastOccurredAtMS  int64  `json:"lastOccurredAtMs"`
}

type messageRecoveryGap struct {
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
	Details          messageRecoveryGapDetails
	mediaSegmentIDOK bool
	kind             string
	severity         string
}

type messageRecoveryCommitFunc func(*sql.Tx) error

var _ MessageRecoveryJournal = (*SQLiteRepository)(nil)

func (r *SQLiteRepository) BeginMessageRecovery(
	ctx context.Context,
	input BeginMessageRecoveryInput,
) (MessageRecoveryJournalEntry, error) {
	return r.beginMessageRecovery(ctx, input, func(tx *sql.Tx) error {
		return tx.Commit()
	})
}

func (r *SQLiteRepository) beginMessageRecovery(
	ctx context.Context,
	input BeginMessageRecoveryInput,
	commit messageRecoveryCommitFunc,
) (MessageRecoveryJournalEntry, error) {
	if commit == nil {
		return MessageRecoveryJournalEntry{}, ErrRecoveryPersistence
	}
	occurredAtMS, err := validateBeginMessageRecoveryInput(ctx, input)
	if err != nil {
		return MessageRecoveryJournalEntry{}, err
	}
	unlock := r.lockManifestSession(input.SessionID)
	defer unlock()
	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return MessageRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
	}
	defer tx.Rollback()

	current, err := loadRecorderRecoverySession(ctx, tx, input.SessionID)
	if err != nil {
		return MessageRecoveryJournalEntry{}, err
	}
	if current.Status != input.ExpectedStatus {
		return MessageRecoveryJournalEntry{}, ErrStaleRecovery
	}
	targetRecording := messageRecoveryBeginRecording(input.ExpectedRecordingStatus)
	gap, found, err := queryOpenMessageRecoveryGap(ctx, tx, input.SessionID)
	if err != nil {
		return MessageRecoveryJournalEntry{}, err
	}
	// A wall-clock rollback during a reconnect episode must not move the
	// journal behind its already durable first disconnect.
	if found && occurredAtMS < gap.StartedAtMS {
		occurredAtMS = gap.StartedAtMS
	}
	if current.OperationID == input.OperationID {
		if current.RecordingStatus != targetRecording || !found ||
			!messageRecoveryGapMatchesBegin(gap, current, input, occurredAtMS) {
			return MessageRecoveryJournalEntry{}, ErrRecoveryGapConflict
		}
		_ = tx.Rollback()
		return MessageRecoveryJournalEntry{
			Session: r.materializeCommittedLocked(ctx, current),
			GapID:   gap.ID,
		}, nil
	}
	if current.OperationID != input.ExpectedOperationID ||
		current.RecordingStatus != input.ExpectedRecordingStatus {
		return MessageRecoveryJournalEntry{}, ErrStaleRecovery
	}
	if occurredAtMS < current.StartedAt {
		return MessageRecoveryJournalEntry{}, ErrRecoveryGapConflict
	}

	var gapID string
	if found {
		if !messageRecoveryGapReady(gap, current) ||
			gap.Details.BeginAttempts >= maximumMessageRecoveryBegins {
			return MessageRecoveryJournalEntry{}, ErrRecoveryGapConflict
		}
		details := gap.Details
		details.LastOperationID = input.OperationID
		details.BeginAttempts++
		details.LastErrorCode = input.ErrorCode
		details.LastOccurredAtMS = occurredAtMS
		detailsJSON, encodeErr := encodeMessageRecoveryGapDetails(details)
		if encodeErr != nil {
			return MessageRecoveryJournalEntry{}, encodeErr
		}
		updated, updateErr := tx.ExecContext(ctx, `UPDATE capture_gaps SET
			reason_code = ?, details_json = ?
			WHERE id = ? AND session_id = ? AND media_segment_id IS NULL
			AND kind = ? AND severity = ? AND ended_at IS NULL
			AND end_offset_ms IS NULL AND recovered = 0
			AND reason_code = ? AND details_json = ? AND dedupe_key = ?`,
			input.ErrorCode,
			detailsJSON,
			gap.ID,
			gap.SessionID,
			messageRecoveryGapKind,
			messageRecoveryGapSeverity,
			gap.ReasonCode,
			gap.DetailsJSON,
			gap.DedupeKey,
		)
		if updateErr != nil {
			return MessageRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
		}
		if affected, rowsErr := updated.RowsAffected(); rowsErr != nil || affected != 1 {
			return MessageRecoveryJournalEntry{}, ErrRecoveryGapConflict
		}
		gapID = gap.ID
	} else {
		var count int
		if countErr := tx.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM capture_gaps WHERE session_id = ? AND kind = ?`,
			input.SessionID,
			messageRecoveryGapKind,
		).Scan(&count); countErr != nil {
			return MessageRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
		}
		if count >= maximumRecoveryGaps {
			return MessageRecoveryJournalEntry{}, ErrRecoveryGapConflict
		}
		generated, idErr := uuid.NewV7()
		if idErr != nil {
			return MessageRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
		}
		gapID = generated.String()
		details := messageRecoveryGapDetails{
			Version:           messageRecoveryDetailsSchema,
			OpenedOperationID: input.OperationID,
			LastOperationID:   input.OperationID,
			BeginAttempts:     1,
			FirstErrorCode:    input.ErrorCode,
			LastErrorCode:     input.ErrorCode,
			LastOccurredAtMS:  occurredAtMS,
		}
		detailsJSON, encodeErr := encodeMessageRecoveryGapDetails(details)
		if encodeErr != nil {
			return MessageRecoveryJournalEntry{}, encodeErr
		}
		inserted, insertErr := tx.ExecContext(ctx, `INSERT INTO capture_gaps(
			id, session_id, media_segment_id, kind, started_at, ended_at,
			start_offset_ms, end_offset_ms, severity, recovered, reason_code,
			details_json, dedupe_key
		) VALUES (?, ?, NULL, ?, ?, NULL, ?, NULL, ?, 0, ?, ?, ?)
		ON CONFLICT(session_id, dedupe_key) DO NOTHING`,
			gapID,
			input.SessionID,
			messageRecoveryGapKind,
			occurredAtMS,
			occurredAtMS-current.StartedAt,
			messageRecoveryGapSeverity,
			input.ErrorCode,
			detailsJSON,
			messageRecoveryDedupeKey(input.OperationID),
		)
		if insertErr != nil {
			return MessageRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
		}
		if affected, rowsErr := inserted.RowsAffected(); rowsErr != nil || affected != 1 {
			return MessageRecoveryJournalEntry{}, ErrRecoveryGapConflict
		}
	}

	next := current
	next.OperationID = input.OperationID
	next.RecordingStatus = targetRecording
	next.ManifestDirty = true
	next.UpdatedAt = max(r.now().UTC().UnixMilli(), current.UpdatedAt+1)
	staged, stageErr := r.stageManifest(next)
	if stageErr != nil {
		return MessageRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
	}
	defer staged.discard()
	if err := updateMessageRecoverySession(ctx, tx, current, next); err != nil {
		return MessageRecoveryJournalEntry{}, err
	}
	if err := commit(tx); err != nil {
		_ = tx.Rollback()
		resolved, resolveErr := r.resolveBeginMessageRecoveryOutcome(input, occurredAtMS)
		if resolveErr != nil {
			return MessageRecoveryJournalEntry{}, recorderRecoveryPersistenceError(ctx)
		}
		resolved.Session = r.materializeRecorderRecoveryCommitLocked(
			ctx,
			staged,
			next,
			resolved.Session,
		)
		return resolved, nil
	}
	return MessageRecoveryJournalEntry{
		Session: r.promoteCommittedLocked(ctx, staged, next),
		GapID:   gapID,
	}, nil
}

func (r *SQLiteRepository) FinishMessageRecovery(
	ctx context.Context,
	input FinishMessageRecoveryInput,
) (LiveSession, error) {
	return r.finishMessageRecovery(ctx, input, func(tx *sql.Tx) error {
		return tx.Commit()
	})
}

func (r *SQLiteRepository) finishMessageRecovery(
	ctx context.Context,
	input FinishMessageRecoveryInput,
	commit messageRecoveryCommitFunc,
) (LiveSession, error) {
	if commit == nil {
		return LiveSession{}, ErrRecoveryPersistence
	}
	completedAtMS, err := validateFinishMessageRecoveryInput(ctx, input)
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
			current.RecordingStatus != input.TargetRecordingStatus) {
		return LiveSession{}, ErrStaleRecovery
	}
	gap, found, err := queryMessageRecoveryGap(
		ctx,
		tx,
		messageRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil || !found {
		if err != nil {
			return LiveSession{}, err
		}
		return LiveSession{}, ErrRecoveryGapConflict
	}
	recorderGap, err := loadOptionalRecorderGap(ctx, tx, current, input.RecorderGapID)
	if err != nil {
		return LiveSession{}, err
	}
	if completedAtMS < gap.StartedAtMS {
		completedAtMS = gap.StartedAtMS
	}
	if input.RecorderGapID != "" && completedAtMS < recorderGap.StartedAtMS {
		completedAtMS = recorderGap.StartedAtMS
	}
	if current.RecordingStatus == input.TargetRecordingStatus && gap.EndedAtMS != nil {
		if messageRecoveryGapMatchesFinish(gap, current, input, completedAtMS) &&
			recorderGapMatchesMessageFinish(recorderGap, current, input, completedAtMS) {
			_ = tx.Rollback()
			return r.materializeCommittedLocked(ctx, current), nil
		}
		return LiveSession{}, ErrRecoveryGapConflict
	}
	if current.RecordingStatus != input.ExpectedRecordingStatus ||
		!messageRecoveryGapReady(gap, current) ||
		completedAtMS < gap.StartedAtMS {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	if input.RecorderGapID != "" {
		if !recorderRecoveryGapReadyForTerminalMutation(recorderGap, current) ||
			completedAtMS < recorderGap.StartedAtMS {
			return LiveSession{}, ErrRecoveryGapConflict
		}
		if err := requireRecorderRecoverySourceAttempt(
			ctx,
			tx,
			input.SessionID,
			recorderGap.Details.SourceAttemptID,
			completedAtMS,
			true,
		); err != nil {
			return LiveSession{}, err
		}
	}

	next := recorderRecoveryNextSession(r, current, input.TargetRecordingStatus)
	staged, stageErr := r.stageManifest(next)
	if stageErr != nil {
		return LiveSession{}, recorderRecoveryPersistenceError(ctx)
	}
	defer staged.discard()
	if err := closeMessageGapForFinish(ctx, tx, current, gap, input, completedAtMS); err != nil {
		return LiveSession{}, err
	}
	if input.RecorderGapID != "" {
		if err := closeRecorderGapForMessageFinish(
			ctx,
			tx,
			current,
			recorderGap,
			input,
			completedAtMS,
		); err != nil {
			return LiveSession{}, err
		}
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
	if err := commit(tx); err != nil {
		_ = tx.Rollback()
		resolved, resolveErr := r.resolveFinishMessageRecoveryOutcome(input, completedAtMS)
		if resolveErr != nil {
			return LiveSession{}, recorderRecoveryPersistenceError(ctx)
		}
		return r.materializeRecorderRecoveryCommitLocked(ctx, staged, next, resolved), nil
	}
	return r.promoteCommittedLocked(ctx, staged, next), nil
}

func (r *SQLiteRepository) CloseMessageRecovery(
	ctx context.Context,
	input CloseMessageRecoveryInput,
) error {
	return r.closeMessageRecovery(ctx, input, func(tx *sql.Tx) error {
		return tx.Commit()
	})
}

func (r *SQLiteRepository) closeMessageRecovery(
	ctx context.Context,
	input CloseMessageRecoveryInput,
	commit messageRecoveryCommitFunc,
) error {
	if commit == nil {
		return ErrRecoveryPersistence
	}
	closedAtMS, err := validateCloseMessageRecoveryInput(ctx, input)
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
	gap, found, err := queryMessageRecoveryGap(
		ctx,
		tx,
		messageRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil || !found {
		if err != nil {
			return err
		}
		return ErrRecoveryGapConflict
	}
	recorderGap, err := loadOptionalRecorderGap(ctx, tx, current, input.RecorderGapID)
	if err != nil {
		return err
	}
	if closedAtMS < gap.StartedAtMS {
		closedAtMS = gap.StartedAtMS
	}
	if input.RecorderGapID != "" && closedAtMS < recorderGap.StartedAtMS {
		closedAtMS = recorderGap.StartedAtMS
	}
	if messageRecoveryGapMatchesClose(gap, current, input, closedAtMS) &&
		recorderGapMatchesMessageClose(recorderGap, current, input, closedAtMS) {
		_ = tx.Rollback()
		return nil
	}
	if !messageRecoveryGapReady(gap, current) || closedAtMS < gap.StartedAtMS {
		return ErrRecoveryGapConflict
	}
	if input.RecorderGapID != "" {
		if !recorderRecoveryGapReadyForTerminalMutation(recorderGap, current) ||
			closedAtMS < recorderGap.StartedAtMS {
			return ErrRecoveryGapConflict
		}
		if err := requireRecorderRecoverySourceAttempt(
			ctx,
			tx,
			input.SessionID,
			recorderGap.Details.SourceAttemptID,
			closedAtMS,
			false,
		); err != nil {
			return err
		}
	}
	finishInput := FinishMessageRecoveryInput{
		SessionID:               input.SessionID,
		GapID:                   input.GapID,
		ExpectedStatus:          input.ExpectedStatus,
		ExpectedRecordingStatus: input.ExpectedRecordingStatus,
		ExpectedOperationID:     input.ExpectedOperationID,
		TargetRecordingStatus:   input.ExpectedRecordingStatus,
		Recovered:               false,
		ErrorCode:               input.ErrorCode,
		CompletedAt:             input.ClosedAt,
		RecorderGapID:           input.RecorderGapID,
		RecorderRestartAttempts: input.RecorderRestartAttempts,
		RecorderErrorCode:       input.RecorderErrorCode,
	}
	if err := closeMessageGapForFinish(ctx, tx, current, gap, finishInput, closedAtMS); err != nil {
		return err
	}
	if input.RecorderGapID != "" {
		if err := closeRecorderGapForMessageFinish(
			ctx,
			tx,
			current,
			recorderGap,
			finishInput,
			closedAtMS,
		); err != nil {
			return err
		}
	}
	if err := commit(tx); err != nil {
		_ = tx.Rollback()
		if resolveErr := r.resolveCloseMessageRecoveryOutcome(input, closedAtMS); resolveErr != nil {
			return recorderRecoveryPersistenceError(ctx)
		}
	}
	return nil
}

func validateBeginMessageRecoveryInput(
	ctx context.Context,
	input BeginMessageRecoveryInput,
) (int64, error) {
	if err := validateRecorderRecoveryContext(ctx); err != nil {
		return 0, err
	}
	if validateUUIDv7("session id", input.SessionID) != nil ||
		validateUUIDv7("expected operation id", input.ExpectedOperationID) != nil ||
		validateUUIDv7("operation id", input.OperationID) != nil ||
		input.ExpectedOperationID == input.OperationID ||
		input.ExpectedStatus != SessionRecording ||
		!messageRecoveryMutableRecording(input.ExpectedRecordingStatus) ||
		!validMessageRecoveryErrorCode(input.ErrorCode) {
		return 0, ErrRecoveryContractInvalid
	}
	return recorderRecoveryTimeMS(input.OccurredAt)
}

func validateFinishMessageRecoveryInput(
	ctx context.Context,
	input FinishMessageRecoveryInput,
) (int64, error) {
	if err := validateRecorderRecoveryContext(ctx); err != nil {
		return 0, err
	}
	if validateUUIDv7("session id", input.SessionID) != nil ||
		validateUUIDv7("gap id", input.GapID) != nil ||
		validateUUIDv7("expected operation id", input.ExpectedOperationID) != nil ||
		input.ExpectedStatus != SessionRecording ||
		!messageRecoveryMutableRecording(input.ExpectedRecordingStatus) ||
		!validRecordingStatus(input.TargetRecordingStatus) ||
		!validMessageRecoveryErrorCode(input.ErrorCode) {
		return 0, ErrRecoveryContractInvalid
	}
	if input.Recovered {
		validTarget := input.ExpectedRecordingStatus == RecordingReconnecting &&
			input.TargetRecordingStatus == RecordingActive
		validTarget = validTarget ||
			(input.ExpectedRecordingStatus == RecordingDisabled &&
				input.TargetRecordingStatus == RecordingDisabled)
		validTarget = validTarget ||
			(input.ExpectedRecordingStatus == RecordingUnavailable &&
				input.TargetRecordingStatus == RecordingUnavailable)
		if !validTarget || input.ErrorCode != MessageRecoveryRecoveredErrorCode {
			return 0, ErrRecoveryContractInvalid
		}
	} else if input.ExpectedRecordingStatus != RecordingReconnecting ||
		input.TargetRecordingStatus != RecordingUnavailable ||
		input.ErrorCode != MessageRecoveryExhaustedErrorCode {
		return 0, ErrRecoveryContractInvalid
	}
	if err := validateOptionalRecorderMessageFields(
		input.RecorderGapID,
		input.RecorderRestartAttempts,
		input.RecorderErrorCode,
	); err != nil {
		return 0, err
	}
	return recorderRecoveryTimeMS(input.CompletedAt)
}

func validateCloseMessageRecoveryInput(
	ctx context.Context,
	input CloseMessageRecoveryInput,
) (int64, error) {
	if err := validateRecorderRecoveryContext(ctx); err != nil {
		return 0, err
	}
	if validateUUIDv7("session id", input.SessionID) != nil ||
		validateUUIDv7("gap id", input.GapID) != nil ||
		validateUUIDv7("expected operation id", input.ExpectedOperationID) != nil ||
		!validSessionStatus(input.ExpectedStatus) ||
		!validRecordingStatus(input.ExpectedRecordingStatus) ||
		input.ErrorCode != MessageRecoveryFinalizedErrorCode {
		return 0, ErrRecoveryContractInvalid
	}
	if err := validateOptionalRecorderMessageFields(
		input.RecorderGapID,
		input.RecorderRestartAttempts,
		input.RecorderErrorCode,
	); err != nil {
		return 0, err
	}
	return recorderRecoveryTimeMS(input.ClosedAt)
}

func validateOptionalRecorderMessageFields(gapID string, attempts int, code string) error {
	if gapID == "" {
		if attempts != 0 || code != "" {
			return ErrRecoveryContractInvalid
		}
		return nil
	}
	if validateUUIDv7("recorder gap id", gapID) != nil ||
		attempts < 0 || attempts > defaultRecorderRecoveryMaximumAttempts ||
		!validRecorderRecoveryErrorCode(code) {
		return ErrRecoveryContractInvalid
	}
	return nil
}

func messageRecoveryMutableRecording(status RecordingStatus) bool {
	switch status {
	case RecordingActive, RecordingReconnecting, RecordingDisabled, RecordingUnavailable:
		return true
	default:
		return false
	}
}

func validMessageRecoveryErrorCode(code string) bool {
	switch code {
	case MessageDisconnectErrorCode,
		MessageRecoveryRetryErrorCode,
		MessageSubscriptionErrorCode,
		MessageRecoveryRecoveredErrorCode,
		MessageRecoveryExhaustedErrorCode,
		MessageRecoveryFinalizedErrorCode:
		return true
	default:
		return false
	}
}

func messageRecoveryDedupeKey(operationID string) string {
	return messageRecoveryDedupePrefix + operationID
}

func encodeMessageRecoveryGapDetails(details messageRecoveryGapDetails) (string, error) {
	if !validMessageRecoveryGapDetails(details) {
		return "", ErrRecoveryContractInvalid
	}
	payload, err := json.Marshal(details)
	if err != nil || len(payload) > maximumRecoveryDetailsJSON {
		return "", ErrRecoveryContractInvalid
	}
	return string(payload), nil
}

func decodeMessageRecoveryGapDetails(payload string) (messageRecoveryGapDetails, error) {
	if payload == "" || len(payload) > maximumRecoveryDetailsJSON {
		return messageRecoveryGapDetails{}, ErrRecoveryGapConflict
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	var details messageRecoveryGapDetails
	if err := decoder.Decode(&details); err != nil {
		return messageRecoveryGapDetails{}, ErrRecoveryGapConflict
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return messageRecoveryGapDetails{}, ErrRecoveryGapConflict
	}
	encoded, err := json.Marshal(details)
	if err != nil || string(encoded) != payload || !validMessageRecoveryGapDetails(details) {
		return messageRecoveryGapDetails{}, ErrRecoveryGapConflict
	}
	return details, nil
}

func validMessageRecoveryGapDetails(details messageRecoveryGapDetails) bool {
	return details.Version == messageRecoveryDetailsSchema &&
		validateUUIDv7("opened operation id", details.OpenedOperationID) == nil &&
		validateUUIDv7("last operation id", details.LastOperationID) == nil &&
		details.BeginAttempts >= 1 &&
		details.BeginAttempts <= maximumMessageRecoveryBegins &&
		validMessageRecoveryErrorCode(details.FirstErrorCode) &&
		validMessageRecoveryErrorCode(details.LastErrorCode) &&
		details.LastOccurredAtMS >= 0
}

func queryMessageRecoveryGap(
	ctx context.Context,
	queryer queryRower,
	query string,
	args ...any,
) (messageRecoveryGap, bool, error) {
	var gap messageRecoveryGap
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
		return messageRecoveryGap{}, false, nil
	}
	if err != nil {
		return messageRecoveryGap{}, false, recorderRecoveryPersistenceError(ctx)
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
	gap.Details, err = decodeMessageRecoveryGapDetails(gap.DetailsJSON)
	if err != nil || !validMessageRecoveryGap(gap, recovered) {
		return messageRecoveryGap{}, false, ErrRecoveryGapConflict
	}
	return gap, true, nil
}

func queryOpenMessageRecoveryGap(
	ctx context.Context,
	queryer queryRower,
	sessionID string,
) (messageRecoveryGap, bool, error) {
	var count int
	if err := queryer.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM capture_gaps
		WHERE session_id = ? AND kind = ? AND ended_at IS NULL`,
		sessionID,
		messageRecoveryGapKind,
	).Scan(&count); err != nil {
		return messageRecoveryGap{}, false, recorderRecoveryPersistenceError(ctx)
	}
	if count > 1 {
		return messageRecoveryGap{}, false, ErrRecoveryGapConflict
	}
	if count == 0 {
		return messageRecoveryGap{}, false, nil
	}
	return queryMessageRecoveryGap(
		ctx,
		queryer,
		messageRecoveryGapSelectSQL+` WHERE session_id = ? AND kind = ? AND ended_at IS NULL`,
		sessionID,
		messageRecoveryGapKind,
	)
}

func validMessageRecoveryGap(gap messageRecoveryGap, recovered int) bool {
	if validateUUIDv7("gap id", gap.ID) != nil ||
		validateUUIDv7("gap session id", gap.SessionID) != nil ||
		gap.mediaSegmentIDOK || gap.kind != messageRecoveryGapKind ||
		gap.severity != messageRecoveryGapSeverity || gap.StartedAtMS < 0 ||
		gap.StartOffsetMS < 0 || recovered < 0 || recovered > 1 ||
		gap.ReasonCode != gap.Details.LastErrorCode ||
		gap.DedupeKey != messageRecoveryDedupeKey(gap.Details.OpenedOperationID) ||
		gap.Details.LastOccurredAtMS < gap.StartedAtMS {
		return false
	}
	if gap.EndedAtMS == nil {
		return gap.EndOffsetMS == nil && !gap.Recovered
	}
	return gap.EndOffsetMS != nil &&
		*gap.EndedAtMS >= gap.StartedAtMS &&
		*gap.EndOffsetMS >= gap.StartOffsetMS &&
		gap.Details.LastOccurredAtMS == *gap.EndedAtMS
}

func messageRecoveryGapReady(gap messageRecoveryGap, session LiveSession) bool {
	return gap.SessionID == session.ID && gap.EndedAtMS == nil &&
		gap.EndOffsetMS == nil && !gap.Recovered &&
		gap.StartOffsetMS == gap.StartedAtMS-session.StartedAt
}

func requireOpenMessageRecoveryGap(
	ctx context.Context,
	queryer queryRower,
	session LiveSession,
) error {
	gap, found, err := queryOpenMessageRecoveryGap(ctx, queryer, session.ID)
	if err != nil {
		return err
	}
	if !found || !messageRecoveryGapReady(gap, session) {
		return ErrRecoveryGapConflict
	}
	return nil
}

func messageRecoveryGapMatchesBegin(
	gap messageRecoveryGap,
	session LiveSession,
	input BeginMessageRecoveryInput,
	occurredAtMS int64,
) bool {
	return messageRecoveryGapReady(gap, session) &&
		gap.Details.LastOperationID == input.OperationID &&
		gap.ReasonCode == input.ErrorCode &&
		gap.Details.LastErrorCode == input.ErrorCode &&
		gap.Details.LastOccurredAtMS == occurredAtMS
}

func updateMessageRecoverySession(
	ctx context.Context,
	tx *sql.Tx,
	current LiveSession,
	next LiveSession,
) error {
	result, err := tx.ExecContext(ctx, `UPDATE live_sessions SET
		operation_id = ?, recording_status = ?, manifest_dirty = 1, updated_at = ?
		WHERE id = ? AND status = ? AND recording_status = ?
		AND operation_id = ? AND updated_at = ?`,
		next.OperationID,
		next.RecordingStatus,
		next.UpdatedAt,
		current.ID,
		current.Status,
		current.RecordingStatus,
		current.OperationID,
		current.UpdatedAt,
	)
	if err != nil {
		return recorderRecoveryPersistenceError(ctx)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil || affected != 1 {
		return ErrStaleRecovery
	}
	return nil
}

func loadOptionalRecorderGap(
	ctx context.Context,
	queryer queryRower,
	session LiveSession,
	gapID string,
) (recorderRecoveryGap, error) {
	if gapID == "" {
		return recorderRecoveryGap{}, nil
	}
	gap, found, err := queryRecorderRecoveryGap(
		ctx,
		queryer,
		recorderRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		session.ID,
		gapID,
	)
	if err != nil {
		return recorderRecoveryGap{}, err
	}
	if !found {
		return recorderRecoveryGap{}, ErrRecoveryGapConflict
	}
	return gap, nil
}

func closeMessageGapForFinish(
	ctx context.Context,
	tx *sql.Tx,
	session LiveSession,
	gap messageRecoveryGap,
	input FinishMessageRecoveryInput,
	completedAtMS int64,
) error {
	details := gap.Details
	details.LastErrorCode = input.ErrorCode
	details.LastOccurredAtMS = completedAtMS
	detailsJSON, err := encodeMessageRecoveryGapDetails(details)
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
		AND kind = ? AND severity = ? AND ended_at IS NULL
		AND end_offset_ms IS NULL AND recovered = 0
		AND reason_code = ? AND details_json = ? AND dedupe_key = ?`,
		completedAtMS,
		completedAtMS-session.StartedAt,
		recovered,
		input.ErrorCode,
		detailsJSON,
		gap.ID,
		gap.SessionID,
		messageRecoveryGapKind,
		messageRecoveryGapSeverity,
		gap.ReasonCode,
		gap.DetailsJSON,
		gap.DedupeKey,
	)
	if err != nil {
		return recorderRecoveryPersistenceError(ctx)
	}
	if affected, rowsErr := updated.RowsAffected(); rowsErr != nil || affected != 1 {
		return ErrRecoveryGapConflict
	}
	return nil
}

func closeRecorderGapForMessageFinish(
	ctx context.Context,
	tx *sql.Tx,
	session LiveSession,
	gap recorderRecoveryGap,
	input FinishMessageRecoveryInput,
	completedAtMS int64,
) error {
	details := gap.Details
	details.RestartAttempts = input.RecorderRestartAttempts
	details.LastOccurredAtMS = completedAtMS
	recovered := 0
	reasonCode := input.RecorderErrorCode
	if input.Recovered {
		recovered = 1
		reasonCode = gap.ReasonCode
		details.LastErrorCode = gap.Details.SourceErrorCode
	} else {
		details.LastErrorCode = input.RecorderErrorCode
	}
	detailsJSON, err := encodeRecorderRecoveryGapDetails(details)
	if err != nil {
		return err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE capture_gaps SET
		ended_at = ?, end_offset_ms = ?, recovered = ?, reason_code = ?, details_json = ?
		WHERE id = ? AND session_id = ? AND media_segment_id IS NULL
		AND kind = ? AND severity = ? AND ended_at IS NULL
		AND end_offset_ms IS NULL AND recovered = 0
		AND reason_code = ? AND details_json = ? AND dedupe_key = ?`,
		completedAtMS,
		completedAtMS-session.StartedAt,
		recovered,
		reasonCode,
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
		return ErrRecoveryGapConflict
	}
	return nil
}

func messageRecoveryGapMatchesFinish(
	gap messageRecoveryGap,
	session LiveSession,
	input FinishMessageRecoveryInput,
	completedAtMS int64,
) bool {
	return gap.SessionID == input.SessionID && gap.EndedAtMS != nil &&
		*gap.EndedAtMS == completedAtMS && gap.EndOffsetMS != nil &&
		*gap.EndOffsetMS == completedAtMS-session.StartedAt &&
		gap.Recovered == input.Recovered && gap.ReasonCode == input.ErrorCode &&
		gap.Details.LastErrorCode == input.ErrorCode &&
		gap.Details.LastOccurredAtMS == completedAtMS
}

func recorderGapMatchesMessageFinish(
	gap recorderRecoveryGap,
	session LiveSession,
	input FinishMessageRecoveryInput,
	completedAtMS int64,
) bool {
	if input.RecorderGapID == "" {
		return gap.ID == ""
	}
	if gap.ID != input.RecorderGapID || gap.EndedAtMS == nil ||
		*gap.EndedAtMS != completedAtMS || gap.EndOffsetMS == nil ||
		*gap.EndOffsetMS != completedAtMS-session.StartedAt ||
		gap.Recovered != input.Recovered ||
		gap.Details.RestartAttempts != input.RecorderRestartAttempts ||
		gap.Details.LastOccurredAtMS != completedAtMS {
		return false
	}
	if input.Recovered {
		return gap.ReasonCode == gap.Details.SourceErrorCode &&
			gap.Details.LastErrorCode == gap.Details.SourceErrorCode
	}
	return gap.ReasonCode == input.RecorderErrorCode &&
		gap.Details.LastErrorCode == input.RecorderErrorCode
}

func messageRecoveryGapMatchesClose(
	gap messageRecoveryGap,
	session LiveSession,
	input CloseMessageRecoveryInput,
	closedAtMS int64,
) bool {
	return gap.SessionID == input.SessionID && gap.EndedAtMS != nil &&
		*gap.EndedAtMS == closedAtMS && gap.EndOffsetMS != nil &&
		*gap.EndOffsetMS == closedAtMS-session.StartedAt && !gap.Recovered &&
		gap.ReasonCode == input.ErrorCode &&
		gap.Details.LastErrorCode == input.ErrorCode &&
		gap.Details.LastOccurredAtMS == closedAtMS
}

func recorderGapMatchesMessageClose(
	gap recorderRecoveryGap,
	session LiveSession,
	input CloseMessageRecoveryInput,
	closedAtMS int64,
) bool {
	if input.RecorderGapID == "" {
		return gap.ID == ""
	}
	return gap.ID == input.RecorderGapID && gap.EndedAtMS != nil &&
		*gap.EndedAtMS == closedAtMS && gap.EndOffsetMS != nil &&
		*gap.EndOffsetMS == closedAtMS-session.StartedAt && !gap.Recovered &&
		gap.ReasonCode == input.RecorderErrorCode &&
		gap.Details.RestartAttempts == input.RecorderRestartAttempts &&
		gap.Details.LastErrorCode == input.RecorderErrorCode &&
		gap.Details.LastOccurredAtMS == closedAtMS
}

func (r *SQLiteRepository) resolveBeginMessageRecoveryOutcome(
	input BeginMessageRecoveryInput,
	occurredAtMS int64,
) (MessageRecoveryJournalEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := loadRecorderRecoverySession(ctx, r.writer, input.SessionID)
	if err != nil {
		return MessageRecoveryJournalEntry{}, err
	}
	gap, found, err := queryOpenMessageRecoveryGap(ctx, r.writer, input.SessionID)
	if err != nil || !found {
		return MessageRecoveryJournalEntry{}, ErrRecoveryGapConflict
	}
	if session.Status != input.ExpectedStatus ||
		session.RecordingStatus != messageRecoveryBeginRecording(input.ExpectedRecordingStatus) ||
		session.OperationID != input.OperationID ||
		!messageRecoveryGapMatchesBegin(gap, session, input, occurredAtMS) {
		return MessageRecoveryJournalEntry{}, ErrRecoveryGapConflict
	}
	return MessageRecoveryJournalEntry{Session: session, GapID: gap.ID}, nil
}

func (r *SQLiteRepository) resolveFinishMessageRecoveryOutcome(
	input FinishMessageRecoveryInput,
	completedAtMS int64,
) (LiveSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := loadRecorderRecoverySession(ctx, r.writer, input.SessionID)
	if err != nil {
		return LiveSession{}, err
	}
	gap, found, err := queryMessageRecoveryGap(
		ctx,
		r.writer,
		messageRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil || !found {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	recorderGap, err := loadOptionalRecorderGap(ctx, r.writer, session, input.RecorderGapID)
	if err != nil {
		return LiveSession{}, err
	}
	if session.Status != input.ExpectedStatus ||
		session.RecordingStatus != input.TargetRecordingStatus ||
		session.OperationID != input.ExpectedOperationID ||
		!messageRecoveryGapMatchesFinish(gap, session, input, completedAtMS) ||
		!recorderGapMatchesMessageFinish(recorderGap, session, input, completedAtMS) {
		return LiveSession{}, ErrRecoveryGapConflict
	}
	return session, nil
}

func (r *SQLiteRepository) resolveCloseMessageRecoveryOutcome(
	input CloseMessageRecoveryInput,
	closedAtMS int64,
) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := loadRecorderRecoverySession(ctx, r.writer, input.SessionID)
	if err != nil {
		return err
	}
	gap, found, err := queryMessageRecoveryGap(
		ctx,
		r.writer,
		messageRecoveryGapSelectSQL+` WHERE session_id = ? AND id = ?`,
		input.SessionID,
		input.GapID,
	)
	if err != nil || !found {
		return ErrRecoveryGapConflict
	}
	recorderGap, err := loadOptionalRecorderGap(ctx, r.writer, session, input.RecorderGapID)
	if err != nil {
		return err
	}
	if !messageRecoveryGapMatchesClose(gap, session, input, closedAtMS) ||
		!recorderGapMatchesMessageClose(recorderGap, session, input, closedAtMS) {
		return ErrRecoveryGapConflict
	}
	return nil
}
