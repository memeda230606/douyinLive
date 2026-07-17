package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type migration struct {
	Version    int
	Name       string
	Statements []string
}

var schemaMigrations = []migration{
	{
		Version: 1,
		Name:    "initial_capture_schema",
		Statements: []string{
			`CREATE TABLE rooms (
				id TEXT PRIMARY KEY,
				live_id TEXT NOT NULL UNIQUE,
				room_id TEXT,
				alias TEXT NOT NULL,
				anchor_name TEXT,
				monitor_enabled INTEGER NOT NULL DEFAULT 0 CHECK (monitor_enabled IN (0, 1)),
				record_enabled INTEGER NOT NULL DEFAULT 0 CHECK (record_enabled IN (0, 1)),
				recording_profile_json TEXT NOT NULL DEFAULT '{}',
				credential_ref TEXT,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE INDEX idx_rooms_monitor_enabled ON rooms(monitor_enabled)`,
			`CREATE TABLE live_sessions (
				id TEXT PRIMARY KEY,
				room_config_id TEXT NOT NULL REFERENCES rooms(id) ON DELETE RESTRICT,
				platform_room_id TEXT,
				title TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL CHECK (status IN ('starting', 'recording', 'finalizing', 'completed', 'interrupted', 'failed')),
				started_at INTEGER NOT NULL,
				ended_at INTEGER,
				media_epoch_at INTEGER,
				capture_offset_ms INTEGER NOT NULL DEFAULT 0,
				clock_source TEXT NOT NULL CHECK (clock_source IN ('media', 'received', 'calibrated')),
				integrity_score REAL NOT NULL DEFAULT 1 CHECK (integrity_score >= 0 AND integrity_score <= 1),
				data_path TEXT NOT NULL,
				schema_version INTEGER NOT NULL,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE INDEX idx_live_sessions_room_started ON live_sessions(room_config_id, started_at DESC)`,
			`CREATE INDEX idx_live_sessions_status ON live_sessions(status)`,
			`CREATE TABLE live_events (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				method TEXT NOT NULL,
				kind TEXT NOT NULL CHECK (kind IN ('chat', 'gift', 'like', 'member', 'follow', 'system', 'unknown')),
				platform_message_id TEXT,
				dedupe_key TEXT NOT NULL,
				message_create_at INTEGER,
				received_at INTEGER NOT NULL,
				session_offset_ms INTEGER NOT NULL,
				clock_confidence REAL NOT NULL CHECK (clock_confidence >= 0 AND clock_confidence <= 1),
				user_hash TEXT,
				display_name TEXT,
				content TEXT,
				numeric_value REAL,
				normalized_json TEXT NOT NULL DEFAULT '{}',
				raw_file TEXT,
				raw_offset INTEGER,
				raw_length INTEGER,
				parse_status TEXT NOT NULL CHECK (parse_status IN ('parsed', 'unknown', 'failed')),
				UNIQUE(session_id, dedupe_key)
			)`,
			`CREATE INDEX idx_live_events_session_offset ON live_events(session_id, session_offset_ms)`,
			`CREATE INDEX idx_live_events_kind_offset ON live_events(session_id, kind, session_offset_ms)`,
			`CREATE INDEX idx_live_events_user_hash ON live_events(user_hash)`,
			`CREATE TABLE media_segments (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				sequence INTEGER NOT NULL CHECK (sequence > 0),
				relative_path TEXT NOT NULL,
				container TEXT NOT NULL,
				video_codec TEXT,
				audio_codec TEXT,
				started_at INTEGER NOT NULL,
				ended_at INTEGER NOT NULL,
				pts_start_ms INTEGER,
				pts_end_ms INTEGER,
				duration_ms INTEGER NOT NULL CHECK (duration_ms >= 0),
				size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
				sha256 TEXT,
				status TEXT NOT NULL CHECK (status IN ('partial', 'complete', 'recovered', 'corrupt', 'missing')),
				UNIQUE(session_id, sequence),
				UNIQUE(session_id, relative_path)
			)`,
			`CREATE TABLE transcript_segments (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				media_segment_id TEXT REFERENCES media_segments(id) ON DELETE SET NULL,
				start_ms INTEGER NOT NULL,
				end_ms INTEGER NOT NULL,
				text TEXT NOT NULL,
				confidence REAL,
				speaker TEXT,
				provider TEXT NOT NULL,
				model TEXT NOT NULL,
				language TEXT NOT NULL,
				analysis_version TEXT NOT NULL,
				source_audio_sha256 TEXT NOT NULL,
				CHECK (end_ms >= start_ms)
			)`,
			`CREATE INDEX idx_transcript_segments_session_start ON transcript_segments(session_id, start_ms)`,
			`CREATE TABLE metric_buckets (
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				bucket_start_ms INTEGER NOT NULL,
				bucket_size_ms INTEGER NOT NULL CHECK (bucket_size_ms > 0),
				chat_count INTEGER NOT NULL DEFAULT 0,
				unique_chatters INTEGER NOT NULL DEFAULT 0,
				like_delta INTEGER NOT NULL DEFAULT 0,
				gift_count INTEGER NOT NULL DEFAULT 0,
				gift_value REAL,
				follow_count INTEGER NOT NULL DEFAULT 0,
				enter_count INTEGER NOT NULL DEFAULT 0,
				active_users INTEGER NOT NULL DEFAULT 0,
				message_total INTEGER NOT NULL DEFAULT 0,
				speech_ms INTEGER,
				silence_ms INTEGER,
				words_per_minute REAL,
				sentiment_score REAL,
				analysis_version TEXT NOT NULL,
				completeness REAL NOT NULL CHECK (completeness >= 0 AND completeness <= 1),
				PRIMARY KEY(session_id, bucket_start_ms, bucket_size_ms)
			)`,
			`CREATE TABLE analysis_reports (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				status TEXT NOT NULL CHECK (status IN ('pending', 'running', 'partial', 'completed', 'failed', 'cancelled')),
				analysis_version TEXT NOT NULL,
				input_fingerprint TEXT NOT NULL,
				summary_json TEXT NOT NULL DEFAULT '{}',
				highlights_json TEXT NOT NULL DEFAULT '[]',
				topics_json TEXT NOT NULL DEFAULT '[]',
				questions_json TEXT NOT NULL DEFAULT '[]',
				started_at INTEGER,
				completed_at INTEGER,
				error_code TEXT,
				error_summary TEXT
			)`,
			`CREATE INDEX idx_analysis_reports_session ON analysis_reports(session_id, status)`,
			`CREATE INDEX idx_analysis_reports_fingerprint ON analysis_reports(input_fingerprint, status)`,
			`CREATE TABLE capture_gaps (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				media_segment_id TEXT REFERENCES media_segments(id) ON DELETE SET NULL,
				kind TEXT NOT NULL CHECK (kind IN ('message_disconnect', 'recording_restart', 'stream_unavailable', 'disk_full', 'process_crash', 'clock_uncertain')),
				started_at INTEGER NOT NULL,
				ended_at INTEGER,
				start_offset_ms INTEGER NOT NULL,
				end_offset_ms INTEGER,
				severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error')),
				recovered INTEGER NOT NULL DEFAULT 0 CHECK (recovered IN (0, 1)),
				reason_code TEXT NOT NULL,
				details_json TEXT NOT NULL DEFAULT '{}'
			)`,
			`CREATE INDEX idx_capture_gaps_session_start ON capture_gaps(session_id, start_offset_ms)`,
		},
	},
	{
		Version: 2,
		Name:    "separate_recording_status_and_session_operations",
		Statements: []string{
			`ALTER TABLE live_sessions ADD COLUMN operation_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE live_sessions ADD COLUMN manifest_dirty INTEGER NOT NULL DEFAULT 1
				CHECK (manifest_dirty IN (0, 1))`,
			`ALTER TABLE live_sessions ADD COLUMN recording_status TEXT NOT NULL DEFAULT 'pending'
				CHECK (recording_status IN ('pending', 'disabled', 'starting', 'recording', 'unavailable', 'reconnecting', 'finalizing', 'completed', 'incomplete', 'failed'))`,
			`UPDATE live_sessions SET recording_status = CASE status
				WHEN 'starting' THEN 'starting'
				WHEN 'recording' THEN 'recording'
				WHEN 'finalizing' THEN 'finalizing'
				WHEN 'completed' THEN 'completed'
				WHEN 'interrupted' THEN 'incomplete'
				WHEN 'failed' THEN 'failed'
				ELSE 'pending'
			END`,
			`CREATE UNIQUE INDEX idx_live_sessions_active_room
				ON live_sessions(room_config_id)
				WHERE status IN ('starting', 'recording', 'finalizing')`,
			`CREATE UNIQUE INDEX idx_live_sessions_operation_id
				ON live_sessions(operation_id) WHERE operation_id <> ''`,
			`CREATE INDEX idx_live_sessions_manifest_dirty
				ON live_sessions(manifest_dirty, id)`,
		},
	},
}

