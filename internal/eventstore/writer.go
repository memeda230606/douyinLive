package eventstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"time"

	modernsqlite "modernc.org/sqlite"
)

var (
	ErrWriterUnavailable     = errors.New("event persistence writer is unavailable")
	ErrInvalidBatch          = errors.New("event persistence batch is invalid")
	ErrCheckpointConflict    = errors.New("event persistence checkpoint conflict")
	ErrPersistenceConstraint = errors.New("event persistence constraint failed")
	ErrPersistenceBusy       = errors.New("event persistence is busy")
	ErrPersistenceCorrupt    = errors.New("event persistence database is corrupt")
	ErrPersistenceFull       = errors.New("event persistence storage is full")
	ErrPersistenceCommit     = errors.New("event persistence commit failed")
)

const (
	maxIdentifierLength = 512
	maxRelativeFile     = 1024
	maxJSONLength       = 4 << 20
)

// Writer commits normalized event batches and their recovery checkpoint in one
// SQLite transaction. The supplied DB must be the application's single writer.
type Writer struct {
	db *sql.DB
}

func NewWriter(db *sql.DB) (*Writer, error) {
	if db == nil {
		return nil, ErrWriterUnavailable
	}
	return &Writer{db: db}, nil
}

// Checkpoint returns the latest committed durable position for a session.
func (w *Writer) Checkpoint(ctx context.Context, sessionID string) (Checkpoint, bool, error) {
	if w == nil || w.db == nil {
		return Checkpoint{}, false, ErrWriterUnavailable
	}
	if ctx == nil || !validIdentifier(sessionID) {
		return Checkpoint{}, false, ErrInvalidBatch
	}
	checkpoint, found, err := readCheckpoint(ctx, w.db, sessionID)
	if err != nil {
		return Checkpoint{}, false, classifyPersistenceError(err)
	}
	return checkpoint, found, nil
}

