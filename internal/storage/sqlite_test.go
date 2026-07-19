package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
)

func TestOpenInitializesSchemaAndPragmas(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := Open(ctx, layout, OpenOptions{Now: time.Unix(1_700_000_000, 0), CreateBackups: true})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	if got, want := store.SchemaVersion(), latestSchemaVersion(schemaMigrations); got != want {
		t.Fatalf("SchemaVersion() = %d, want %d", got, want)
	}
	assertPragmaInt(t, store.Writer(), "foreign_keys", 1)
	assertPragmaInt(t, store.Writer(), "busy_timeout", 5000)
	var journalMode string
	if err := store.Writer().QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	for _, table := range []string{"rooms", "live_sessions", "live_events", "media_segments", "transcript_segments", "metric_buckets", "analysis_reports", "capture_gaps"} {
		var count int
		if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("query table %q: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("table %q count = %d, want 1", table, count)
		}
	}

	for _, column := range []string{"operation_id", "recording_status", "manifest_dirty"} {
		if !tableHasColumn(t, store.Reader(), "live_sessions", column) {
			t.Fatalf("live_sessions missing column %q", column)
		}
	}
	for _, index := range []string{
		"idx_live_sessions_active_room",
		"idx_live_sessions_operation_id",
		"idx_live_sessions_manifest_dirty",
		"idx_live_sessions_recovery_page",
		"idx_session_media_recovery_page",
	} {
		var count int
		if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&count); err != nil {
			t.Fatalf("query index %q: %v", index, err)
		}
		if count != 1 {
			t.Fatalf("index %q count = %d, want 1", index, count)
		}
	}

	_, err = store.Writer().Exec(`INSERT INTO live_sessions(
		id, room_config_id, status, started_at, clock_source, data_path, schema_version, created_at, updated_at
	) VALUES ('session', 'missing-room', 'starting', 1, 'received', 'relative', 1, 1, 1)`)
	if err == nil {
		t.Fatal("foreign-key violating insert succeeded")
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		store, err := Open(ctx, layout, OpenOptions{})
		if err != nil {
			t.Fatalf("Open() attempt %d error = %v", attempt, err)
		}
		var count int
		if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
			t.Fatalf("count migrations: %v", err)
		}
		if count != len(schemaMigrations) {
			t.Fatalf("migration count = %d, want %d", count, len(schemaMigrations))
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close() attempt %d error = %v", attempt, err)
		}
	}
}

func TestSchemaV1UpgradeBackfillsRecordingStatusAndCreatesBackup(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(layout.Database, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, db, schemaMigrations[:1], time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("apply schema v1: %v", err)
	}
	statuses := []string{"starting", "recording", "finalizing", "completed", "interrupted", "failed"}
	for i, status := range statuses {
		roomID := "room-" + status
		if _, err := db.Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at) VALUES (?, ?, ?, 1, 1)`, roomID, roomID, roomID); err != nil {
			t.Fatalf("insert room %q: %v", roomID, err)
		}
		if _, err := db.Exec(`INSERT INTO live_sessions(
			id, room_config_id, status, started_at, clock_source, data_path, schema_version, created_at, updated_at
		) VALUES (?, ?, ?, ?, 'received', ?, 1, 1, 1)`, "session-"+status, roomID, status, i+1, "rooms/"+roomID); err != nil {
			t.Fatalf("insert v1 session %q: %v", status, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 database: %v", err)
	}

	upgradeAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	store, err := Open(ctx, layout, OpenOptions{Now: upgradeAt, CreateBackups: true})
	if err != nil {
		t.Fatalf("Open() upgrade error = %v", err)
	}
	defer store.Close()
	if got, want := store.SchemaVersion(), latestSchemaVersion(schemaMigrations); got != want {
		t.Fatalf("SchemaVersion() = %d, want %d", got, want)
	}
	want := map[string]string{
		"starting": "starting", "recording": "recording", "finalizing": "finalizing",
		"completed": "completed", "interrupted": "incomplete", "failed": "failed",
	}
	for status, recordingStatus := range want {
		var gotStatus, operationID string
		var manifestDirty int
		if err := store.Reader().QueryRow(`SELECT recording_status, operation_id, manifest_dirty FROM live_sessions WHERE id = ?`, "session-"+status).Scan(&gotStatus, &operationID, &manifestDirty); err != nil {
			t.Fatalf("read upgraded session %q: %v", status, err)
		}
		if gotStatus != recordingStatus || operationID != "" || manifestDirty != 1 {
			t.Fatalf("upgraded %q = (%q, %q, dirty=%d), want (%q, empty, dirty=1)", status, gotStatus, operationID, manifestDirty, recordingStatus)
		}
	}
	backups, err := filepath.Glob(filepath.Join(layout.BackupsDir, "app-v1-*.db"))
	if err != nil {
		t.Fatalf("Glob backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("v1 backup count = %d, want 1", len(backups))
	}
	backupDB, err := sql.Open("sqlite", sqliteDSN(backups[0], true))
	if err != nil {
		t.Fatalf("open v1 backup: %v", err)
	}
	defer backupDB.Close()
	if tableHasColumn(t, backupDB, "live_sessions", "recording_status") {
		t.Fatal("v1 backup unexpectedly contains recording_status")
	}
}

func TestSchemaV2DuplicateActiveSessionsRollBackCompletely(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "duplicate-active.db")
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, db, schemaMigrations[:1], time.Now()); err != nil {
		t.Fatalf("apply schema v1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at) VALUES ('room', 'live', 'alias', 1, 1)`); err != nil {
		t.Fatalf("insert room: %v", err)
	}
	for _, status := range []string{"starting", "recording"} {
		if _, err := db.Exec(`INSERT INTO live_sessions(
			id, room_config_id, status, started_at, clock_source, data_path, schema_version, created_at, updated_at
		) VALUES (?, 'room', ?, 1, 'received', ?, 1, 1, 1)`, status, status, status); err != nil {
			t.Fatalf("insert duplicate active session: %v", err)
		}
	}
	if _, err := applyMigrationSet(ctx, db, schemaMigrations, time.Now()); err == nil {
		t.Fatal("schema v2 migration succeeded with duplicate active sessions")
	}
	if tableHasColumn(t, db, "live_sessions", "operation_id") || tableHasColumn(t, db, "live_sessions", "recording_status") || tableHasColumn(t, db, "live_sessions", "manifest_dirty") {
		t.Fatal("failed schema v2 migration left added columns behind")
	}
	version, err := currentSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("currentSchemaVersion() error = %v", err)
	}
	if version != 1 {
		t.Fatalf("schema version = %d, want 1 after rollback", version)
	}
}