const createMigrationTableSQL = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	applied_at INTEGER NOT NULL
)`

func latestSchemaVersion(migrations []migration) int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].Version
}

func ensureMigrationTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, createMigrationTableSQL); err != nil {
		return fmt.Errorf("create schema migration table: %w", err)
	}
	return nil
}

func currentSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return version, nil
}

func applyMigrationSet(ctx context.Context, db *sql.DB, migrations []migration, now time.Time) (int, error) {
	if err := ensureMigrationTable(ctx, db); err != nil {
		return 0, err
	}
	current, err := currentSchemaVersion(ctx, db)
	if err != nil {
		return 0, err
	}
	latest := latestSchemaVersion(migrations)
	if current > latest {
		return 0, fmt.Errorf("database schema version %d is newer than supported version %d", current, latest)
	}

	previous := 0
	for _, item := range migrations {
		if item.Version <= previous {
			return 0, fmt.Errorf("migration versions are not strictly increasing at %d", item.Version)
		}
		previous = item.Version
		if item.Version <= current {
			continue
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin migration %d: %w", item.Version, err)
		}
		for _, statement := range item.Statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return 0, fmt.Errorf("apply migration %d (%s): %w", item.Version, item.Name, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, name, applied_at) VALUES (?, ?, ?)`,
			item.Version, item.Name, now.UTC().UnixMilli(),
		); err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("record migration %d: %w", item.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit migration %d: %w", item.Version, err)
		}
		current = item.Version
	}
	return current, nil
}
