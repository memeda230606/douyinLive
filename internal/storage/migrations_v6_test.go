package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSchemaV5UpgradeCreatesV6PlaybackFoundationAndBackup(t *testing.T) {
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
	if _, err := applyMigrationSet(ctx, database, schemaMigrations[:5], time.Unix(1_700_000_000, 0)); err != nil {
		database.Close()
		t.Fatalf("apply schema v5: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at)
		VALUES ('room-v5', 'live-v5', 'legacy', 1, 1)`); err != nil {
		database.Close()
		t.Fatalf("insert v5 room: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO live_sessions(
		id, room_config_id, status, started_at, clock_source, data_path,
		schema_version, created_at, updated_at
	) VALUES ('session-v5', 'room-v5', 'completed', 1, 'received', 'rooms/v5', 1, 1, 1)`); err != nil {
		database.Close()
		t.Fatalf("insert v5 session: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO metric_buckets(
		session_id, bucket_start_ms, bucket_size_ms, chat_count, unique_chatters,
		like_delta, gift_count, gift_value, follow_count, enter_count, active_users,
		message_total, speech_ms, silence_ms, words_per_minute, sentiment_score,
		analysis_version, completeness
	) VALUES ('session-v5', 10000, 10000, 3, 2, 4, 1, 2.5, 1, 5, 6,
		20, 7000, 3000, 120.5, 0.25, 'analysis-v1', 0.9)`); err != nil {
		database.Close()
		t.Fatalf("insert v5 metric bucket: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("close schema v5 database: %v", err)
	}

	upgradeAt := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	store, err := Open(ctx, layout, OpenOptions{Now: upgradeAt, CreateBackups: true})
	if err != nil {
		t.Fatalf("Open() schema v6 upgrade error = %v", err)
	}
	defer store.Close()
	if got := store.SchemaVersion(); got != 6 {
		t.Fatalf("SchemaVersion() = %d, want 6", got)
	}
	for _, index := range []string{
		"idx_live_sessions_playback_page",
		"idx_live_events_playback_page",
		"idx_capture_gaps_playback_page",
	} {
		assertSQLiteObject(t, store.Reader(), "index", index)
	}

	var chatCount, uniqueChatters, messageTotal int64
	var giftValue, wordsPerMinute, sentiment, completeness float64
	if err := store.Reader().QueryRow(`SELECT chat_count, unique_chatters, message_total,
		gift_value, words_per_minute, sentiment_score, completeness
		FROM metric_buckets
		WHERE session_id = 'session-v5' AND analysis_version = 'analysis-v1'
			AND bucket_start_ms = 10000 AND bucket_size_ms = 10000`).Scan(
		&chatCount, &uniqueChatters, &messageTotal, &giftValue,
		&wordsPerMinute, &sentiment, &completeness,
	); err != nil {
		t.Fatalf("read upgraded metric bucket: %v", err)
	}
	if chatCount != 3 || uniqueChatters != 2 || messageTotal != 20 ||
		giftValue != 2.5 || wordsPerMinute != 120.5 || sentiment != 0.25 || completeness != 0.9 {
		t.Fatal("upgraded metric bucket values changed")
	}
	if _, err := store.Writer().Exec(`INSERT INTO metric_buckets(
		session_id, bucket_start_ms, bucket_size_ms, analysis_version, completeness
	) VALUES ('session-v5', 10000, 10000, 'analysis-v2', 1)`); err != nil {
		t.Fatalf("insert second analysis version: %v", err)
	}
	if _, err := store.Writer().Exec(`INSERT INTO metric_buckets(
		session_id, bucket_start_ms, bucket_size_ms, analysis_version, completeness
	) VALUES ('session-v5', 10000, 10000, 'analysis-v2', 1)`); err == nil {
		t.Fatal("duplicate metric bucket identity succeeded")
	}

	backups, err := filepath.Glob(filepath.Join(layout.BackupsDir, "app-v5-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("v5 backup files = (%v, %v), want one", backups, err)
	}
	backup, err := sql.Open("sqlite", sqliteDSN(backups[0], true))
	if err != nil {
		t.Fatalf("open v5 backup: %v", err)
	}
	defer backup.Close()
	version, err := currentSchemaVersion(ctx, backup)
	if err != nil || version != 5 {
		t.Fatalf("backup schema version = (%d, %v), want (5, nil)", version, err)
	}
	if got := primaryKeyColumnCount(t, backup, "metric_buckets"); got != 3 {
		t.Fatalf("v5 metric bucket primary key columns = %d, want 3", got)
	}
}

func TestSchemaV6FailureRollsBackMetricBucketsIndexesAndVersion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v6-rollback.db")
	database, err := sql.Open("sqlite", sqliteDSN(path, false))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if _, err := applyMigrationSet(ctx, database, schemaMigrations[:5], time.Now()); err != nil {
		t.Fatalf("apply schema v5: %v", err)
	}
	broken := schemaMigrations[5]
	broken.Statements = append([]string(nil), broken.Statements[:5]...)
	broken.Statements = append(broken.Statements, "THIS IS NOT SQL")
	if _, err := applyMigrationSet(ctx, database, []migration{broken}, time.Now()); err == nil {
		t.Fatal("broken schema v6 migration succeeded")
	}
	assertSQLiteObject(t, database, "table", "metric_buckets")
	var legacyTableCount int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'metric_buckets_v5'`).Scan(&legacyTableCount); err != nil {
		t.Fatalf("inspect rolled-back metric bucket table: %v", err)
	}
	if legacyTableCount != 0 {
		t.Fatal("failed schema v6 migration left metric_buckets_v5 behind")
	}
	for _, index := range []string{
		"idx_live_sessions_playback_page",
		"idx_live_events_playback_page",
		"idx_capture_gaps_playback_page",
	} {
		var count int
		if err := database.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", index,
		).Scan(&count); err != nil {
			t.Fatalf("inspect rolled-back index %q: %v", index, err)
		}
		if count != 0 {
			t.Fatalf("failed schema v6 migration left index %q", index)
		}
	}
	version, err := currentSchemaVersion(ctx, database)
	if err != nil || version != 5 {
		t.Fatalf("schema version after rollback = (%d, %v), want (5, nil)", version, err)
	}
	if got := primaryKeyColumnCount(t, database, "metric_buckets"); got != 3 {
		t.Fatalf("rolled-back primary key columns = %d, want 3", got)
	}
}

func primaryKeyColumnCount(t *testing.T, database *sql.DB, table string) int {
	t.Helper()
	rows, err := database.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("inspect %s primary key: %v", table, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan %s primary key: %v", table, err)
		}
		if primaryKey > 0 {
			count++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s primary key: %v", table, err)
	}
	return count
}