func TestSchemaV2RejectsInvalidRecordingStatusAndDuplicateOperation(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := Open(ctx, layout, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	for _, roomID := range []string{"room-a", "room-b"} {
		if _, err := store.Writer().Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at) VALUES (?, ?, ?, 1, 1)`, roomID, roomID, roomID); err != nil {
			t.Fatalf("insert room: %v", err)
		}
	}
	insert := `INSERT INTO live_sessions(
		id, room_config_id, status, recording_status, operation_id, started_at, clock_source, data_path, schema_version, created_at, updated_at
	) VALUES (?, ?, 'completed', ?, ?, 1, 'received', ?, 1, 1, 1)`
	if _, err := store.Writer().Exec(insert, "bad", "room-a", "not-a-status", "op-a", "bad"); err == nil {
		t.Fatal("invalid recording_status insert succeeded")
	}
	if _, err := store.Writer().Exec(insert, "one", "room-a", "completed", "same-op", "one"); err != nil {
		t.Fatalf("insert first operation: %v", err)
	}
	if _, err := store.Writer().Exec(insert, "two", "room-b", "completed", "same-op", "two"); err == nil {
		t.Fatal("duplicate nonempty operation_id insert succeeded")
	}
	var manifestDirty int
	if err := store.Reader().QueryRow(`SELECT manifest_dirty FROM live_sessions WHERE id = 'one'`).Scan(&manifestDirty); err != nil {
		t.Fatalf("read default manifest_dirty: %v", err)
	}
	if manifestDirty != 1 {
		t.Fatalf("default manifest_dirty = %d, want 1", manifestDirty)
	}
	if _, err := store.Writer().Exec(`UPDATE live_sessions SET manifest_dirty = 2 WHERE id = 'one'`); err == nil {
		t.Fatal("invalid manifest_dirty update succeeded")
	}
}

func TestSchemaV1ActiveSessionCanBeClaimedTerminatedAndReplacedAfterUpgrade(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(layout.Database, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, db, schemaMigrations[:1], time.Unix(1_700_000_000, 0)); err != nil {
		db.Close()
		t.Fatalf("apply schema v1: %v", err)
	}
	roomID := mustUUIDv7(t)
	sessionID := mustUUIDv7(t)
	startedAt := time.Date(2026, 7, 17, 8, 0, 0, 0, time.UTC)
	dataPath := "rooms/" + roomID + "/sessions/2026/07/" + sessionID
	if _, err := db.Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at)
		VALUES (?, ?, 'legacy-room', 1, 1)`, roomID, "legacy-live-"+roomID); err != nil {
		db.Close()
		t.Fatalf("insert v1 room: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO live_sessions(
		id, room_config_id, status, started_at, clock_source, data_path, schema_version, created_at, updated_at
	) VALUES (?, ?, 'starting', ?, 'received', ?, 1, 1, 1)`, sessionID, roomID, startedAt.UnixMilli(), dataPath); err != nil {
		db.Close()
		t.Fatalf("insert v1 active session: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v1 database: %v", err)
	}

	store, err := Open(ctx, layout, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() upgrade error = %v", err)
	}
	defer store.Close()
	if got, want := store.SchemaVersion(), latestSchemaVersion(schemaMigrations); got != want {
		t.Fatalf("SchemaVersion() = %d, want %d", got, want)
	}
	repo, err := capture.NewSQLiteRepository(store.Writer(), store.Reader(), layout.Root)
	if err != nil {
		t.Fatalf("NewSQLiteRepository() error = %v", err)
	}
	claimOperationID := mustUUIDv7(t)
	endedAt := startedAt.Add(time.Minute)
	transition := capture.TransitionSessionInput{
		ID: sessionID, ExpectedStatus: capture.SessionStarting,
		ExpectedRecordingStatus: capture.RecordingStarting, ExpectedOperationID: "",
		Status: capture.SessionInterrupted, RecordingStatus: capture.RecordingIncomplete,
		NextOperationID: claimOperationID, EndedAt: &endedAt,
	}
	terminated, err := repo.Transition(ctx, transition)
	if err != nil {
		t.Fatalf("claim and terminate migrated session: %v", err)
	}
	if terminated.OperationID != claimOperationID || terminated.Status != capture.SessionInterrupted || terminated.RecordingStatus != capture.RecordingIncomplete {
		t.Fatalf("terminated migrated session = %+v", terminated)
	}
	if replayed, err := repo.Transition(ctx, transition); err != nil || replayed.ID != sessionID {
		t.Fatalf("idempotent migrated transition replay = (%+v, %v)", replayed, err)
	}
	manifestPath := filepath.Join(layout.Root, filepath.FromSlash(dataPath), "session.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("migrated session manifest missing: %v", err)
	}

	newSession, err := repo.Create(ctx, capture.CreateSessionInput{
		RoomConfigID: roomID, OperationID: mustUUIDv7(t), StartedAt: startedAt.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Create() after terminating migrated session: %v", err)
	}
	if newSession.ID == sessionID {
		t.Fatal("new session reused migrated session ID")
	}
	if active, found, err := repo.ActiveForRoom(ctx, roomID); err != nil || !found || active.ID != newSession.ID {
		t.Fatalf("ActiveForRoom() = (%+v, %v, %v), want new session", active, found, err)
	}
}

func TestFailedMigrationRollsBackDDLAndVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rollback.db")
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	bad := []migration{{Version: 1, Name: "bad", Statements: []string{
		`CREATE TABLE partial_table(id INTEGER PRIMARY KEY)`,
		`THIS IS NOT SQL`,
	}}}
	if _, err := applyMigrationSet(ctx, db, bad, time.Now()); err == nil {
		t.Fatal("applyMigrationSet() error = nil, want failure")
	}
	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'partial_table'`).Scan(&tableCount); err != nil {
		t.Fatalf("query partial table: %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("partial table count = %d, want 0", tableCount)
	}
	var migrationCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrationCount); err != nil {
		t.Fatalf("query migration count: %v", err)
	}
	if migrationCount != 0 {
		t.Fatalf("migration count = %d, want 0", migrationCount)
	}
}