// PersistBatch writes source/aggregate events, gift fold state, capture gaps,
// and the checkpoint atomically. A targeted dedupe conflict is idempotent;
// unrelated primary-key, foreign-key, and CHECK failures remain visible through
// ErrPersistenceConstraint and roll back the entire batch.
func (w *Writer) PersistBatch(ctx context.Context, batch Batch) error {
	if w == nil || w.db == nil {
		return ErrWriterUnavailable
	}
	if ctx == nil {
		return ErrInvalidBatch
	}
	combos := collapseCombos(batch.GiftCombos)
	batch.GiftCombos = combos
	if !validBatch(batch) {
		return ErrInvalidBatch
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrPersistenceCommit, err)
	}

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return classifyPersistenceError(err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	current, found, err := readCheckpoint(ctx, tx, batch.SessionID)
	if err != nil {
		return classifyPersistenceError(err)
	}
	replay := false
	sealedReplay := false
	switch {
	case found && sameCheckpointPosition(current, batch.Checkpoint):
		replay = true
		sealedReplay = current.State == CheckpointClosed
	case !found && batch.PreviousSequence == 0:
	case found && current.CommittedSequence == batch.PreviousSequence:
		if current.State == CheckpointClosed {
			return ErrCheckpointConflict
		}
		if current.PrivacyKeyID != batch.Checkpoint.PrivacyKeyID ||
			!validCheckpointTransition(current.State, batch.Checkpoint.State) {
			return ErrCheckpointConflict
		}
	default:
		return ErrCheckpointConflict
	}

	for _, event := range batch.Events {
		inserted, err := persistEvent(ctx, tx, event)
		if err != nil {
			return classifyPersistenceError(err)
		}
		if inserted && (sealedReplay || replay && event.Role == EventRoleSource) {
			return ErrCheckpointConflict
		}
	}
	for _, combo := range batch.GiftCombos {
		if combo.Status == ComboClosed {
			role, err := eventRole(ctx, tx, combo.SessionID, combo.AggregateEventID)
			if err != nil {
				return classifyPersistenceError(err)
			}
			if role != EventRoleAggregate {
				return ErrPersistenceConstraint
			}
		}
		if sealedReplay {
			matches, err := comboMatches(ctx, tx, combo)
			if err != nil {
				return classifyPersistenceError(err)
			}
			if !matches {
				return ErrCheckpointConflict
			}
			continue
		}
		if err := persistCombo(ctx, tx, combo); err != nil {
			return classifyPersistenceError(err)
		}
	}
	for _, gap := range batch.Gaps {
		inserted, err := persistGap(ctx, tx, gap)
		if err != nil {
			return classifyPersistenceError(err)
		}
		if sealedReplay && inserted {
			return ErrCheckpointConflict
		}
	}

	if !replay {
		updated, err := persistCheckpoint(ctx, tx, batch.Checkpoint, batch.PreviousSequence)
		if err != nil {
			return classifyPersistenceError(err)
		}
		if !updated {
			return ErrCheckpointConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return classifyPersistenceError(err)
	}
	committed = true
	return nil
}

const insertEventSQL = `INSERT INTO live_events(
	id, session_id, ingest_sequence, event_role, method, kind,
	platform_message_id, dedupe_key, message_create_at, received_at,
	session_offset_ms, clock_confidence, user_hash, display_name, content,
	numeric_value, normalized_json, raw_file, raw_offset, raw_length,
	parse_status, parse_error_code, normalizer_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, dedupe_key) DO NOTHING`

func persistEvent(ctx context.Context, tx *sql.Tx, event Event) (bool, error) {
	normalizedJSON := event.NormalizedJSON
	if normalizedJSON == "" {
		normalizedJSON = "{}"
	}
	var rawFile, rawOffset, rawLength any
	if event.Raw.File != "" {
		rawFile = event.Raw.File
		rawOffset = event.Raw.Offset
		rawLength = event.Raw.Length
	}
	result, err := tx.ExecContext(ctx, insertEventSQL,
		event.ID, event.SessionID, event.IngestSequence, event.Role,
		event.Method, event.Kind, nullString(event.PlatformMessageID), event.DedupeKey,
		timeMillisPtr(event.MessageCreateAt), event.ReceivedAt.UTC().UnixMilli(),
		event.SessionOffsetMS, event.ClockConfidence, nullString(event.UserHash),
		nullString(event.DisplayName), nullString(event.Content), event.NumericValue,
		normalizedJSON, rawFile, rawOffset, rawLength, event.ParseStatus,
		nullString(event.ParseErrorCode), event.NormalizerVersion,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

const upsertComboSQL = `INSERT INTO gift_combo_states(
	session_id, combo_key, status, user_hash, gift_id, gift_name,
	total_count, total_value, first_ingest_sequence, last_ingest_sequence,
	started_at, updated_at, closed_at, aggregate_event_id, normalizer_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, combo_key) DO UPDATE SET
	status = excluded.status,
	gift_name = excluded.gift_name,
	total_count = excluded.total_count,
	total_value = excluded.total_value,
	last_ingest_sequence = excluded.last_ingest_sequence,
	updated_at = excluded.updated_at,
	closed_at = excluded.closed_at,
	aggregate_event_id = excluded.aggregate_event_id,
	normalizer_version = excluded.normalizer_version
WHERE gift_combo_states.status = 'open'
	AND excluded.first_ingest_sequence = gift_combo_states.first_ingest_sequence
	AND excluded.gift_id = gift_combo_states.gift_id
	AND excluded.user_hash IS gift_combo_states.user_hash
	AND excluded.last_ingest_sequence >= gift_combo_states.last_ingest_sequence
	AND excluded.updated_at >= gift_combo_states.updated_at
	AND excluded.total_count >= gift_combo_states.total_count`

func persistCombo(ctx context.Context, tx *sql.Tx, combo GiftComboState) error {
	result, err := tx.ExecContext(ctx, upsertComboSQL, comboValues(combo)...)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 1 {
		return nil
	}
	matches, err := comboMatches(ctx, tx, combo)
	if err != nil {
		return err
	}
	if !matches {
		return ErrCheckpointConflict
	}
	return nil
}

func comboValues(combo GiftComboState) []any {
	return []any{
		combo.SessionID, combo.ComboKey, combo.Status, nullString(combo.UserHash),
		combo.GiftID, nullString(combo.GiftName), combo.TotalCount, combo.TotalValue,
		combo.FirstSequence, combo.LastSequence, combo.StartedAt.UTC().UnixMilli(),
		combo.UpdatedAt.UTC().UnixMilli(), timeMillisPtr(combo.ClosedAt),
		nullString(combo.AggregateEventID), combo.NormalizerVersion,
	}
}

const selectComboSQL = `SELECT status, user_hash, gift_id, gift_name, total_count,
	total_value, first_ingest_sequence, last_ingest_sequence, started_at,
	updated_at, closed_at, aggregate_event_id, normalizer_version
FROM gift_combo_states WHERE session_id = ? AND combo_key = ?`

func comboMatches(ctx context.Context, tx *sql.Tx, combo GiftComboState) (bool, error) {
	var (
		status, giftID, normalizerVersion string
		userHash, giftName, aggregateID   sql.NullString
		totalCount, first, last           int64
		totalValue                        sql.NullFloat64
		startedAt, updatedAt              int64
		closedAt                          sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, selectComboSQL, combo.SessionID, combo.ComboKey).Scan(
		&status, &userHash, &giftID, &giftName, &totalCount, &totalValue,
		&first, &last, &startedAt, &updatedAt, &closedAt, &aggregateID,
		&normalizerVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status == string(combo.Status) && nullableStringEqual(userHash, combo.UserHash) &&
		giftID == combo.GiftID && nullableStringEqual(giftName, combo.GiftName) &&
		totalCount == combo.TotalCount && nullableFloatEqual(totalValue, combo.TotalValue) &&
		first == combo.FirstSequence && last == combo.LastSequence &&
		startedAt == combo.StartedAt.UTC().UnixMilli() && updatedAt == combo.UpdatedAt.UTC().UnixMilli() &&
		nullableTimeEqual(closedAt, combo.ClosedAt) && nullableStringEqual(aggregateID, combo.AggregateEventID) &&
		normalizerVersion == combo.NormalizerVersion, nil
}

const insertGapSQL = `INSERT INTO capture_gaps(
	id, session_id, media_segment_id, kind, started_at, ended_at,
	start_offset_ms, end_offset_ms, severity, recovered, reason_code,
	details_json, dedupe_key
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id, dedupe_key) DO UPDATE SET
	ended_at = excluded.ended_at,
	end_offset_ms = excluded.end_offset_ms,
	details_json = excluded.details_json
WHERE capture_gaps.id = excluded.id
	AND capture_gaps.media_segment_id IS excluded.media_segment_id
	AND capture_gaps.kind = 'event_persistence'
	AND excluded.kind = 'event_persistence'
	AND capture_gaps.started_at = excluded.started_at
	AND capture_gaps.start_offset_ms = excluded.start_offset_ms
	AND capture_gaps.severity = excluded.severity
	AND capture_gaps.recovered = excluded.recovered
	AND capture_gaps.reason_code = excluded.reason_code
	AND capture_gaps.ended_at IS NOT NULL
	AND excluded.ended_at IS NOT NULL
	AND excluded.ended_at >= capture_gaps.ended_at
	AND capture_gaps.end_offset_ms IS NOT NULL
	AND excluded.end_offset_ms IS NOT NULL
	AND excluded.end_offset_ms >= capture_gaps.end_offset_ms
	AND json_type(capture_gaps.details_json, '$.count') = 'integer'
	AND json_type(excluded.details_json, '$.count') = 'integer'
	AND CAST(json_extract(excluded.details_json, '$.count') AS INTEGER) >
		CAST(json_extract(capture_gaps.details_json, '$.count') AS INTEGER)`

func persistGap(ctx context.Context, tx *sql.Tx, gap CaptureGap) (bool, error) {
	details := gap.DetailsJSON
	if details == "" {
		details = "{}"
	}
	result, err := tx.ExecContext(ctx, insertGapSQL,
		gap.ID, gap.SessionID, nullString(gap.MediaSegmentID), gap.Kind,
		gap.StartedAt.UTC().UnixMilli(), timeMillisPtr(gap.EndedAt),
		gap.StartOffsetMS, gap.EndOffsetMS, gap.Severity, boolInt(gap.Recovered),
		gap.ReasonCode, details, gap.DedupeKey,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 1 || gap.Kind != "event_persistence" {
		return rows == 1, err
	}
	count, cumulative := eventPersistenceGapCount(gap.DetailsJSON)
	if !cumulative {
		return false, nil
	}
	dominates, err := persistedGapDominates(ctx, tx, gap, count)
	if err != nil {
		return false, err
	}
	if !dominates {
		return false, ErrCheckpointConflict
	}
	return false, nil
}

func eventPersistenceGapCount(details string) (int64, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(details), &object) != nil || len(object) != 1 {
		return 0, false
	}
	raw, found := object["count"]
	if !found {
		return 0, false
	}
	var count int64
	if json.Unmarshal(raw, &count) != nil || count <= 0 {
		return 0, false
	}
	return count, true
}

func persistedGapDominates(ctx context.Context, tx *sql.Tx, gap CaptureGap, count int64) (bool, error) {
	var (
		id, kind, severity, reasonCode, details string
		mediaSegmentID                          sql.NullString
		startedAt, startOffset                  int64
		endedAt, endOffset                      sql.NullInt64
		recovered                               int
	)
	err := tx.QueryRowContext(ctx, `SELECT id, media_segment_id, kind, started_at,
		ended_at, start_offset_ms, end_offset_ms, severity, recovered,
		reason_code, details_json FROM capture_gaps
		WHERE session_id = ? AND dedupe_key = ?`, gap.SessionID, gap.DedupeKey).Scan(
		&id, &mediaSegmentID, &kind, &startedAt, &endedAt, &startOffset,
		&endOffset, &severity, &recovered, &reasonCode, &details,
	)
	if err != nil {
		return false, err
	}
	persistedCount, ok := eventPersistenceGapCount(details)
	if !ok || persistedCount < count || id != gap.ID || kind != gap.Kind ||
		mediaSegmentID.Valid != (gap.MediaSegmentID != "") ||
		(mediaSegmentID.Valid && mediaSegmentID.String != gap.MediaSegmentID) ||
		startedAt != gap.StartedAt.UTC().UnixMilli() || startOffset != gap.StartOffsetMS ||
		severity != gap.Severity || recovered != boolInt(gap.Recovered) || reasonCode != gap.ReasonCode {
		return false, nil
	}
	if gap.EndedAt != nil && (!endedAt.Valid || endedAt.Int64 < gap.EndedAt.UTC().UnixMilli()) {
		return false, nil
	}
	if gap.EndOffsetMS != nil && (!endOffset.Valid || endOffset.Int64 < *gap.EndOffsetMS) {
		return false, nil
	}
	return true, nil
}

const upsertCheckpointSQL = `INSERT INTO event_ingest_checkpoints(
	session_id, committed_sequence, state, privacy_key_id, spool_file, spool_offset,
	raw_file, raw_offset, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
	committed_sequence = excluded.committed_sequence,
	state = excluded.state,
	privacy_key_id = excluded.privacy_key_id,
	spool_file = excluded.spool_file,
	spool_offset = excluded.spool_offset,
	raw_file = excluded.raw_file,
	raw_offset = excluded.raw_offset,
	updated_at = excluded.updated_at
WHERE event_ingest_checkpoints.committed_sequence = ?
	AND event_ingest_checkpoints.privacy_key_id = excluded.privacy_key_id`

func persistCheckpoint(ctx context.Context, tx *sql.Tx, checkpoint Checkpoint, previous int64) (bool, error) {
	result, err := tx.ExecContext(ctx, upsertCheckpointSQL,
		checkpoint.SessionID, checkpoint.CommittedSequence,
		checkpoint.State, checkpoint.PrivacyKeyID,
		checkpoint.Spool.File, checkpoint.Spool.Offset,
		checkpoint.Raw.File, checkpoint.Raw.Offset,
		checkpoint.UpdatedAt.UTC().UnixMilli(), previous,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

type checkpointQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const selectCheckpointSQL = `SELECT committed_sequence, state, privacy_key_id, spool_file, spool_offset,
	raw_file, raw_offset, updated_at
FROM event_ingest_checkpoints WHERE session_id = ?`

func readCheckpoint(ctx context.Context, query checkpointQuerier, sessionID string) (Checkpoint, bool, error) {
	var result Checkpoint
	var updatedAt int64
	err := query.QueryRowContext(ctx, selectCheckpointSQL, sessionID).Scan(
		&result.CommittedSequence, &result.State, &result.PrivacyKeyID,
		&result.Spool.File, &result.Spool.Offset,
		&result.Raw.File, &result.Raw.Offset, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Checkpoint{}, false, nil
	}
	if err != nil {
		return Checkpoint{}, false, err
	}
	result.SessionID = sessionID
	result.UpdatedAt = unixMillis(updatedAt)
	return result, true, nil
}

func eventRole(ctx context.Context, tx *sql.Tx, sessionID, eventID string) (EventRole, error) {
	var role EventRole
	err := tx.QueryRowContext(ctx,
		`SELECT event_role FROM live_events WHERE session_id = ? AND id = ?`,
		sessionID, eventID,
	).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrPersistenceConstraint
	}
	return role, err
}

func validBatch(batch Batch) bool {
	if !validIdentifier(batch.SessionID) || batch.PreviousSequence < 0 ||
		batch.Checkpoint.SessionID != batch.SessionID ||
		batch.Checkpoint.CommittedSequence < batch.PreviousSequence ||
		!validCheckpointState(batch.Checkpoint.State) ||
		!validIdentifier(batch.Checkpoint.PrivacyKeyID) ||
		batch.Checkpoint.UpdatedAt.IsZero() {
		return false
	}
	target := batch.Checkpoint.CommittedSequence
	if target > 0 && (!validRelativeFile(batch.Checkpoint.Spool.File) ||
		batch.Checkpoint.Spool.Offset <= 0 || !validRelativeFile(batch.Checkpoint.Raw.File) ||
		batch.Checkpoint.Raw.Offset <= 0) {
		return false
	}
	if target == 0 && (batch.Checkpoint.Spool.File != "" || batch.Checkpoint.Spool.Offset != 0 ||
		batch.Checkpoint.Raw.File != "" || batch.Checkpoint.Raw.Offset != 0) {
		return false
	}

	lastSource := batch.PreviousSequence
	maxSource := int64(0)
	sourceSequences := make(map[int64]struct{}, len(batch.Events))
	for _, event := range batch.Events {
		if !validEvent(event, batch.SessionID, batch.PreviousSequence, target) {
			return false
		}
		if event.Role == EventRoleSource {
			if event.IngestSequence < lastSource {
				return false
			}
			lastSource = event.IngestSequence
			if _, duplicate := sourceSequences[event.IngestSequence]; duplicate {
				return false
			}
			sourceSequences[event.IngestSequence] = struct{}{}
			if event.IngestSequence > maxSource {
				maxSource = event.IngestSequence
			}
		}
	}
	if target > batch.PreviousSequence && maxSource != target {
		return false
	}
	for _, combo := range batch.GiftCombos {
		if !validCombo(combo, batch.SessionID, target) {
			return false
		}
	}
	for _, gap := range batch.Gaps {
		if !validGap(gap, batch.SessionID) {
			return false
		}
	}
	return true
}

func validEvent(event Event, sessionID string, previous, target int64) bool {
	if !validIdentifier(event.ID) || event.SessionID != sessionID ||
		event.IngestSequence <= 0 || event.IngestSequence > target ||
		(event.Role != EventRoleSource && event.Role != EventRoleAggregate) ||
		len(event.Method) > maxIdentifierLength || !validEventKind(event.Kind) ||
		!validIdentifier(event.DedupeKey) || event.ReceivedAt.IsZero() ||
		event.ClockConfidence < 0 || event.ClockConfidence > 1 ||
		!validParseStatus(event.ParseStatus) ||
		!validIdentifier(event.NormalizerVersion) {
		return false
	}
	if event.Role == EventRoleSource && event.IngestSequence <= previous {
		return false
	}
	if strings.TrimSpace(event.Method) == "" &&
		(event.ParseStatus != ParseFailed || event.ParseErrorCode != "EVENT_METHOD_MISSING") {
		return false
	}
	if event.ParseStatus == ParseParsed && event.ParseErrorCode != "" {
		return false
	}
	if event.ParseStatus == ParseFailed && !validStableCode(event.ParseErrorCode) {
		return false
	}
	if event.ParseErrorCode != "" && !validStableCode(event.ParseErrorCode) {
		return false
	}
	if !validJSONObject(defaultJSON(event.NormalizedJSON)) {
		return false
	}
	if event.Raw.File == "" {
		return event.Role == EventRoleAggregate && event.Raw.Offset == 0 &&
			event.Raw.Length == 0 && event.Raw.CRC32C == 0
	}
	return validRelativeFile(event.Raw.File) && event.Raw.Offset >= 0 &&
		event.Raw.Length >= RawFrameHeaderSize
}

func validCombo(combo GiftComboState, sessionID string, target int64) bool {
	if combo.SessionID != sessionID || !validIdentifier(combo.ComboKey) ||
		(combo.Status != ComboOpen && combo.Status != ComboClosed) ||
		!validIdentifier(combo.GiftID) || combo.TotalCount <= 0 ||
		combo.FirstSequence <= 0 || combo.LastSequence < combo.FirstSequence ||
		(combo.TotalValue != nil && *combo.TotalValue < 0) ||
		combo.LastSequence > target || combo.StartedAt.IsZero() || combo.UpdatedAt.IsZero() ||
		combo.UpdatedAt.Before(combo.StartedAt) || !validIdentifier(combo.NormalizerVersion) {
		return false
	}
	if combo.Status == ComboOpen {
		return combo.ClosedAt == nil && combo.AggregateEventID == ""
	}
	return combo.ClosedAt != nil && !combo.ClosedAt.Before(combo.UpdatedAt) &&
		validIdentifier(combo.AggregateEventID)
}

func validGap(gap CaptureGap, sessionID string) bool {
	if !validIdentifier(gap.ID) || gap.SessionID != sessionID ||
		!validGapKind(gap.Kind) || gap.StartedAt.IsZero() ||
		!validSeverity(gap.Severity) || !validStableCode(gap.ReasonCode) ||
		!validIdentifier(gap.DedupeKey) || !validJSONObject(defaultJSON(gap.DetailsJSON)) {
		return false
	}
	if gap.EndedAt != nil && gap.EndedAt.Before(gap.StartedAt) {
		return false
	}
	if gap.EndOffsetMS != nil && *gap.EndOffsetMS < gap.StartOffsetMS {
		return false
	}
	return gap.MediaSegmentID == "" || validIdentifier(gap.MediaSegmentID)
}

func validEventKind(kind EventKind) bool {
	switch kind {
	case EventChat, EventGift, EventLike, EventMember, EventFollow, EventSystem, EventUnknown:
		return true
	default:
		return false
	}
}

func validParseStatus(status ParseStatus) bool {
	return status == ParseParsed || status == ParseUnknown || status == ParseFailed
}

func validCheckpointState(state CheckpointState) bool {
	return state == CheckpointOpen || state == CheckpointClosing ||
		state == CheckpointClosed || state == CheckpointDegraded
}

func validCheckpointTransition(from, to CheckpointState) bool {
	switch from {
	case CheckpointOpen:
		return to == CheckpointOpen || to == CheckpointClosing || to == CheckpointDegraded
	case CheckpointClosing:
		return to == CheckpointClosing || to == CheckpointClosed || to == CheckpointDegraded
	case CheckpointDegraded:
		return validCheckpointState(to)
	case CheckpointClosed:
		return to == CheckpointClosed
	default:
		return false
	}
}

func validGapKind(kind string) bool {
	switch kind {
	case "message_disconnect", "recording_restart", "stream_unavailable", "disk_full",
		"process_crash", "clock_uncertain", "event_persistence":
		return true
	default:
		return false
	}
}

func validSeverity(severity string) bool {
	return severity == "info" || severity == "warning" || severity == "error"
}

func validIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maxIdentifierLength
}

func validStableCode(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validRelativeFile(value string) bool {
	if value == "" || len(value) > maxRelativeFile || strings.ContainsAny(value, `\\:`) ||
		strings.HasPrefix(value, "/") {
		return false
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, "../")
}

func validJSONObject(value string) bool {
	if len(value) > maxJSONLength || !json.Valid([]byte(value)) {
		return false
	}
	trimmed := strings.TrimSpace(value)
	return strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")
}

func defaultJSON(value string) string {
	if value == "" {
		return "{}"
	}
	return value
}

func collapseCombos(combos []GiftComboState) []GiftComboState {
	if len(combos) < 2 {
		return combos
	}
	result := make([]GiftComboState, 0, len(combos))
	positions := make(map[string]int, len(combos))
	for _, combo := range combos {
		key := combo.SessionID + "\x00" + combo.ComboKey
		if index, found := positions[key]; found {
			result[index] = combo
			continue
		}
		positions[key] = len(result)
		result = append(result, combo)
	}
	return result
}

func sameCheckpointPosition(left, right Checkpoint) bool {
	return left.SessionID == right.SessionID &&
		left.CommittedSequence == right.CommittedSequence &&
		left.State == right.State && left.PrivacyKeyID == right.PrivacyKeyID &&
		left.Spool == right.Spool && left.Raw == right.Raw
}

func classifyPersistenceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCheckpointConflict) || errors.Is(err, ErrPersistenceConstraint) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrPersistenceCommit, err)
	}
	var sqliteErr *modernsqlite.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code() & 0xff {
		case 5, 6:
			return ErrPersistenceBusy
		case 10, 11, 26:
			return ErrPersistenceCorrupt
		case 13:
			return ErrPersistenceFull
		case 19:
			return ErrPersistenceConstraint
		}
	}
	return ErrPersistenceCommit
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func timeMillisPtr(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().UnixMilli()
}

func unixMillis(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}

func nullableStringEqual(value sql.NullString, expected string) bool {
	return value.Valid == (expected != "") && (!value.Valid || value.String == expected)
}

func nullableFloatEqual(value sql.NullFloat64, expected *float64) bool {
	return value.Valid == (expected != nil) && (!value.Valid || value.Float64 == *expected)
}

func nullableTimeEqual(value sql.NullInt64, expected *time.Time) bool {
	return value.Valid == (expected != nil) && (!value.Valid || value.Int64 == expected.UTC().UnixMilli())
}
