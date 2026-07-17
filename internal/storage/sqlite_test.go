package storage

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
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