func TestBackupDatabaseCreatesConsistentCopy(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := Open(ctx, layout, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	backup, err := backupDatabase(ctx, store.Writer(), layout, store.SchemaVersion(), time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatalf("backupDatabase() error = %v", err)
	}
	if info, err := os.Stat(backup); err != nil || info.Size() == 0 {
		t.Fatalf("backup Stat() = (%v, %v)", info, err)
	}
	backupDB, err := sql.Open("sqlite", sqliteDSN(backup, true))
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer backupDB.Close()
	if err := quickCheck(ctx, backupDB); err != nil {
		t.Fatalf("backup quickCheck() error = %v", err)
	}
}

func TestQuickCheckErrorClassification(t *testing.T) {
	if !errors.Is(errors.Join(ErrIntegrityCheck), ErrIntegrityCheck) {
		t.Fatal("ErrIntegrityCheck must support errors.Is")
	}
}

func TestSchemaV3EmptyDatabaseCreatesEventIngestObjects(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := Open(ctx, layout, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	for _, table := range []string{"event_ingest_checkpoints", "gift_combo_states"} {
		assertSQLiteObject(t, store.Reader(), "table", table)
	}
	for _, column := range []string{"ingest_sequence", "event_role", "normalizer_version", "parse_error_code"} {
		if !tableHasColumn(t, store.Reader(), "live_events", column) {
			t.Fatalf("live_events missing column %q", column)
		}
	}
	if !tableHasColumn(t, store.Reader(), "capture_gaps", "dedupe_key") {
		t.Fatal("capture_gaps missing dedupe_key")
	}
	for _, index := range []string{
		"idx_live_events_session_ingest", "idx_live_events_source_sequence",
		"idx_live_events_role_offset",
		"idx_event_ingest_checkpoints_sequence", "idx_gift_combo_states_status_updated",
		"idx_capture_gaps_kind_recovered",
	} {
		assertSQLiteObject(t, store.Reader(), "index", index)
	}
	for _, trigger := range []string{
		"trg_event_checkpoint_privacy_key_immutable",
		"trg_event_checkpoint_state_transition",
		"trg_event_checkpoint_closed_immutable",
		"trg_gift_combo_closed_immutable",
	} {
		assertSQLiteObject(t, store.Reader(), "trigger", trigger)
	}
}

func TestSchemaV2UpgradePreservesLegacyEventsAndCaptureGaps(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(layout.Database, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, db, schemaMigrations[:2], time.Unix(1_700_000_000, 0)); err != nil {
		db.Close()
		t.Fatalf("apply schema v2: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at)
		VALUES ('room-v2', 'live-v2', 'legacy', 1, 1)`); err != nil {
		db.Close()
		t.Fatalf("insert v2 room: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO live_sessions(
		id, room_config_id, status, started_at, clock_source, data_path,
		schema_version, created_at, updated_at
	) VALUES ('session-v2', 'room-v2', 'completed', 1, 'received', 'rooms/legacy', 1, 1, 1)`); err != nil {
		db.Close()
		t.Fatalf("insert v2 session: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO live_events(
		id, session_id, method, kind, dedupe_key, received_at,
		session_offset_ms, clock_confidence, parse_status
	) VALUES ('event-v2', 'session-v2', 'LegacyMethod', 'unknown', 'legacy-event', 2, 1, 0, 'unknown')`); err != nil {
		db.Close()
		t.Fatalf("insert v2 event: %v", err)
	}
	// Schema v2 permitted these reversed terminal values. The v3 rebuild must
	// preserve every v2-valid row instead of silently dropping or rewriting it.
	if _, err := db.Exec(`INSERT INTO capture_gaps(
		id, session_id, kind, started_at, ended_at, start_offset_ms,
		end_offset_ms, severity, recovered, reason_code, details_json
	) VALUES ('gap-v2', 'session-v2', 'message_disconnect', 20, 10, 20, 10,
		'warning', 0, 'LEGACY_GAP', '{}')`); err != nil {
		db.Close()
		t.Fatalf("insert v2 gap: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close v2 database: %v", err)
	}

	upgradeAt := time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC)
	store, err := Open(ctx, layout, OpenOptions{Now: upgradeAt, CreateBackups: true})
	if err != nil {
		t.Fatalf("Open() v3 upgrade error = %v", err)
	}
	defer store.Close()
	if got := store.SchemaVersion(); got != 5 {
		t.Fatalf("SchemaVersion() = %d, want 5", got)
	}

	var sequence int64
	var role, normalizer string
	var parseCode sql.NullString
	if err := store.Reader().QueryRow(`SELECT ingest_sequence, event_role,
		normalizer_version, parse_error_code FROM live_events WHERE id = 'event-v2'`).Scan(
		&sequence, &role, &normalizer, &parseCode,
	); err != nil {
		t.Fatalf("read upgraded legacy event: %v", err)
	}
	if sequence != 0 || role != "source" || normalizer != "legacy-v1" || parseCode.Valid {
		t.Fatalf("legacy event v3 defaults = (%d, %q, %q, %+v)", sequence, role, normalizer, parseCode)
	}
	var startedAt, endedAt, startOffset, endOffset int64
	var dedupeKey string
	if err := store.Reader().QueryRow(`SELECT started_at, ended_at, start_offset_ms,
		end_offset_ms, dedupe_key FROM capture_gaps WHERE id = 'gap-v2'`).Scan(
		&startedAt, &endedAt, &startOffset, &endOffset, &dedupeKey,
	); err != nil {
		t.Fatalf("read upgraded legacy gap: %v", err)
	}
	if startedAt != 20 || endedAt != 10 || startOffset != 20 || endOffset != 10 || dedupeKey != "legacy:gap-v2" {
		t.Fatalf("upgraded legacy gap = (%d, %d, %d, %d, %q)", startedAt, endedAt, startOffset, endOffset, dedupeKey)
	}
	rows, err := store.Reader().Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if rows.Next() {
		rows.Close()
		t.Fatal("schema v3 upgrade left a foreign-key violation")
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close foreign_key_check rows: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(layout.BackupsDir, "app-v2-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("v2 backup files = (%v, %v), want one", backups, err)
	}
	backupDB, err := sql.Open("sqlite", sqliteDSN(backups[0], true))
	if err != nil {
		t.Fatalf("open v2 backup: %v", err)
	}
	defer backupDB.Close()
	if tableHasColumn(t, backupDB, "live_events", "ingest_sequence") ||
		tableHasColumn(t, backupDB, "capture_gaps", "dedupe_key") {
		t.Fatal("v2 backup unexpectedly contains schema v3 columns")
	}
}

func TestSchemaV3FailureRollsBackRebuildAndVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3-rollback.db")
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, db, schemaMigrations[:2], time.Now()); err != nil {
		t.Fatalf("apply schema v2: %v", err)
	}
	brokenV3 := schemaMigrations[2]
	brokenV3.Statements = []string{
		brokenV3.Statements[0],
		`ALTER TABLE capture_gaps RENAME TO capture_gaps_v2`,
		`THIS IS NOT SQL`,
	}
	if _, err := applyMigrationSet(ctx, db, []migration{brokenV3}, time.Now()); err == nil {
		t.Fatal("broken schema v3 migration succeeded")
	}
	if tableHasColumn(t, db, "live_events", "ingest_sequence") {
		t.Fatal("failed schema v3 migration left live_events column behind")
	}
	assertSQLiteObject(t, db, "table", "capture_gaps")
	var renamedCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'capture_gaps_v2'`).Scan(&renamedCount); err != nil {
		t.Fatalf("query capture_gaps_v2: %v", err)
	}
	if renamedCount != 0 {
		t.Fatal("failed schema v3 migration left renamed capture_gaps_v2 behind")
	}
	version, err := currentSchemaVersion(ctx, db)
	if err != nil {
		t.Fatalf("currentSchemaVersion() error = %v", err)
	}
	if version != 2 {
		t.Fatalf("schema version = %d, want 2 after v3 rollback", version)
	}
}

func TestSchemaV3EnforcesCheckpointComboAndGapConstraints(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := Open(ctx, layout, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	db := store.Writer()
	for _, roomID := range []string{"room-a", "room-b"} {
		if _, err := db.Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at) VALUES (?, ?, ?, 1, 1)`, roomID, roomID, roomID); err != nil {
			t.Fatalf("insert room %q: %v", roomID, err)
		}
		sessionID := "session-" + roomID
		if _, err := db.Exec(`INSERT INTO live_sessions(
			id, room_config_id, status, started_at, clock_source, data_path,
			schema_version, created_at, updated_at
		) VALUES (?, ?, 'completed', 1, 'received', ?, 1, 1, 1)`, sessionID, roomID, "rooms/"+roomID); err != nil {
			t.Fatalf("insert session %q: %v", sessionID, err)
		}
	}
	insertEvent := `INSERT INTO live_events(
		id, session_id, ingest_sequence, event_role, method, kind, dedupe_key,
		received_at, session_offset_ms, clock_confidence, normalized_json,
		parse_status, normalizer_version
	) VALUES (?, ?, 1, ?, 'GiftMethod', 'gift', ?, 2, 1, 1, '{}', 'parsed', 'v1')`
	if _, err := db.Exec(insertEvent, "aggregate-a", "session-room-a", "aggregate", "aggregate-a"); err != nil {
		t.Fatalf("insert aggregate event: %v", err)
	}
	if _, err := db.Exec(insertEvent, "source-a", "session-room-a", "source", "source-a"); err != nil {
		t.Fatalf("insert source event: %v", err)
	}
	if _, err := db.Exec(insertEvent, "source-b", "session-room-a", "source", "source-b"); err == nil {
		t.Fatal("duplicate source ingest_sequence insert succeeded")
	}
	if _, err := db.Exec(insertEvent, "aggregate-b", "session-room-a", "aggregate", "aggregate-b"); err != nil {
		t.Fatalf("second aggregate at shared ingest_sequence: %v", err)
	}
	if _, err := db.Exec(insertEvent, "bad-role", "session-room-a", "not-a-role", "bad-role"); err == nil {
		t.Fatal("invalid event_role insert succeeded")
	}

	insertCheckpoint := `INSERT INTO event_ingest_checkpoints(
		session_id, committed_sequence, state, privacy_key_id,
		spool_file, spool_offset, raw_file, raw_offset, updated_at
	) VALUES ('session-room-a', 0, ?, ?, '', 0, '', 0, 1)`
	if _, err := db.Exec(insertCheckpoint, "invalid", "key-a"); err == nil {
		t.Fatal("invalid checkpoint state insert succeeded")
	}
	if _, err := db.Exec(insertCheckpoint, "open", ""); err == nil {
		t.Fatal("empty checkpoint privacy_key_id insert succeeded")
	}
	if _, err := db.Exec(`INSERT INTO event_ingest_checkpoints(
		session_id, committed_sequence, state, privacy_key_id,
		spool_file, spool_offset, raw_file, raw_offset, updated_at
	) VALUES ('session-room-b', 1, 'open', 'key-b', 'spool/a.wal', 1, '', 0, 1)`); err == nil {
		t.Fatal("positive checkpoint without raw position succeeded")
	}
	if _, err := db.Exec(insertCheckpoint, "open", "key-a"); err != nil {
		t.Fatalf("insert valid checkpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE event_ingest_checkpoints SET privacy_key_id = 'key-b' WHERE session_id = 'session-room-a'`); err == nil {
		t.Fatal("checkpoint privacy key mutation succeeded")
	}
	if _, err := db.Exec(`UPDATE event_ingest_checkpoints SET state = 'closed' WHERE session_id = 'session-room-a'`); err == nil {
		t.Fatal("checkpoint open-to-closed transition succeeded")
	}
	if _, err := db.Exec(`UPDATE event_ingest_checkpoints SET state = 'closing' WHERE session_id = 'session-room-a'`); err != nil {
		t.Fatalf("checkpoint open-to-closing transition: %v", err)
	}
	if _, err := db.Exec(`UPDATE event_ingest_checkpoints SET state = 'closed' WHERE session_id = 'session-room-a'`); err != nil {
		t.Fatalf("checkpoint closing-to-closed transition: %v", err)
	}
	if _, err := db.Exec(`UPDATE event_ingest_checkpoints SET updated_at = 2 WHERE session_id = 'session-room-a'`); err == nil {
		t.Fatal("closed checkpoint mutation succeeded")
	}

	insertCombo := `INSERT INTO gift_combo_states(
		session_id, combo_key, status, gift_id, total_count,
		first_ingest_sequence, last_ingest_sequence, started_at, updated_at,
		closed_at, aggregate_event_id, normalizer_version
	) VALUES (?, ?, ?, 'gift-1', 2, 1, 1, 1, 2, ?, ?, 'v1')`
	if _, err := db.Exec(insertCombo, "session-room-a", "bad-open", "open", int64(3), "aggregate-a"); err == nil {
		t.Fatal("open combo with close fields insert succeeded")
	}
	if _, err := db.Exec(insertCombo, "session-room-b", "cross-session", "closed", int64(3), "aggregate-a"); err == nil {
		t.Fatal("cross-session aggregate reference succeeded")
	}
	if _, err := db.Exec(insertCombo, "session-room-a", "closed-combo", "closed", int64(3), "aggregate-a"); err != nil {
		t.Fatalf("insert valid closed combo: %v", err)
	}
	closedMutations := []struct {
		name       string
		assignment string
	}{
		{name: "session_id", assignment: "session_id = 'session-room-b'"},
		{name: "combo_key", assignment: "combo_key = 'renamed-combo'"},
		{name: "status", assignment: "status = 'open'"},
		{name: "user_hash", assignment: "user_hash = 'changed-user'"},
		{name: "gift_id", assignment: "gift_id = 'gift-2'"},
		{name: "gift_name", assignment: "gift_name = 'changed-gift'"},
		{name: "total_count", assignment: "total_count = 3"},
		{name: "total_value", assignment: "total_value = 6"},
		{name: "first_ingest_sequence", assignment: "first_ingest_sequence = 2"},
		{name: "last_ingest_sequence", assignment: "last_ingest_sequence = 2"},
		{name: "started_at", assignment: "started_at = 0"},
		{name: "updated_at", assignment: "updated_at = 3"},
		{name: "closed_at", assignment: "closed_at = 4"},
		{name: "aggregate_event_id", assignment: "aggregate_event_id = 'aggregate-b'"},
		{name: "normalizer_version", assignment: "normalizer_version = 'v2'"},
	}
	for _, mutation := range closedMutations {
		t.Run("closed_combo_rejects_"+mutation.name, func(t *testing.T) {
			_, err := db.Exec(`UPDATE gift_combo_states SET ` + mutation.assignment +
				` WHERE session_id = 'session-room-a' AND combo_key = 'closed-combo'`)
			if err == nil {
				t.Fatalf("closed combo %s mutation succeeded", mutation.name)
			}
			if !strings.Contains(err.Error(), "gift combo is already closed") {
				t.Fatalf("closed combo %s mutation used wrong constraint: %v", mutation.name, err)
			}
		})
	}
	if _, err := db.Exec(`UPDATE gift_combo_states SET
		session_id = session_id,
		combo_key = combo_key,
		status = status,
		user_hash = user_hash,
		gift_id = gift_id,
		gift_name = gift_name,
		total_count = total_count,
		total_value = total_value,
		first_ingest_sequence = first_ingest_sequence,
		last_ingest_sequence = last_ingest_sequence,
		started_at = started_at,
		updated_at = updated_at,
		closed_at = closed_at,
		aggregate_event_id = aggregate_event_id,
		normalizer_version = normalizer_version
		WHERE session_id = 'session-room-a' AND combo_key = 'closed-combo'`); err != nil {
		t.Fatalf("closed combo exact no-op update failed: %v", err)
	}

	insertGap := `INSERT INTO capture_gaps(
		id, session_id, kind, started_at, start_offset_ms, severity,
		reason_code, details_json, dedupe_key
	) VALUES (?, 'session-room-a', ?, 1, 0, 'warning', 'PERSISTENCE_BUSY', '{}', ?)`
	if _, err := db.Exec(insertGap, "gap-a", "event_persistence", "gap-key"); err != nil {
		t.Fatalf("insert event_persistence gap: %v", err)
	}
	if _, err := db.Exec(insertGap, "gap-b", "event_persistence", "gap-key"); err == nil {
		t.Fatal("duplicate capture gap dedupe key succeeded")
	}
	if _, err := db.Exec(insertGap, "gap-c", "not-a-gap", "other-key"); err == nil {
		t.Fatal("invalid capture gap kind succeeded")
	}
}

func TestSchemaV3UpgradeCreatesV4BackupAndMediaObjects(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(layout.Database, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, db, schemaMigrations[:3], time.Unix(1_700_000_000, 0)); err != nil {
		db.Close()
		t.Fatalf("apply schema v3: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close schema v3 database: %v", err)
	}

	upgradeAt := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)
	store, err := Open(ctx, layout, OpenOptions{Now: upgradeAt, CreateBackups: true})
	if err != nil {
		t.Fatalf("Open() schema v4 upgrade error = %v", err)
	}
	defer store.Close()
	if got := store.SchemaVersion(); got != 5 {
		t.Fatalf("SchemaVersion() = %d, want 5", got)
	}
	for _, table := range []string{"recording_roots", "session_media", "media_artifacts"} {
		assertSQLiteObject(t, store.Reader(), "table", table)
	}
	for _, column := range []string{"attempt_id", "attempt_sequence", "source_relative_path", "probe_version", "error_code"} {
		if !tableHasColumn(t, store.Reader(), "media_segments", column) {
			t.Fatalf("media_segments missing schema v4 column %q", column)
		}
	}
	if !tableHasExtendedColumn(t, store.Reader(), "session_media", "relative_key") {
		t.Fatal("session_media missing generated relative_key column")
	}

	backups, err := filepath.Glob(filepath.Join(layout.BackupsDir, "app-v3-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("v3 backup files = (%v, %v), want one", backups, err)
	}
	backupDB, err := sql.Open("sqlite", sqliteDSN(backups[0], true))
	if err != nil {
		t.Fatalf("open v3 backup: %v", err)
	}
	defer backupDB.Close()
	version, err := currentSchemaVersion(ctx, backupDB)
	if err != nil || version != 3 {
		t.Fatalf("backup schema version = (%d, %v), want (3, nil)", version, err)
	}
	var v4TableCount int
	if err := backupDB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'session_media'`).Scan(&v4TableCount); err != nil {
		t.Fatalf("inspect v3 backup: %v", err)
	}
	if v4TableCount != 0 || tableHasColumn(t, backupDB, "media_segments", "attempt_id") {
		t.Fatal("v3 backup unexpectedly contains schema v4 objects")
	}
}

func TestSchemaV4FailureRollsBackDDLAndVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v4-rollback.db")
	db, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, db, schemaMigrations[:3], time.Now()); err != nil {
		t.Fatalf("apply schema v3: %v", err)
	}
	brokenV4 := schemaMigrations[3]
	brokenV4.Statements = append([]string(nil), brokenV4.Statements[:3]...)
	brokenV4.Statements = append(brokenV4.Statements, `THIS IS NOT SQL`)
	if _, err := applyMigrationSet(ctx, db, []migration{brokenV4}, time.Now()); err == nil {
		t.Fatal("broken schema v4 migration succeeded")
	}
	for _, table := range []string{"recording_roots", "session_media"} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
			t.Fatalf("inspect rolled-back table %q: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("failed schema v4 migration left table %q behind", table)
		}
	}
	if tableHasColumn(t, db, "media_segments", "attempt_id") {
		t.Fatal("failed schema v4 migration left media_segments column behind")
	}
	version, err := currentSchemaVersion(ctx, db)
	if err != nil || version != 3 {
		t.Fatalf("schema version after rollback = (%d, %v), want (3, nil)", version, err)
	}
}

func TestSchemaV4EnforcesMediaRootAndArtifactConstraints(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := Open(ctx, layout, OpenOptions{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	db := store.Writer()
	if _, err := db.Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at)
		VALUES ('room-v4', 'live-v4', 'v4', 1, 1)`); err != nil {
		t.Fatalf("insert room: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO live_sessions(
		id, room_config_id, status, started_at, clock_source, data_path,
		schema_version, created_at, updated_at
	) VALUES ('session-v4', 'room-v4', 'completed', 1, 'received', 'rooms/v4', 1, 1, 1)`); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO live_sessions(
		id, room_config_id, status, started_at, clock_source, data_path,
		schema_version, created_at, updated_at
	) VALUES ('session-v4-case', 'room-v4', 'completed', 1, 'received',
		'rooms/v4-case', 1, 1, 1)`); err != nil {
		t.Fatalf("insert case-alias session: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO recording_roots(
		id, absolute_path, canonical_key, volume_identity, status,
		created_at, updated_at, last_verified_at
	) VALUES ('root-v4', 'D:/recordings', ?, ?, 'ready', 1, 1, 1)`,
		strings.Repeat("a", 64), strings.Repeat("b", 64)); err != nil {
		t.Fatalf("insert recording root: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO recording_roots(
		id, absolute_path, canonical_key, volume_identity, status,
		created_at, updated_at, last_verified_at
	) VALUES ('bad-root', 'D:/bad', ?, ?, 'missing', 1, 1, 1)`,
		strings.Repeat("c", 64), strings.Repeat("d", 64)); err == nil {
		t.Fatal("recording root with invalid status succeeded")
	}
	if _, err := db.Exec(`INSERT INTO session_media(
		session_id, root_id, relative_path, state, attempts_json, created_at, updated_at
	) VALUES ('session-v4', 'root-v4', 'sessions/v4/media', 'open', '[]', 1, 1)`); err != nil {
		t.Fatalf("insert session media: %v", err)
	}
	var relativeKey string
	if err := db.QueryRow(`SELECT relative_key FROM session_media WHERE session_id = 'session-v4'`).Scan(&relativeKey); err != nil {
		t.Fatalf("read generated relative key: %v", err)
	}
	if relativeKey != "sessions/v4/media" {
		t.Fatalf("generated relative key = %q", relativeKey)
	}
	if _, err := db.Exec(`INSERT INTO session_media(
		session_id, root_id, relative_path, state, attempts_json, created_at, updated_at
	) VALUES ('session-v4-case', 'root-v4', 'SESSIONS/V4/MEDIA', 'open', '[]', 1, 1)`); err == nil {
		t.Fatal("case-aliased session media path succeeded")
	}
	if _, err := db.Exec(`UPDATE session_media SET relative_path = 'sessions/v4/other' WHERE session_id = 'session-v4'`); err == nil {
		t.Fatal("session media location mutation succeeded")
	}
	if _, err := db.Exec(`DELETE FROM recording_roots WHERE id = 'root-v4'`); err == nil {
		t.Fatal("deleting an in-use recording root succeeded")
	}
	if _, err := db.Exec(`INSERT INTO media_segments(
		id, session_id, sequence, relative_path, container, started_at, ended_at,
		duration_ms, size_bytes, status, attempt_id, attempt_sequence,
		source_relative_path, probe_version
	) VALUES ('segment-v4', 'session-v4', 1, 'segments/1.mkv', 'matroska', 1, 2,
		1, 1, 'complete', 'attempt-v4', 1, 'attempts/1.partial', 'ffprobe-8.1.2')`); err != nil {
		t.Fatalf("insert media segment: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO media_segments(
		id, session_id, sequence, relative_path, container, started_at, ended_at,
		duration_ms, size_bytes, status, attempt_id, attempt_sequence,
		source_relative_path, probe_version
	) VALUES ('segment-v4-path-case', 'session-v4', 2, 'SEGMENTS/1.MKV', 'matroska', 1, 2,
		1, 1, 'complete', 'attempt-v4-path-case', 2, 'attempts/path-case.partial', 'ffprobe-8.1.2')`); err == nil {
		t.Fatal("case-aliased segment relative path succeeded")
	}
	if _, err := db.Exec(`INSERT INTO media_segments(
		id, session_id, sequence, relative_path, container, started_at, ended_at,
		duration_ms, size_bytes, status, attempt_id, attempt_sequence,
		source_relative_path, probe_version
	) VALUES ('segment-v4-source-case', 'session-v4', 2, 'segments/source-case.mkv', 'matroska', 1, 2,
		1, 1, 'complete', 'attempt-v4-source-case', 2, 'ATTEMPTS/1.PARTIAL', 'ffprobe-8.1.2')`); err == nil {
		t.Fatal("case-aliased segment source path succeeded")
	}
	if _, err := db.Exec(`INSERT INTO media_segments(
		id, session_id, sequence, relative_path, container, started_at, ended_at,
		duration_ms, size_bytes, status, attempt_id, attempt_sequence,
		source_relative_path, probe_version
	) VALUES ('segment-v4-2', 'session-v4', 2, 'segments/2.mkv', 'matroska', 1, 2,
		1, 1, 'complete', 'attempt-v4-2', 2, 'attempts/2.partial', 'ffprobe-8.1.2')`); err != nil {
		t.Fatalf("insert second media segment: %v", err)
	}
	insertArtifact := `INSERT INTO media_artifacts(
		id, session_id, media_segment_id, kind, relative_path, status, created_at, updated_at
	) VALUES (?, 'session-v4', 'segment-v4', ?, ?, ?, 1, 1)`
	if _, err := db.Exec(insertArtifact, "artifact-v4", "playback_mp4", "artifacts/1.mp4", "pending_transcode"); err != nil {
		t.Fatalf("insert pending_transcode artifact: %v", err)
	}
	if _, err := db.Exec(insertArtifact, "bad-artifact", "asr_wav", "artifacts/1.wav", "uploaded"); err == nil {
		t.Fatal("media artifact with invalid status succeeded")
	}
	if _, err := db.Exec(insertArtifact, "duplicate-artifact", "playback_mp4", "artifacts/other.mp4", "complete"); err == nil {
		t.Fatal("duplicate segment artifact kind succeeded")
	}
	if _, err := db.Exec(`INSERT INTO media_artifacts(
		id, session_id, media_segment_id, kind, relative_path, status, created_at, updated_at
	) VALUES ('artifact-v4-path-case', 'session-v4', 'segment-v4-2', 'asr_wav',
		'ARTIFACTS/1.MP4', 'complete', 1, 1)`); err == nil {
		t.Fatal("case-aliased artifact path succeeded")
	}
}

func TestSchemaV4UpgradeCreatesV5RecoveryIndexesAndBackup(t *testing.T) {
	ctx := context.Background()
	layout, err := PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	database, err := sql.Open("sqlite", sqliteDSN(layout.Database, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	database.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(
		ctx, database, schemaMigrations[:4], time.Unix(1_700_000_000, 0),
	); err != nil {
		database.Close()
		t.Fatalf("apply schema v4: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close schema v4 database: %v", err)
	}

	upgradeAt := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	store, err := Open(ctx, layout, OpenOptions{
		Now: upgradeAt, CreateBackups: true,
	})
	if err != nil {
		t.Fatalf("Open() schema v5 upgrade error = %v", err)
	}
	defer store.Close()
	if got := store.SchemaVersion(); got != 5 {
		t.Fatalf("SchemaVersion() = %d, want 5", got)
	}
	for _, index := range []string{
		"idx_live_sessions_recovery_page",
		"idx_session_media_recovery_page",
	} {
		assertSQLiteObject(t, store.Reader(), "index", index)
	}

	backups, err := filepath.Glob(filepath.Join(layout.BackupsDir, "app-v4-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("v4 backup files = (%v, %v), want one", backups, err)
	}
	backup, err := sql.Open("sqlite", sqliteDSN(backups[0], true))
	if err != nil {
		t.Fatalf("open v4 backup: %v", err)
	}
	defer backup.Close()
	version, err := currentSchemaVersion(ctx, backup)
	if err != nil || version != 4 {
		t.Fatalf("backup schema version = (%d, %v), want (4, nil)", version, err)
	}
	for _, index := range []string{
		"idx_live_sessions_recovery_page",
		"idx_session_media_recovery_page",
	} {
		var count int
		if err := backup.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?",
			index,
		).Scan(&count); err != nil {
			t.Fatalf("inspect v4 backup index %q: %v", index, err)
		}
		if count != 0 {
			t.Fatalf("v4 backup unexpectedly contains index %q", index)
		}
	}
}

func TestSchemaV5FailureRollsBackIndexesAndVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v5-rollback.db")
	database, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(
		ctx, database, schemaMigrations[:4], time.Now(),
	); err != nil {
		t.Fatalf("apply schema v4: %v", err)
	}
	broken := schemaMigrations[4]
	broken.Statements = []string{broken.Statements[0], "THIS IS NOT SQL"}
	if _, err := applyMigrationSet(
		ctx, database, []migration{broken}, time.Now(),
	); err == nil {
		t.Fatal("broken schema v5 migration succeeded")
	}
	for _, index := range []string{
		"idx_live_sessions_recovery_page",
		"idx_session_media_recovery_page",
	} {
		var count int
		if err := database.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?",
			index,
		).Scan(&count); err != nil {
			t.Fatalf("inspect rolled-back index %q: %v", index, err)
		}
		if count != 0 {
			t.Fatalf("failed schema v5 migration left index %q behind", index)
		}
	}
	version, err := currentSchemaVersion(ctx, database)
	if err != nil || version != 4 {
		t.Fatalf("schema version after rollback = (%d, %v), want (4, nil)", version, err)
	}
}

func assertSQLiteObject(t *testing.T, db *sql.DB, objectType, name string) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`, objectType, name).Scan(&count); err != nil {
		t.Fatalf("query sqlite object %s %q: %v", objectType, name, err)
	}
	if count != 1 {
		t.Fatalf("sqlite object %s %q count = %d, want 1", objectType, name, count)
	}
}
func tableHasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func tableHasExtendedColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_xinfo(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_xinfo(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey, hidden int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(
			&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey, &hidden,
		); err != nil {
			t.Fatalf("scan table_xinfo(%s): %v", table, err)
		}
		if name == column {
			return true
		}
	}
	return false
}

func assertPragmaInt(t *testing.T, db *sql.DB, name string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`PRAGMA ` + name).Scan(&got); err != nil {
		t.Fatalf("read PRAGMA %s: %v", name, err)
	}
	if got != want {
		t.Fatalf("PRAGMA %s = %d, want %d", name, got, want)
	}
}

func mustUUIDv7(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7() error = %v", err)
	}
	return id.String()
}
