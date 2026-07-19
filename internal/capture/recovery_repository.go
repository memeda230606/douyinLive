package capture

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

type validatedRecoveryGap struct {
	input       RecoveryGapInput
	detailsJSON string
}

type validatedRecoverAndClose struct {
	input           RecoverAndCloseInput
	targetRecording RecordingStatus
	gaps            []validatedRecoveryGap
}

type recoveryCommitFunc func(*sql.Tx) error

func (r *SQLiteRepository) ListRecoverablePage(
	ctx context.Context,
	input RecoverablePageQuery,
) (RecoverableSessionPage, error) {
	var page RecoverableSessionPage
	if err := requireContext(ctx); err != nil {
		return page, err
	}
	if r == nil || r.reader == nil {
		return page, ErrRecoveryPersistence
	}
	if input.ScanCutoffMS <= 0 || input.Limit < 1 ||
		input.Limit > maximumRecoverablePageSize {
		return page, ErrRecoveryContractInvalid
	}
	if input.AfterID != "" {
		if err := validateUUIDv7("recoverable page cursor", input.AfterID); err != nil {
			return page, ErrRecoveryContractInvalid
		}
	}

	query := sessionSelectSQL +
		" WHERE status IN ('starting', 'recording', 'finalizing')" +
		" AND created_at <= ?"
	args := make([]any, 0, 3)
	args = append(args, input.ScanCutoffMS)
	if input.AfterID != "" {
		query += " AND id > ?"
		args = append(args, input.AfterID)
	}
	query += " ORDER BY id ASC LIMIT ?"
	args = append(args, input.Limit)

	rows, err := r.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return page, errors.Join(ErrRecoveryPersistence, err)
	}
	sessions := make([]LiveSession, 0, input.Limit)
	for rows.Next() {
		session, scanErr := scanSession(rows)
		if scanErr != nil {
			_ = rows.Close()
			return page, errors.Join(ErrRecoveryPersistence, scanErr)
		}
		sessions = append(sessions, session)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		return page, errors.Join(ErrRecoveryPersistence, rowsErr, closeErr)
	}

	// No manifest is materialized while paging. Filesystem work starts only
	// after this method has closed the SQLite rows and returned the snapshots.
	page.Sessions = sessions
	if len(sessions) == input.Limit {
		page.NextID = sessions[len(sessions)-1].ID
	}
	return page, nil
}

func (r *SQLiteRepository) RecoverAndClose(
	ctx context.Context,
	input RecoverAndCloseInput,
) (LiveSession, error) {
	return r.recoverAndClose(ctx, input, func(transaction *sql.Tx) error {
		return transaction.Commit()
	})
}

