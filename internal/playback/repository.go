package playback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/google/uuid"
)

const (
	cursorKindSessions = "sessions"
	cursorKindEvents   = "events"
	cursorKindGaps     = "gaps"
	maxCursorLength    = 2048
)

var validSessionStatuses = map[string]struct{}{
	"starting": {}, "recording": {}, "finalizing": {}, "completed": {},
	"interrupted": {}, "failed": {},
}

var validEventKinds = map[string]struct{}{
	"chat": {}, "gift": {}, "like": {}, "member": {}, "follow": {},
	"system": {}, "unknown": {},
}

var validEventRoles = map[string]struct{}{
	"source": {}, "aggregate": {},
}

var validGapKinds = map[string]struct{}{
	"message_disconnect": {}, "recording_restart": {}, "stream_unavailable": {},
	"disk_full": {}, "process_crash": {}, "clock_uncertain": {},
	"event_persistence": {},
}

type Repository struct {
	reader *sql.DB
}

type cursorEnvelope struct {
	Version int    `json:"v"`
	Kind    string `json:"k"`
	Filter  string `json:"f"`
	Primary int64  `json:"p"`
	ID      string `json:"i"`
}

type normalizedSessionFilter struct {
	RoomConfigID string   `json:"roomConfigId,omitempty"`
	Statuses     []string `json:"statuses,omitempty"`
	StartedAtMin *int64   `json:"startedAtMin,omitempty"`
	StartedAtMax *int64   `json:"startedAtMax,omitempty"`
}

type normalizedEventFilter struct {
	SessionID string   `json:"sessionId"`
	Kinds     []string `json:"kinds,omitempty"`
	Roles     []string `json:"roles,omitempty"`
	OffsetMin *int64   `json:"offsetMin,omitempty"`
	OffsetMax *int64   `json:"offsetMax,omitempty"`
}

type normalizedGapFilter struct {
	SessionID string   `json:"sessionId"`
	Kinds     []string `json:"kinds,omitempty"`
	Recovered *bool    `json:"recovered,omitempty"`
	OffsetMin *int64   `json:"offsetMin,omitempty"`
	OffsetMax *int64   `json:"offsetMax,omitempty"`
}

func NewRepository(reader *sql.DB) (*Repository, error) {
	if reader == nil {
		return nil, errors.New("playback repository requires a reader database")
	}
	return &Repository{reader: reader}, nil
}

func (r *Repository) GetSession(ctx context.Context, sessionID string) (SessionResult, error) {
	if ctx == nil {
		return SessionResult{}, fmt.Errorf("%w: context", ErrInvalidArgument)
	}
	if !isUUIDv7(sessionID) {
		return SessionResult{}, fmt.Errorf("%w: session id", ErrInvalidArgument)
	}
	row := r.reader.QueryRowContext(ctx, sessionSelectSQL+` WHERE s.id = ?`, sessionID)
	item, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionResult{}, ErrSessionNotFound
	}
	if err != nil {
		return SessionResult{}, fmt.Errorf("get playback session: %w", err)
	}
	return SessionResult{Version: ContractVersion, Session: item}, nil
}

