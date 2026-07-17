package capture

import (
	"context"

	"database/sql"

	"encoding/json"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"

	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const sessionSelectSQL = `SELECT id, room_config_id, operation_id, platform_room_id, title,
	status, recording_status, manifest_dirty, started_at, ended_at, media_epoch_at, capture_offset_ms,
	clock_source, integrity_score, data_path, schema_version, created_at, updated_at
	FROM live_sessions`

type SQLiteRepository struct {
	writer                    *sql.DB
	reader                    *sql.DB
	dataRoot                  string
	now                       func() time.Time
	outcomeTimeout            time.Duration
	promoteManifest           func(*stagedManifest) error
	manifestIssueMu           sync.RWMutex
	manifestIssues            map[string]manifestIssueState
	manifestReporter          ManifestHealthReporter
	updateManifestDirty       func(context.Context, LiveSession, bool) error
	beforeManifestMarkerClear func() error
	sessionLockMu             sync.Mutex
	sessionLocks              map[string]*manifestSessionLock
}

const (
	defaultOutcomeResolutionTimeout = 250 * time.Millisecond
	manifestRepairPageSize          = 128
)

type manifestIssueState struct {
	requiredAcknowledged bool
}

type manifestSessionLock struct {
	mu   sync.Mutex
	refs int
}

func NewSQLiteRepository(writer, reader *sql.DB, dataRoot string) (*SQLiteRepository, error) {
	return NewSQLiteRepositoryWithOptions(writer, reader, dataRoot, SQLiteRepositoryOptions{})
}

func NewSQLiteRepositoryWithOptions(writer, reader *sql.DB, dataRoot string, options SQLiteRepositoryOptions) (*SQLiteRepository, error) {
	repository, err := newSQLiteRepository(writer, reader, dataRoot, time.Now)
	if err != nil {
		return nil, err
	}
	repository.manifestReporter = options.ManifestHealthReporter
	return repository, nil
}

func newSQLiteRepository(writer, reader *sql.DB, dataRoot string, now func() time.Time) (*SQLiteRepository, error) {
	if writer == nil || reader == nil {
		return nil, errors.New("capture repository requires reader and writer databases")
	}
	if strings.TrimSpace(dataRoot) == "" {
		return nil, errors.New("capture repository data root is empty")
	}
	absoluteRoot, err := filepath.Abs(filepath.Clean(dataRoot))
	if err != nil {
		return nil, fmt.Errorf("resolve capture repository data root: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	repository := &SQLiteRepository{
		writer: writer, reader: reader, dataRoot: absoluteRoot, now: now,
		outcomeTimeout:  defaultOutcomeResolutionTimeout,
		promoteManifest: func(staged *stagedManifest) error { return staged.promote() },
		manifestIssues:  make(map[string]manifestIssueState),
		sessionLocks:    make(map[string]*manifestSessionLock),
	}
	repository.updateManifestDirty = repository.persistManifestDirty
	return repository, nil
}

func (r *SQLiteRepository) Create(ctx context.Context, input CreateSessionInput) (LiveSession, error) {
	if err := requireContext(ctx); err != nil {
		return LiveSession{}, err
	}
	if err := validateUUIDv7("room config id", input.RoomConfigID); err != nil {
		return LiveSession{}, err
	}
	if err := validateUUIDv7("operation id", input.OperationID); err != nil {
		return LiveSession{}, err
	}
	if input.Recording == "" {
		input.Recording = RecordingPending
	}
	if !validRecordingStatus(input.Recording) {
		return LiveSession{}, fmt.Errorf("invalid recording status %q", input.Recording)
	}

	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return LiveSession{}, fmt.Errorf("begin live session creation: %w", err)
	}
	defer tx.Rollback()

	existing, err := querySession(ctx, tx, sessionSelectSQL+` WHERE operation_id = ?`, input.OperationID)
	if err == nil {
		if existing.RoomConfigID != input.RoomConfigID {
			return LiveSession{}, ErrOperationConflict
		}
		_ = tx.Rollback()
		return r.materializeCommitted(ctx, existing), nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, fmt.Errorf("find idempotent live session operation: %w", err)
	}
	if active, found, err := activeForRoom(ctx, tx, input.RoomConfigID); err != nil {
		return LiveSession{}, err
	} else if found {
		if active.OperationID == input.OperationID {
			_ = tx.Rollback()
			return r.materializeCommitted(ctx, active), nil
		}
		return LiveSession{}, ErrActiveSessionExists
	}

	id, err := uuid.NewV7()
	if err != nil {
		return LiveSession{}, fmt.Errorf("generate live session id: %w", err)
	}
	startedAt := input.StartedAt
	if startedAt.IsZero() {
		startedAt = r.now()
	}
	startedAt = startedAt.UTC()
	nowMillis := r.now().UTC().UnixMilli()
	session := LiveSession{
		ID: id.String(), RoomConfigID: input.RoomConfigID, OperationID: input.OperationID,
		PlatformRoomID: strings.TrimSpace(input.PlatformRoomID), Title: strings.TrimSpace(input.Title),
		Status: SessionStarting, RecordingStatus: input.Recording, ManifestDirty: true,
		StartedAt: startedAt.UnixMilli(), ClockSource: ClockReceived, IntegrityScore: 1,
		DataPath:      sessionDataPath(input.RoomConfigID, startedAt, id.String()),
		SchemaVersion: SessionManifestSchemaVersion, CreatedAt: nowMillis, UpdatedAt: nowMillis,
	}
	staged, err := r.stageManifest(session)
	if err != nil {
		return LiveSession{}, err
	}
	defer staged.discard()

	_, err = tx.ExecContext(ctx, `INSERT INTO live_sessions(
		id, room_config_id, operation_id, platform_room_id, title, status, recording_status, manifest_dirty,
		started_at, ended_at, media_epoch_at, capture_offset_ms, clock_source, integrity_score,
		data_path, schema_version, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.RoomConfigID, session.OperationID, nullableText(session.PlatformRoomID), session.Title,
		session.Status, session.RecordingStatus, session.StartedAt, nil, nil, session.CaptureOffsetMS,
		session.ClockSource, session.IntegrityScore, session.DataPath, session.SchemaVersion,
		session.CreatedAt, session.UpdatedAt,
	)
	if err != nil {
		return LiveSession{}, classifyCreateError(err)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		if resolved, resolveErr := r.resolveCreateOutcome(input); resolveErr == nil {
			return r.promoteCommitted(ctx, staged, resolved), nil
		}
		return LiveSession{}, fmt.Errorf("commit live session creation: %w", err)
	}
	return r.promoteCommitted(ctx, staged, session), nil
}

func (r *SQLiteRepository) Get(ctx context.Context, id string) (LiveSession, error) {
	if err := requireContext(ctx); err != nil {
		return LiveSession{}, err
	}
	if err := validateUUIDv7("session id", id); err != nil {
		return LiveSession{}, ErrSessionNotFound
	}
	session, err := querySession(ctx, r.reader, sessionSelectSQL+` WHERE id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, ErrSessionNotFound
	}
	if err != nil {
		return LiveSession{}, fmt.Errorf("get live session: %w", err)
	}
	return r.materialize(ctx, session)
}

func (r *SQLiteRepository) ActiveForRoom(ctx context.Context, roomConfigID string) (LiveSession, bool, error) {
	if err := requireContext(ctx); err != nil {
		return LiveSession{}, false, err
	}
	if err := validateUUIDv7("room config id", roomConfigID); err != nil {
		return LiveSession{}, false, err
	}
	session, found, err := activeForRoom(ctx, r.reader, roomConfigID)
	if err != nil || !found {
		return session, found, err
	}
	session, err = r.materialize(ctx, session)
	if err != nil {
		return session, true, err
	}
	return session, true, nil
}

func (r *SQLiteRepository) ListRecoverable(ctx context.Context) ([]LiveSession, error) {
	if err := requireContext(ctx); err != nil {
		return nil, err
	}
	rows, err := r.reader.QueryContext(ctx, sessionSelectSQL+
		` WHERE status IN ('starting', 'recording', 'finalizing') ORDER BY started_at ASC, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list recoverable live sessions: %w", err)
	}
	sessions := make([]LiveSession, 0)
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan recoverable live session: %w", err)
		}
		sessions = append(sessions, session)
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil || closeErr != nil {
		return nil, fmt.Errorf("iterate recoverable live sessions: %w", errors.Join(rowsErr, closeErr))
	}
	result := make([]LiveSession, 0, len(sessions))
	for _, session := range sessions {
		session, err = r.materialize(ctx, session)
		if err != nil {
			return nil, err
		}
		result = append(result, session)
	}
	return result, nil
}
func (r *SQLiteRepository) Transition(ctx context.Context, input TransitionSessionInput) (LiveSession, error) {
	if err := requireContext(ctx); err != nil {
		return LiveSession{}, err
	}
	if err := validateUUIDv7("session id", input.ID); err != nil {
		return LiveSession{}, ErrSessionNotFound
	}
	if !validSessionStatus(input.ExpectedStatus) || !validSessionStatus(input.Status) {
		return LiveSession{}, errors.New("transition contains invalid session status")
	}
	if !validRecordingStatus(input.ExpectedRecordingStatus) || !validRecordingStatus(input.RecordingStatus) {
		return LiveSession{}, errors.New("transition contains invalid recording status")
	}
	if input.ExpectedOperationID == "" {
		if input.NextOperationID == "" {
			return LiveSession{}, errors.New("claiming an empty operation id requires a next operation id")
		}
	} else if err := validateUUIDv7("expected operation id", input.ExpectedOperationID); err != nil {
		return LiveSession{}, err
	}
	if input.NextOperationID != "" {
		if err := validateUUIDv7("next operation id", input.NextOperationID); err != nil {
			return LiveSession{}, err
		}
	}
	targetOperationID := input.ExpectedOperationID
	if input.NextOperationID != "" {
		targetOperationID = input.NextOperationID
	}

	unlockSession := r.lockManifestSession(input.ID)
	defer unlockSession()
	tx, err := r.writer.BeginTx(ctx, nil)
	if err != nil {
		return LiveSession{}, fmt.Errorf("begin live session transition: %w", err)
	}
	defer tx.Rollback()
	current, err := querySession(ctx, tx, sessionSelectSQL+` WHERE id = ?`, input.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, ErrSessionNotFound
	}
	if err != nil {
		return LiveSession{}, fmt.Errorf("read live session for transition: %w", err)
	}
	if transitionAlreadyApplied(current, input, targetOperationID) {
		_ = tx.Rollback()
		return r.materializeCommittedLocked(ctx, current), nil
	}
	if current.Status != input.ExpectedStatus || current.RecordingStatus != input.ExpectedRecordingStatus || current.OperationID != input.ExpectedOperationID {
		return LiveSession{}, ErrStaleTransition
	}

	next := current
	next.Status = input.Status
	next.RecordingStatus = input.RecordingStatus
	next.OperationID = targetOperationID
	next.ManifestDirty = true
	if input.EndedAt != nil {
		value := input.EndedAt.UTC().UnixMilli()
		if value < current.StartedAt {
			return LiveSession{}, errors.New("session end precedes session start")
		}
		next.EndedAt = &value
	}
	if input.MediaEpochAt != nil {
		value := input.MediaEpochAt.UTC().UnixMilli()
		next.MediaEpochAt = &value
	}
	if input.CaptureOffsetMS != nil {
		next.CaptureOffsetMS = *input.CaptureOffsetMS
	}
	if input.ClockSource != nil {
		if !validClockSource(*input.ClockSource) {
			return LiveSession{}, fmt.Errorf("invalid clock source %q", *input.ClockSource)
		}
		next.ClockSource = *input.ClockSource
	}
	if input.IntegrityScore != nil {
		if *input.IntegrityScore < 0 || *input.IntegrityScore > 1 {
			return LiveSession{}, errors.New("integrity score must be between 0 and 1")
		}
		next.IntegrityScore = *input.IntegrityScore
	}
	next.UpdatedAt = max(r.now().UTC().UnixMilli(), current.UpdatedAt+1)

	staged, err := r.stageManifest(next)
	if err != nil {
		return LiveSession{}, err
	}
	defer staged.discard()
	result, err := tx.ExecContext(ctx, `UPDATE live_sessions SET
		operation_id = ?, status = ?, recording_status = ?, manifest_dirty = 1, ended_at = ?, media_epoch_at = ?,
		capture_offset_ms = ?, clock_source = ?, integrity_score = ?, updated_at = ?
		WHERE id = ? AND status = ? AND recording_status = ? AND operation_id = ?`,
		next.OperationID, next.Status, next.RecordingStatus, next.EndedAt, next.MediaEpochAt,
		next.CaptureOffsetMS, next.ClockSource, next.IntegrityScore, next.UpdatedAt,
		current.ID, input.ExpectedStatus, input.ExpectedRecordingStatus, input.ExpectedOperationID,
	)
	if err != nil {
		return LiveSession{}, fmt.Errorf("update live session transition: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return LiveSession{}, fmt.Errorf("inspect live session transition: %w", err)
	}
	if affected != 1 {
		return LiveSession{}, ErrStaleTransition
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		if resolved, resolveErr := r.resolveTransitionOutcome(input, targetOperationID); resolveErr == nil {
			return r.promoteCommittedLocked(ctx, staged, resolved), nil
		}
		return LiveSession{}, fmt.Errorf("commit live session transition: %w", err)
	}
	return r.promoteCommittedLocked(ctx, staged, next), nil
}

func (r *SQLiteRepository) resolveCreateOutcome(input CreateSessionInput) (LiveSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := querySession(ctx, r.writer, sessionSelectSQL+` WHERE operation_id = ?`, input.OperationID)
	if err != nil {
		return LiveSession{}, err
	}
	if session.RoomConfigID != input.RoomConfigID {
		return LiveSession{}, ErrOperationConflict
	}
	return session, nil
}

func (r *SQLiteRepository) resolveTransitionOutcome(input TransitionSessionInput, targetOperationID string) (LiveSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.outcomeTimeout)
	defer cancel()
	session, err := querySession(ctx, r.writer, sessionSelectSQL+` WHERE id = ?`, input.ID)
	if err != nil {
		return LiveSession{}, err
	}
	if !transitionAlreadyApplied(session, input, targetOperationID) {
		return LiveSession{}, ErrStaleTransition
	}
	return session, nil
}

func transitionAlreadyApplied(session LiveSession, input TransitionSessionInput, targetOperationID string) bool {
	if session.OperationID != targetOperationID || session.Status != input.Status || session.RecordingStatus != input.RecordingStatus {
		return false
	}
	if input.EndedAt != nil && (session.EndedAt == nil || *session.EndedAt != input.EndedAt.UTC().UnixMilli()) {
		return false
	}
	if input.MediaEpochAt != nil && (session.MediaEpochAt == nil || *session.MediaEpochAt != input.MediaEpochAt.UTC().UnixMilli()) {
		return false
	}
	if input.CaptureOffsetMS != nil && session.CaptureOffsetMS != *input.CaptureOffsetMS {
		return false
	}
	if input.ClockSource != nil && session.ClockSource != *input.ClockSource {
		return false
	}
	if input.IntegrityScore != nil && session.IntegrityScore != *input.IntegrityScore {
		return false
	}
	return true
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func querySession(ctx context.Context, queryer queryRower, query string, args ...any) (LiveSession, error) {
	return scanSession(queryer.QueryRowContext(ctx, query, args...))
}

type rowScanner interface {
	Scan(...any) error
}

func scanSession(scanner rowScanner) (LiveSession, error) {
	var session LiveSession
	var platformRoomID sql.NullString
	var endedAt, mediaEpochAt sql.NullInt64
	var manifestDirty int
	if err := scanner.Scan(
		&session.ID, &session.RoomConfigID, &session.OperationID, &platformRoomID, &session.Title,
		&session.Status, &session.RecordingStatus, &manifestDirty, &session.StartedAt, &endedAt, &mediaEpochAt,
		&session.CaptureOffsetMS, &session.ClockSource, &session.IntegrityScore, &session.DataPath,
		&session.SchemaVersion, &session.CreatedAt, &session.UpdatedAt,
	); err != nil {
		return LiveSession{}, err
	}
	session.PlatformRoomID = platformRoomID.String
	session.ManifestDirty = manifestDirty == 1
	if endedAt.Valid {
		session.EndedAt = &endedAt.Int64
	}
	if mediaEpochAt.Valid {
		session.MediaEpochAt = &mediaEpochAt.Int64
	}
	return session, nil
}

type queryContext interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func activeForRoom(ctx context.Context, queryer queryContext, roomConfigID string) (LiveSession, bool, error) {
	session, err := querySession(ctx, queryer, sessionSelectSQL+
		` WHERE room_config_id = ? AND status IN ('starting', 'recording', 'finalizing') ORDER BY started_at DESC LIMIT 1`, roomConfigID)
	if errors.Is(err, sql.ErrNoRows) {
		return LiveSession{}, false, nil
	}
	if err != nil {
		return LiveSession{}, false, fmt.Errorf("find active live session: %w", err)
	}
	return session, true, nil
}

func (r *SQLiteRepository) writeManifest(session LiveSession) (string, error) {
	staged, err := r.stageManifest(session)
	if err != nil {
		return "", err
	}
	defer staged.discard()
	if err := r.promoteManifest(staged); err != nil {
		return "", fmt.Errorf("atomically replace session manifest: %w", err)
	}
	return staged.finalPath, nil
}

type stagedManifest struct {
	temporaryPath string
	finalPath     string
}

func (r *SQLiteRepository) stageManifest(session LiveSession) (*stagedManifest, error) {
	manifestPath, err := secureManifestPath(r.dataRoot, session.DataPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	payload, err := encodeManifest(session)
	if err != nil {
		return nil, fmt.Errorf("encode session manifest: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(manifestPath), ".session-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create session manifest temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("protect session manifest temporary file: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("write session manifest temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("sync session manifest temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return nil, fmt.Errorf("close session manifest temporary file: %w", err)
	}
	return &stagedManifest{temporaryPath: temporaryPath, finalPath: manifestPath}, nil
}

func (s *stagedManifest) promote() error {
	return os.Rename(s.temporaryPath, s.finalPath)
}

func (s *stagedManifest) discard() {
	if s != nil {
		_ = os.Remove(s.temporaryPath)
	}
}

func encodeManifest(session LiveSession) ([]byte, error) {
	payload, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	return payload, nil
}

func secureManifestPath(root, relative string) (string, error) {
	if relative == "" || strings.Contains(relative, `\`) || pathpkg.IsAbs(relative) {
		return "", errors.New("session data path must be a relative slash path")
	}
	cleaned := pathpkg.Clean(relative)
	if cleaned != relative || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("session data path escapes the data root")
	}
	target := filepath.Join(root, filepath.FromSlash(cleaned), "session.json")
	relativeTarget, err := filepath.Rel(root, target)
	if err != nil {
		return "", fmt.Errorf("validate session manifest path: %w", err)
	}
	if relativeTarget == ".." || strings.HasPrefix(relativeTarget, ".."+string(filepath.Separator)) {
		return "", errors.New("session manifest escapes the data root")
	}
	return target, nil
}

func sessionDataPath(roomConfigID string, startedAt time.Time, sessionID string) string {
	return pathpkg.Join("rooms", roomConfigID, "sessions", startedAt.UTC().Format("2006"), startedAt.UTC().Format("01"), sessionID)
}

func validateUUIDv7(label, value string) error {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed.Version() != 7 {
		return fmt.Errorf("%s must be a UUIDv7", label)
	}
	return nil
}

func requireContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("capture repository context is nil")
	}
	return ctx.Err()
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func classifyCreateError(err error) error {
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "operation_id"):
		return ErrOperationConflict
	case strings.Contains(lower, "room_config_id") && strings.Contains(lower, "unique"):
		return ErrActiveSessionExists
	default:
		return fmt.Errorf("insert live session: %w", err)
	}
}