func (r *SQLiteRepository) recoverAndClose(
	ctx context.Context,
	input RecoverAndCloseInput,
	commit recoveryCommitFunc,
) (LiveSession, error) {
	if err := requireContext(ctx); err != nil {
		return LiveSession{}, err
	}
	if r == nil || r.writer == nil || commit == nil {
		return LiveSession{}, ErrRecoveryPersistence
	}
	validated, err := validateRecoverAndCloseInput(input)
	if err != nil {
		return LiveSession{}, err
	}

	unlock := r.lockManifestSession(input.SessionID)
	defer unlock()

	transaction, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return LiveSession{}, errors.Join(ErrRecoveryPersistence, err)
	}
	defer transaction.Rollback()

	current, err := querySession(
		ctx, transaction, sessionSelectSQL+" WHERE id = ?", input.SessionID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, ErrSessionNotFound
	}
	if err != nil {
		return LiveSession{}, errors.Join(ErrRecoveryPersistence, err)
	}
	if err := validateRecoveryTimeline(current, validated); err != nil {
		return LiveSession{}, err
	}
	if err := validateOpenRecoveryGapTimeline(
		ctx, transaction, current.ID, input.EndedAtMS,
	); err != nil {
		return LiveSession{}, err
	}
	if recoveryAlreadyApplied(current, validated) {
		if err := recoveryGapsMatch(ctx, transaction, current, validated.gaps); err != nil {
			return LiveSession{}, err
		}
		if err := requireNoOpenRecoveryGaps(ctx, transaction, current.ID); err != nil {
			return LiveSession{}, err
		}
		_ = transaction.Rollback()
		return r.materializeCommittedLocked(ctx, current), nil
	}
	if current.Status != input.ExpectedStatus ||
		current.RecordingStatus != input.ExpectedRecordingStatus ||
		current.OperationID != input.ExpectedOperationID ||
		current.CreatedAt > input.ScanCutoffMS {
		return LiveSession{}, ErrStaleRecovery
	}

	next := current
	next.OperationID = input.RecoveryOperationID
	next.Status = SessionInterrupted
	next.RecordingStatus = validated.targetRecording
	next.ManifestDirty = true
	endedAt := input.EndedAtMS
	next.EndedAt = &endedAt
	next.IntegrityScore = input.IntegrityScore
	next.UpdatedAt = max(r.now().UTC().UnixMilli(), current.UpdatedAt+1)

	staged, err := r.stageManifest(next)
	if err != nil {
		return LiveSession{}, err
	}
	defer staged.discard()

	updateSQL := "UPDATE live_sessions SET " +
		"operation_id = ?, status = 'interrupted', recording_status = ?, " +
		"manifest_dirty = 1, ended_at = ?, integrity_score = ?, updated_at = ? " +
		"WHERE id = ? AND status = ? AND recording_status = ? " +
		"AND operation_id = ? AND created_at <= ?"
	result, err := transaction.ExecContext(ctx, updateSQL,
		next.OperationID, next.RecordingStatus, endedAt, next.IntegrityScore,
		next.UpdatedAt, current.ID, input.ExpectedStatus,
		input.ExpectedRecordingStatus, input.ExpectedOperationID,
		input.ScanCutoffMS,
	)
	if err != nil {
		return LiveSession{}, errors.Join(ErrRecoveryPersistence, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return LiveSession{}, errors.Join(ErrRecoveryPersistence, err)
	}
	if affected != 1 {
		return LiveSession{}, ErrStaleRecovery
	}
	if err := closeOpenRecoveryGaps(
		ctx, transaction, current, input.EndedAtMS,
	); err != nil {
		return LiveSession{}, err
	}
	if err := persistRecoveryGaps(ctx, transaction, current, validated.gaps); err != nil {
		return LiveSession{}, err
	}
	if err := requireNoOpenRecoveryGaps(ctx, transaction, current.ID); err != nil {
		return LiveSession{}, err
	}

	if err := commit(transaction); err != nil {
		_ = transaction.Rollback()
		resolved, resolveErr := r.resolveRecoveryOutcome(validated)
		if resolveErr == nil {
			return r.promoteCommittedLocked(ctx, staged, resolved), nil
		}
		return LiveSession{}, errors.Join(ErrRecoveryPersistence, err, resolveErr)
	}
	return r.promoteCommittedLocked(ctx, staged, next), nil
}