func (r *Repository) ListSessions(
	ctx context.Context,
	filter SessionFilter,
	page PageRequest,
) (SessionPage, error) {
	if ctx == nil {
		return SessionPage{}, fmt.Errorf("%w: context", ErrInvalidArgument)
	}
	normalized, err := normalizeSessionFilter(filter)
	if err != nil {
		return SessionPage{}, err
	}
	limit, err := normalizeLimit(page.Limit)
	if err != nil {
		return SessionPage{}, err
	}
	digest, err := filterDigest(cursorKindSessions, normalized)
	if err != nil {
		return SessionPage{}, err
	}
	cursor, hasCursor, err := decodeCursor(page.Cursor, cursorKindSessions, digest)
	if err != nil {
		return SessionPage{}, err
	}

	query := sessionSelectSQL + ` WHERE 1 = 1`
	args := make([]any, 0, 10)
	if normalized.RoomConfigID != "" {
		query += ` AND s.room_config_id = ?`
		args = append(args, normalized.RoomConfigID)
	}
	query, args = appendStringSet(query, args, "s.status", normalized.Statuses)
	if normalized.StartedAtMin != nil {
		query += ` AND s.started_at >= ?`
		args = append(args, *normalized.StartedAtMin)
	}
	if normalized.StartedAtMax != nil {
		query += ` AND s.started_at <= ?`
		args = append(args, *normalized.StartedAtMax)
	}
	if hasCursor {
		query += ` AND (s.started_at < ? OR (s.started_at = ? AND s.id < ?))`
		args = append(args, cursor.Primary, cursor.Primary, cursor.ID)
	}
	query += ` ORDER BY s.started_at DESC, s.id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := r.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return SessionPage{}, fmt.Errorf("list playback sessions: %w", err)
	}
	defer rows.Close()
	items := make([]SessionDTO, 0, limit+1)
	for rows.Next() {
		item, err := scanSession(rows)
		if err != nil {
			return SessionPage{}, fmt.Errorf("scan playback session: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return SessionPage{}, fmt.Errorf("iterate playback sessions: %w", err)
	}
	result := SessionPage{Version: ContractVersion, Items: items}
	if len(items) > limit {
		last := items[limit-1]
		result.Items = items[:limit]
		result.NextCursor, err = encodeCursor(cursorKindSessions, digest, last.StartedAt, last.ID)
		if err != nil {
			return SessionPage{}, err
		}
	}
	return result, nil
}

func (r *Repository) ListEvents(
	ctx context.Context,
	filter EventFilter,
	page PageRequest,
) (EventPage, error) {
	if ctx == nil {
		return EventPage{}, fmt.Errorf("%w: context", ErrInvalidArgument)
	}
	normalized, err := normalizeEventFilter(filter)
	if err != nil {
		return EventPage{}, err
	}
	limit, err := normalizeLimit(page.Limit)
	if err != nil {
		return EventPage{}, err
	}
	digest, err := filterDigest(cursorKindEvents, normalized)
	if err != nil {
		return EventPage{}, err
	}
	cursor, hasCursor, err := decodeCursor(page.Cursor, cursorKindEvents, digest)
	if err != nil {
		return EventPage{}, err
	}
	query := `SELECT id, ingest_sequence, event_role, kind, received_at,
		session_offset_ms, clock_confidence, display_name, content,
		numeric_value, parse_status
		FROM live_events WHERE session_id = ?`
	args := []any{normalized.SessionID}
	query, args = appendStringSet(query, args, "kind", normalized.Kinds)
	query, args = appendStringSet(query, args, "event_role", normalized.Roles)
	if normalized.OffsetMin != nil {
		query += ` AND session_offset_ms >= ?`
		args = append(args, *normalized.OffsetMin)
	}
	if normalized.OffsetMax != nil {
		query += ` AND session_offset_ms <= ?`
		args = append(args, *normalized.OffsetMax)
	}
	if hasCursor {
		query += ` AND (session_offset_ms > ? OR (session_offset_ms = ? AND id > ?))`
		args = append(args, cursor.Primary, cursor.Primary, cursor.ID)
	}
	query += ` ORDER BY session_offset_ms, id LIMIT ?`
	args = append(args, limit+1)
	rows, err := r.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return EventPage{}, fmt.Errorf("list playback events: %w", err)
	}
	defer rows.Close()
	items := make([]EventDTO, 0, limit+1)
	for rows.Next() {
		item, err := scanEvent(rows)
		if err != nil {
			return EventPage{}, fmt.Errorf("scan playback event: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return EventPage{}, fmt.Errorf("iterate playback events: %w", err)
	}
	result := EventPage{Version: ContractVersion, Items: items}
	if len(items) > limit {
		last := items[limit-1]
		result.Items = items[:limit]
		result.NextCursor, err = encodeCursor(cursorKindEvents, digest, last.SessionOffsetMS, last.ID)
		if err != nil {
			return EventPage{}, err
		}
	}
	return result, nil
}

func (r *Repository) ListGaps(
	ctx context.Context,
	filter GapFilter,
	page PageRequest,
) (GapPage, error) {
	if ctx == nil {
		return GapPage{}, fmt.Errorf("%w: context", ErrInvalidArgument)
	}
	normalized, err := normalizeGapFilter(filter)
	if err != nil {
		return GapPage{}, err
	}
	limit, err := normalizeLimit(page.Limit)
	if err != nil {
		return GapPage{}, err
	}
	digest, err := filterDigest(cursorKindGaps, normalized)
	if err != nil {
		return GapPage{}, err
	}
	cursor, hasCursor, err := decodeCursor(page.Cursor, cursorKindGaps, digest)
	if err != nil {
		return GapPage{}, err
	}
	query := `SELECT id, kind, started_at, ended_at, start_offset_ms,
		end_offset_ms, severity, recovered, reason_code
		FROM capture_gaps WHERE session_id = ?`
	args := []any{normalized.SessionID}
	query, args = appendStringSet(query, args, "kind", normalized.Kinds)
	if normalized.Recovered != nil {
		query += ` AND recovered = ?`
		args = append(args, boolInt(*normalized.Recovered))
	}
	if normalized.OffsetMin != nil {
		query += ` AND start_offset_ms >= ?`
		args = append(args, *normalized.OffsetMin)
	}
	if normalized.OffsetMax != nil {
		query += ` AND start_offset_ms <= ?`
		args = append(args, *normalized.OffsetMax)
	}
	if hasCursor {
		query += ` AND (start_offset_ms > ? OR (start_offset_ms = ? AND id > ?))`
		args = append(args, cursor.Primary, cursor.Primary, cursor.ID)
	}
	query += ` ORDER BY start_offset_ms, id LIMIT ?`
	args = append(args, limit+1)
	rows, err := r.reader.QueryContext(ctx, query, args...)
	if err != nil {
		return GapPage{}, fmt.Errorf("list playback gaps: %w", err)
	}
	defer rows.Close()
	items := make([]GapDTO, 0, limit+1)
	for rows.Next() {
		item, err := scanGap(rows)
		if err != nil {
			return GapPage{}, fmt.Errorf("scan playback gap: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return GapPage{}, fmt.Errorf("iterate playback gaps: %w", err)
	}
	result := GapPage{Version: ContractVersion, Items: items}
	if len(items) > limit {
		last := items[limit-1]
		result.Items = items[:limit]
		result.NextCursor, err = encodeCursor(cursorKindGaps, digest, last.StartOffsetMS, last.ID)
		if err != nil {
			return GapPage{}, err
		}
	}
	return result, nil
}

const sessionSelectSQL = `SELECT s.id, s.room_config_id, r.alias, s.title,
	s.status, s.recording_status, s.started_at, s.ended_at, s.media_epoch_at,
	s.capture_offset_ms, s.clock_source, s.integrity_score, COALESCE(sm.state, '')
	FROM live_sessions s
	JOIN rooms r ON r.id = s.room_config_id
	LEFT JOIN session_media sm ON sm.session_id = s.id`

type scanner interface {
	Scan(dest ...any) error
}

func scanSession(source scanner) (SessionDTO, error) {
	var item SessionDTO
	var endedAt, mediaEpochAt sql.NullInt64
	if err := source.Scan(
		&item.ID, &item.RoomConfigID, &item.RoomAlias, &item.Title,
		&item.Status, &item.RecordingStatus, &item.StartedAt, &endedAt,
		&mediaEpochAt, &item.CaptureOffsetMS, &item.ClockSource,
		&item.IntegrityScore, &item.SessionMediaState,
	); err != nil {
		return SessionDTO{}, err
	}
	item.EndedAt = nullableInt64(endedAt)
	item.MediaEpochAt = nullableInt64(mediaEpochAt)
	return item, nil
}

func scanEvent(source scanner) (EventDTO, error) {
	var item EventDTO
	var displayName, content sql.NullString
	var numericValue sql.NullFloat64
	if err := source.Scan(
		&item.ID, &item.IngestSequence, &item.Role, &item.Kind,
		&item.ReceivedAt, &item.SessionOffsetMS, &item.ClockConfidence,
		&displayName, &content, &numericValue, &item.ParseStatus,
	); err != nil {
		return EventDTO{}, err
	}
	if displayName.Valid {
		item.DisplayName = displayName.String
	}
	if content.Valid {
		item.Content = content.String
	}
	if numericValue.Valid {
		value := numericValue.Float64
		item.NumericValue = &value
	}
	return item, nil
}

func scanGap(source scanner) (GapDTO, error) {
	var item GapDTO
	var endedAt, endOffset sql.NullInt64
	var recovered int
	if err := source.Scan(
		&item.ID, &item.Kind, &item.StartedAt, &endedAt,
		&item.StartOffsetMS, &endOffset, &item.Severity,
		&recovered, &item.ReasonCode,
	); err != nil {
		return GapDTO{}, err
	}
	if recovered != 0 && recovered != 1 {
		return GapDTO{}, errors.New("invalid recovered value")
	}
	item.Recovered = recovered == 1
	item.EndedAt = nullableInt64(endedAt)
	item.EndOffsetMS = nullableInt64(endOffset)
	return item, nil
}

func normalizeSessionFilter(filter SessionFilter) (normalizedSessionFilter, error) {
	if filter.RoomConfigID != "" && !isUUIDv7(filter.RoomConfigID) {
		return normalizedSessionFilter{}, fmt.Errorf("%w: room config id", ErrInvalidArgument)
	}
	statuses, err := normalizeSet(filter.Statuses, validSessionStatuses, "session status")
	if err != nil {
		return normalizedSessionFilter{}, err
	}
	if invalidRange(filter.StartedAtMin, filter.StartedAtMax) {
		return normalizedSessionFilter{}, fmt.Errorf("%w: session time range", ErrInvalidArgument)
	}
	return normalizedSessionFilter{
		RoomConfigID: filter.RoomConfigID, Statuses: statuses,
		StartedAtMin: cloneInt64(filter.StartedAtMin), StartedAtMax: cloneInt64(filter.StartedAtMax),
	}, nil
}

func normalizeEventFilter(filter EventFilter) (normalizedEventFilter, error) {
	if !isUUIDv7(filter.SessionID) {
		return normalizedEventFilter{}, fmt.Errorf("%w: session id", ErrInvalidArgument)
	}
	kinds, err := normalizeSet(filter.Kinds, validEventKinds, "event kind")
	if err != nil {
		return normalizedEventFilter{}, err
	}
	roles, err := normalizeSet(filter.Roles, validEventRoles, "event role")
	if err != nil {
		return normalizedEventFilter{}, err
	}
	if invalidRange(filter.OffsetMin, filter.OffsetMax) {
		return normalizedEventFilter{}, fmt.Errorf("%w: event offset range", ErrInvalidArgument)
	}
	return normalizedEventFilter{
		SessionID: filter.SessionID, Kinds: kinds, Roles: roles,
		OffsetMin: cloneInt64(filter.OffsetMin), OffsetMax: cloneInt64(filter.OffsetMax),
	}, nil
}

func normalizeGapFilter(filter GapFilter) (normalizedGapFilter, error) {
	if !isUUIDv7(filter.SessionID) {
		return normalizedGapFilter{}, fmt.Errorf("%w: session id", ErrInvalidArgument)
	}
	kinds, err := normalizeSet(filter.Kinds, validGapKinds, "gap kind")
	if err != nil {
		return normalizedGapFilter{}, err
	}
	if invalidRange(filter.OffsetMin, filter.OffsetMax) {
		return normalizedGapFilter{}, fmt.Errorf("%w: gap offset range", ErrInvalidArgument)
	}
	return normalizedGapFilter{
		SessionID: filter.SessionID, Kinds: kinds, Recovered: cloneBool(filter.Recovered),
		OffsetMin: cloneInt64(filter.OffsetMin), OffsetMax: cloneInt64(filter.OffsetMax),
	}, nil
}

func normalizeSet(values []string, allowed map[string]struct{}, field string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	for index, value := range result {
		if _, ok := allowed[value]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, field)
		}
		if index > 0 && result[index-1] == value {
			return nil, fmt.Errorf("%w: duplicate %s", ErrInvalidArgument, field)
		}
	}
	return result, nil
}

func normalizeLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultPageSize, nil
	}
	if limit < 1 || limit > MaxPageSize {
		return 0, fmt.Errorf("%w: page limit", ErrInvalidArgument)
	}
	return limit, nil
}

func appendStringSet(query string, args []any, column string, values []string) (string, []any) {
	if len(values) == 0 {
		return query, args
	}
	placeholders := make([]string, len(values))
	for index, value := range values {
		placeholders[index] = "?"
		args = append(args, value)
	}
	return query + ` AND ` + column + ` IN (` + strings.Join(placeholders, ",") + `)`, args
}

func filterDigest(kind string, filter any) (string, error) {
	encoded, err := json.Marshal(filter)
	if err != nil {
		return "", fmt.Errorf("encode playback filter: %w", err)
	}
	digest := sha256.Sum256(append([]byte(kind+":"), encoded...))
	return hex.EncodeToString(digest[:]), nil
}

func encodeCursor(kind, digest string, primary int64, id string) (string, error) {
	payload, err := json.Marshal(cursorEnvelope{
		Version: CursorVersion, Kind: kind, Filter: digest, Primary: primary, ID: id,
	})
	if err != nil {
		return "", fmt.Errorf("encode playback cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCursor(raw, kind, digest string) (cursorEnvelope, bool, error) {
	if raw == "" {
		return cursorEnvelope{}, false, nil
	}
	if len(raw) > maxCursorLength || strings.TrimSpace(raw) != raw {
		return cursorEnvelope{}, false, ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(payload) == 0 {
		return cursorEnvelope{}, false, ErrInvalidCursor
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cursor cursorEnvelope
	if err := decoder.Decode(&cursor); err != nil {
		return cursorEnvelope{}, false, ErrInvalidCursor
	}
	if err := requireJSONEOF(decoder); err != nil {
		return cursorEnvelope{}, false, ErrInvalidCursor
	}
	if cursor.Version != CursorVersion || cursor.Kind != kind ||
		cursor.Filter != digest || !isUUIDv7(cursor.ID) {
		return cursorEnvelope{}, false, ErrInvalidCursor
	}
	return cursor, true, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func isUUIDv7(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.ToLower(value) != value {
		return false
	}
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.Version() == 7 && parsed.String() == value
}

func invalidRange(minimum, maximum *int64) bool {
	return minimum != nil && maximum != nil && *minimum > *maximum
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	copyValue := value.Int64
	return &copyValue
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
