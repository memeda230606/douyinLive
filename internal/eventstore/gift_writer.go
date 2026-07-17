package eventstore

import (
	"context"
	"database/sql"
	"strings"
)

type GiftFoldSnapshot struct {
	State       GiftComboState
	LastSource  Event
	UnitDiamond int64
}

func (w *Writer) GiftFolds(
	ctx context.Context,
	sessionID string,
	keys []string,
) (map[string]GiftFoldSnapshot, error) {
	if w == nil || w.db == nil {
		return nil, ErrWriterUnavailable
	}
	if ctx == nil || !validIdentifier(sessionID) {
		return nil, ErrInvalidBatch
	}
	keys = uniqueValidIdentifiers(keys)
	if len(keys) == 0 {
		return map[string]GiftFoldSnapshot{}, nil
	}
	query, args := giftFoldQuery(sessionID, keys, "", 0)
	rows, err := w.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifyPersistenceError(err)
	}
	return scanGiftFolds(rows, sessionID)
}

func (w *Writer) OpenGiftFolds(
	ctx context.Context,
	sessionID string,
	cutoffMillis *int64,
	limit int,
) ([]GiftFoldSnapshot, error) {
	if w == nil || w.db == nil {
		return nil, ErrWriterUnavailable
	}
	if ctx == nil || !validIdentifier(sessionID) || limit <= 0 {
		return nil, ErrInvalidBatch
	}
	filter := "g.status = 'open'"
	args := []any{sessionID}
	if cutoffMillis != nil {
		filter += " AND g.updated_at <= ?"
		args = append(args, *cutoffMillis)
	}
	query := giftFoldSelect + " WHERE g.session_id = ? AND " + filter +
		" ORDER BY g.updated_at ASC, g.combo_key ASC LIMIT ?"
	args = append(args, limit)
	rows, err := w.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, classifyPersistenceError(err)
	}
	values, err := scanGiftFolds(rows, sessionID)
	if err != nil {
		return nil, err
	}
	result := make([]GiftFoldSnapshot, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	sortGiftFolds(result)
	return result, nil
}

func (w *Writer) ExistingSourceDedupeKeys(
	ctx context.Context,
	sessionID string,
	keys []string,
) (map[string]struct{}, error) {
	if w == nil || w.db == nil {
		return nil, ErrWriterUnavailable
	}
	if ctx == nil || !validIdentifier(sessionID) {
		return nil, ErrInvalidBatch
	}
	keys = uniqueValidIdentifiers(keys)
	if len(keys) == 0 {
		return map[string]struct{}{}, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys)+1)
	args = append(args, sessionID)
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := w.db.QueryContext(ctx, `SELECT dedupe_key FROM live_events
		WHERE session_id = ? AND event_role = 'source' AND dedupe_key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, classifyPersistenceError(err)
	}
	defer rows.Close()
	result := make(map[string]struct{}, len(keys))
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, classifyPersistenceError(err)
		}
		result[key] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, classifyPersistenceError(err)
	}
	return result, nil
}

const giftFoldSelect = `SELECT
	g.combo_key, g.status, g.user_hash, g.gift_id, g.gift_name,
	g.total_count, g.total_value, g.first_ingest_sequence,
	g.last_ingest_sequence, g.started_at, g.updated_at, g.closed_at,
	g.aggregate_event_id, g.normalizer_version,
	e.id, e.method, e.received_at, e.session_offset_ms,
	e.clock_confidence, e.display_name, e.content, e.parse_status,
	e.normalizer_version
	FROM gift_combo_states g
	LEFT JOIN live_events e ON e.session_id = g.session_id
		AND e.ingest_sequence = g.last_ingest_sequence
		AND e.event_role = 'source'`

func giftFoldQuery(sessionID string, keys []string, extra string, limit int) (string, []any) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	query := giftFoldSelect + " WHERE g.session_id = ? AND g.combo_key IN (" + placeholders + ")"
	args := make([]any, 0, len(keys)+2)
	args = append(args, sessionID)
	for _, key := range keys {
		args = append(args, key)
	}
	if extra != "" {
		query += " AND " + extra
	}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	return query, args
}

func scanGiftFolds(rows *sql.Rows, sessionID string) (map[string]GiftFoldSnapshot, error) {
	defer rows.Close()
	result := make(map[string]GiftFoldSnapshot)
	for rows.Next() {
		var (
			comboKey, status, giftID, normalizerVersion   string
			userHash, giftName, aggregateID               sql.NullString
			totalCount, first, last, startedAt, updatedAt int64
			totalValue                                    sql.NullFloat64
			closedAt                                      sql.NullInt64
			eventID, method, eventNormalizer              sql.NullString
			receivedAt, sessionOffset                     sql.NullInt64
			clockConfidence                               sql.NullFloat64
			displayName, content, parseStatus             sql.NullString
		)
		if err := rows.Scan(
			&comboKey, &status, &userHash, &giftID, &giftName,
			&totalCount, &totalValue, &first, &last, &startedAt, &updatedAt,
			&closedAt, &aggregateID, &normalizerVersion,
			&eventID, &method, &receivedAt, &sessionOffset,
			&clockConfidence, &displayName, &content, &parseStatus,
			&eventNormalizer,
		); err != nil {
			return nil, classifyPersistenceError(err)
		}
		state := GiftComboState{
			SessionID: sessionID, ComboKey: comboKey, Status: ComboStatus(status),
			UserHash: userHash.String, GiftID: giftID, GiftName: giftName.String,
			TotalCount: totalCount, FirstSequence: first, LastSequence: last,
			StartedAt: unixMillis(startedAt), UpdatedAt: unixMillis(updatedAt),
			AggregateEventID: aggregateID.String, NormalizerVersion: normalizerVersion,
		}
		if totalValue.Valid {
			value := totalValue.Float64
			state.TotalValue = &value
		}
		if closedAt.Valid {
			value := unixMillis(closedAt.Int64)
			state.ClosedAt = &value
		}
		if !validCombo(state, sessionID, state.LastSequence) {
			return nil, ErrPersistenceCorrupt
		}
		source := Event{
			ID: eventID.String, SessionID: sessionID, IngestSequence: state.LastSequence,
			Role: EventRoleSource, Method: method.String, Kind: EventGift,
			ReceivedAt: unixMillis(receivedAt.Int64), SessionOffsetMS: sessionOffset.Int64,
			ClockConfidence: clockConfidence.Float64, DisplayName: displayName.String,
			Content: content.String, ParseStatus: ParseStatus(parseStatus.String),
			NormalizerVersion: eventNormalizer.String,
		}
		if state.Status == ComboOpen && (!eventID.Valid || !receivedAt.Valid) {
			return nil, ErrPersistenceCorrupt
		}
		unit := int64(0)
		if state.TotalValue != nil && state.TotalCount > 0 {
			unit = int64(*state.TotalValue / float64(state.TotalCount))
		}
		result[comboKey] = GiftFoldSnapshot{State: state, LastSource: source, UnitDiamond: unit}
	}
	if err := rows.Err(); err != nil {
		return nil, classifyPersistenceError(err)
	}
	return result, nil
}

func uniqueValidIdentifiers(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !validIdentifier(value) {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func sortGiftFolds(values []GiftFoldSnapshot) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0; j-- {
			left := values[j-1].State
			right := values[j].State
			if left.UpdatedAt.Before(right.UpdatedAt) ||
				(left.UpdatedAt.Equal(right.UpdatedAt) && left.ComboKey <= right.ComboKey) {
				break
			}
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
}