func validateRecoverAndCloseInput(
	input RecoverAndCloseInput,
) (validatedRecoverAndClose, error) {
	validated := validatedRecoverAndClose{input: input}
	if validateUUIDv7("recovery session", input.SessionID) != nil ||
		!activeSessionStatus(input.ExpectedStatus) ||
		!validRecordingStatus(input.ExpectedRecordingStatus) ||
		validateUUIDv7("recovery operation", input.RecoveryOperationID) != nil ||
		input.RecoveryOperationID == input.ExpectedOperationID ||
		input.ScanCutoffMS <= 0 || input.EndedAtMS < 0 ||
		input.IntegrityScore < 0 || input.IntegrityScore > 1 ||
		len(input.Gaps) > maximumRecoveryGaps {
		return validated, ErrRecoveryContractInvalid
	}
	if input.ExpectedOperationID != "" &&
		validateUUIDv7("expected recovery operation", input.ExpectedOperationID) != nil {
		return validated, ErrRecoveryContractInvalid
	}
	if input.ExpectedRecordingStatus == RecordingDisabled {
		validated.targetRecording = RecordingDisabled
	} else {
		validated.targetRecording = RecordingIncomplete
	}

	validated.gaps = make([]validatedRecoveryGap, 0, len(input.Gaps))
	seenDedupeKeys := make(map[string]struct{}, len(input.Gaps))
	for _, gap := range input.Gaps {
		normalized, err := validateRecoveryGap(gap)
		if err != nil {
			return validatedRecoverAndClose{}, err
		}
		if _, exists := seenDedupeKeys[gap.DedupeKey]; exists {
			return validatedRecoverAndClose{}, ErrRecoveryContractInvalid
		}
		seenDedupeKeys[gap.DedupeKey] = struct{}{}
		validated.gaps = append(validated.gaps, normalized)
	}
	sort.Slice(validated.gaps, func(left, right int) bool {
		return validated.gaps[left].input.DedupeKey <
			validated.gaps[right].input.DedupeKey
	})
	return validated, nil
}

func validateRecoveryGap(input RecoveryGapInput) (validatedRecoveryGap, error) {
	var validated validatedRecoveryGap
	if validateUUIDv7("recovery gap", input.ID) != nil ||
		!validRecoveryGapKind(input.Kind) ||
		!validRecoveryGapSeverity(input.Severity) ||
		input.StartedAtMS < 0 || input.EndedAtMS == nil ||
		(input.EndedAtMS != nil && *input.EndedAtMS < input.StartedAtMS) ||
		!validRecoveryCode(input.ReasonCode) ||
		!validRecoveryDedupeKey(input.DedupeKey) {
		return validated, ErrRecoveryContractInvalid
	}
	if input.MediaSegmentID != "" &&
		validateUUIDv7("recovery gap media segment", input.MediaSegmentID) != nil {
		return validated, ErrRecoveryContractInvalid
	}
	details, err := canonicalRecoveryDetails(input.DetailsJSON)
	if err != nil {
		return validated, err
	}
	validated.input = input
	validated.detailsJSON = details
	return validated, nil
}

func validRecoveryGapKind(value string) bool {
	switch value {
	case "message_disconnect", "recording_restart", "stream_unavailable",
		"disk_full", "process_crash", "clock_uncertain", "event_persistence":
		return true
	default:
		return false
	}
}

func validRecoveryGapSeverity(value string) bool {
	return value == "info" || value == "warning" || value == "error"
}

