package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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
	for _, index := range []string{"idx_live_sessions_active_room", "idx_live_sessions_operation_id", "idx_live_sessions_manifest_dirty"} {
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
	if got := store.SchemaVersion(); got != 2 {
		t.Fatalf("SchemaVersion() = %d, want 2", got)
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
	if got := store.SchemaVersion(); got != 2 {
		t.Fatalf("SchemaVersion() = %d, want 2", got)
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