func validRecoveryCode(value string) bool {
	if value == "" || len(value) > 96 {
		return false
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func validRecoveryDedupeKey(value string) bool {
	if value == "" || len(value) > 512 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func canonicalRecoveryDetails(value string) (string, error) {
	if value == "" {
		value = "{}"
	}
	if len(value) > maximumRecoveryDetailsJSON {
		return "", ErrRecoveryContractInvalid
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return "", ErrRecoveryContractInvalid
	}
	if _, ok := decoded.(map[string]any); !ok {
		return "", ErrRecoveryContractInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", ErrRecoveryContractInvalid
	}
	encoded, err := json.Marshal(decoded)
	if err != nil || len(encoded) > maximumRecoveryDetailsJSON {
		return "", ErrRecoveryContractInvalid
	}
	return string(encoded), nil
}

func validateRecoveryTimeline(
	session LiveSession,
	validated validatedRecoverAndClose,
) error {
	input := validated.input
	if input.EndedAtMS < session.StartedAt {
		return ErrRecoveryContractInvalid
	}
	for _, gap := range validated.gaps {
		if gap.input.StartedAtMS < session.StartedAt ||
			gap.input.StartedAtMS > input.EndedAtMS ||
			(gap.input.EndedAtMS != nil &&
				*gap.input.EndedAtMS > input.EndedAtMS) {
			return ErrRecoveryContractInvalid
		}
	}
	return nil
}

func recoveryAlreadyApplied(
	session LiveSession,
	validated validatedRecoverAndClose,
) bool {
	input := validated.input
	return session.OperationID == input.RecoveryOperationID &&
		session.Status == SessionInterrupted &&
		session.RecordingStatus == validated.targetRecording &&
		session.EndedAt != nil && *session.EndedAt == input.EndedAtMS &&
		session.IntegrityScore == input.IntegrityScore &&
		session.CreatedAt <= input.ScanCutoffMS
}

func validateOpenRecoveryGapTimeline(
	ctx context.Context,
	queryer queryRower,
	sessionID string,
	endedAtMS int64,
) error {
	var inverted int
	err := queryer.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM capture_gaps "+
			"WHERE session_id = ? AND ended_at IS NULL AND started_at > ?",
		sessionID,
		endedAtMS,
	).Scan(&inverted)
	if err != nil {
		return errors.Join(ErrRecoveryPersistence, err)
	}
	if inverted != 0 {
		return ErrRecoveryContractInvalid
	}
	return nil
}

func closeOpenRecoveryGaps(
	ctx context.Context,
	transaction *sql.Tx,
	session LiveSession,
	endedAtMS int64,
) error {
	if err := validateOpenRecoveryGapTimeline(
		ctx, transaction, session.ID, endedAtMS,
	); err != nil {
		return err
	}
	endOffsetMS := endedAtMS - session.StartedAt
	_, err := transaction.ExecContext(
		ctx,
		"UPDATE capture_gaps SET ended_at = ?, end_offset_ms = ?, recovered = 0 "+
			"WHERE session_id = ? AND ended_at IS NULL",
		endedAtMS,
		endOffsetMS,
		session.ID,
	)
	if err != nil {
		return errors.Join(ErrRecoveryPersistence, err)
	}
	return requireNoOpenRecoveryGaps(ctx, transaction, session.ID)
}

func requireNoOpenRecoveryGaps(
	ctx context.Context,
	queryer queryRower,
	sessionID string,
) error {
	var open int
	err := queryer.QueryRowContext(
		ctx,
		"SELECT COUNT(*) FROM capture_gaps "+
			"WHERE session_id = ? AND ended_at IS NULL",
		sessionID,
	).Scan(&open)
	if err != nil {
		return errors.Join(ErrRecoveryPersistence, err)
	}
	if open != 0 {
		return ErrRecoveryGapConflict
	}
	return nil
}

func persistRecoveryGaps(
	ctx context.Context,
	transaction *sql.Tx,
	session LiveSession,
	gaps []validatedRecoveryGap,
) error {
	for _, gap := range gaps {
		if err := persistRecoveryGap(ctx, transaction, session, gap); err != nil {
			return err
		}
	}
	return nil
}

const insertRecoveryGapSQL = "INSERT INTO capture_gaps(" +
	"id, session_id, media_segment_id, kind, started_at, ended_at, " +
	"start_offset_ms, end_offset_ms, severity, recovered, reason_code, " +
	"details_json, dedupe_key" +
	") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) " +
	"ON CONFLICT(session_id, dedupe_key) DO NOTHING"

func persistRecoveryGap(
	ctx context.Context,
	transaction *sql.Tx,
	session LiveSession,
	gap validatedRecoveryGap,
) error {
	startOffset := gap.input.StartedAtMS - session.StartedAt
	var endOffset *int64
	if gap.input.EndedAtMS != nil {
		value := *gap.input.EndedAtMS - session.StartedAt
		endOffset = &value
	}
	result, err := transaction.ExecContext(ctx, insertRecoveryGapSQL,
		gap.input.ID, session.ID, nullableText(gap.input.MediaSegmentID),
		gap.input.Kind, gap.input.StartedAtMS, gap.input.EndedAtMS,
		startOffset, endOffset, gap.input.Severity, boolToInt(gap.input.Recovered),
		gap.input.ReasonCode, gap.detailsJSON, gap.input.DedupeKey,
	)
	if err != nil {
		if matches, matchErr := recoveryGapMatches(
			ctx, transaction, session, gap,
		); matchErr == nil && matches {
			return nil
		}
		return errors.Join(ErrRecoveryGapConflict, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return errors.Join(ErrRecoveryPersistence, err)
	}
	if affected == 1 {
		return nil
	}
	matches, err := recoveryGapMatches(ctx, transaction, session, gap)
	if err != nil {
		return err
	}
	if !matches {
		return ErrRecoveryGapConflict
	}
	return nil
}

func recoveryGapsMatch(
	ctx context.Context,
	queryer queryRower,
	session LiveSession,
	gaps []validatedRecoveryGap,
) error {
	for _, gap := range gaps {
		matches, err := recoveryGapMatches(ctx, queryer, session, gap)
		if err != nil {
			return err
		}
		if !matches {
			return ErrRecoveryGapConflict
		}
	}
	return nil
}

func recoveryGapMatches(
	ctx context.Context,
	queryer queryRower,
	session LiveSession,
	gap validatedRecoveryGap,
) (bool, error) {
	var (
		mediaSegmentID          sql.NullString
		kind, severity          string
		reason, details, dedupe string
		startedAt, startOffset  int64
		endedAt, endOffset      sql.NullInt64
		recovered               int
	)
	selectSQL := "SELECT media_segment_id, kind, started_at, ended_at, " +
		"start_offset_ms, end_offset_ms, severity, recovered, reason_code, " +
		"details_json, dedupe_key FROM capture_gaps " +
		"WHERE session_id = ? AND dedupe_key = ?"
	err := queryer.QueryRowContext(
		ctx, selectSQL, session.ID, gap.input.DedupeKey,
	).Scan(
		&mediaSegmentID, &kind, &startedAt, &endedAt, &startOffset,
		&endOffset, &severity, &recovered, &reason, &details, &dedupe,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errors.Join(ErrRecoveryPersistence, err)
	}
	wantStartOffset := gap.input.StartedAtMS - session.StartedAt
	var wantEndOffset *int64
	if gap.input.EndedAtMS != nil {
		value := *gap.input.EndedAtMS - session.StartedAt
		wantEndOffset = &value
	}
	return nullableStringMatches(mediaSegmentID, gap.input.MediaSegmentID) &&
		kind == gap.input.Kind &&
		startedAt == gap.input.StartedAtMS &&
		nullableInt64Matches(endedAt, gap.input.EndedAtMS) &&
		startOffset == wantStartOffset &&
		nullableInt64Matches(endOffset, wantEndOffset) &&
		severity == gap.input.Severity &&
		recovered == boolToInt(gap.input.Recovered) &&
		reason == gap.input.ReasonCode &&
		details == gap.detailsJSON &&
		dedupe == gap.input.DedupeKey, nil
}

func nullableStringMatches(value sql.NullString, want string) bool {
	return value.Valid == (want != "") && (!value.Valid || value.String == want)
}

func nullableInt64Matches(value sql.NullInt64, want *int64) bool {
	return value.Valid == (want != nil) &&
		(!value.Valid || value.Int64 == *want)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (r *SQLiteRepository) resolveRecoveryOutcome(
	validated validatedRecoverAndClose,
) (LiveSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := querySession(
		ctx, r.writer, sessionSelectSQL+" WHERE id = ?",
		validated.input.SessionID,
	)
	if err != nil {
		return LiveSession{}, err
	}
	if !recoveryAlreadyApplied(session, validated) {
		return LiveSession{}, ErrStaleRecovery
	}
	if err := recoveryGapsMatch(ctx, r.writer, session, validated.gaps); err != nil {
		return LiveSession{}, err
	}
	if err := requireNoOpenRecoveryGaps(ctx, r.writer, session.ID); err != nil {
		return LiveSession{}, err
	}
	return session, nil
}
