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
	{
		Version: 3,
		Name:    "durable_event_ingest",
		Statements: []string{
			`ALTER TABLE live_events ADD COLUMN ingest_sequence INTEGER NOT NULL DEFAULT 0
				CHECK (ingest_sequence >= 0)`,
			`ALTER TABLE live_events ADD COLUMN event_role TEXT NOT NULL DEFAULT 'source'
				CHECK (event_role IN ('source', 'aggregate'))`,
			`ALTER TABLE live_events ADD COLUMN normalizer_version TEXT NOT NULL DEFAULT 'legacy-v1'
				CHECK (length(trim(normalizer_version)) > 0)`,
			`ALTER TABLE live_events ADD COLUMN parse_error_code TEXT
				CHECK (parse_error_code IS NULL OR length(trim(parse_error_code)) > 0)`,
			`CREATE INDEX idx_live_events_session_ingest
				ON live_events(session_id, ingest_sequence, event_role)`,
			`CREATE UNIQUE INDEX idx_live_events_source_sequence
				ON live_events(session_id, ingest_sequence)
				WHERE event_role = 'source' AND ingest_sequence > 0`,
			`CREATE INDEX idx_live_events_role_offset
				ON live_events(session_id, event_role, session_offset_ms)`,
			`CREATE UNIQUE INDEX idx_live_events_session_id
				ON live_events(session_id, id)`,
			`CREATE TABLE event_ingest_checkpoints (
				session_id TEXT PRIMARY KEY REFERENCES live_sessions(id) ON DELETE CASCADE,
				committed_sequence INTEGER NOT NULL DEFAULT 0 CHECK (committed_sequence >= 0),
				state TEXT NOT NULL CHECK (state IN ('open', 'closing', 'closed', 'degraded')),
				privacy_key_id TEXT NOT NULL CHECK (length(trim(privacy_key_id)) > 0),
				spool_file TEXT NOT NULL DEFAULT '',
				spool_offset INTEGER NOT NULL DEFAULT 0 CHECK (spool_offset >= 0),
				raw_file TEXT NOT NULL DEFAULT '',
				raw_offset INTEGER NOT NULL DEFAULT 0 CHECK (raw_offset >= 0),
				updated_at INTEGER NOT NULL,
				CHECK (
					(committed_sequence = 0 AND spool_file = '' AND spool_offset = 0 AND raw_file = '' AND raw_offset = 0)
					OR
					(committed_sequence > 0 AND length(trim(spool_file)) > 0 AND spool_offset > 0
						AND length(trim(raw_file)) > 0 AND raw_offset > 0)
				)
			)`,
			`CREATE INDEX idx_event_ingest_checkpoints_sequence
				ON event_ingest_checkpoints(committed_sequence, session_id)`,
			`CREATE TRIGGER trg_event_checkpoint_privacy_key_immutable
				BEFORE UPDATE ON event_ingest_checkpoints
				WHEN NEW.privacy_key_id IS NOT OLD.privacy_key_id
				BEGIN
					SELECT RAISE(ABORT, 'event checkpoint privacy key is immutable');
				END`,
			`CREATE TRIGGER trg_event_checkpoint_state_transition
				BEFORE UPDATE ON event_ingest_checkpoints
				WHEN NOT (
					(OLD.state = 'open' AND NEW.state IN ('open', 'closing', 'degraded')) OR
					(OLD.state = 'closing' AND NEW.state IN ('closing', 'closed', 'degraded')) OR
					(OLD.state = 'degraded' AND NEW.state IN ('degraded', 'open', 'closing', 'closed')) OR
					(OLD.state = 'closed' AND NEW.state = 'closed')
				)
				BEGIN
					SELECT RAISE(ABORT, 'event checkpoint state transition is invalid');
				END`,
			`CREATE TRIGGER trg_event_checkpoint_closed_immutable
				BEFORE UPDATE ON event_ingest_checkpoints
				WHEN OLD.state = 'closed' AND (
					NEW.committed_sequence IS NOT OLD.committed_sequence OR
					NEW.spool_file IS NOT OLD.spool_file OR
					NEW.spool_offset IS NOT OLD.spool_offset OR
					NEW.raw_file IS NOT OLD.raw_file OR
					NEW.raw_offset IS NOT OLD.raw_offset OR
					NEW.updated_at IS NOT OLD.updated_at
				)
				BEGIN
					SELECT RAISE(ABORT, 'event checkpoint is already closed');
				END`,
			`CREATE TABLE gift_combo_states (
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				combo_key TEXT NOT NULL CHECK (length(trim(combo_key)) > 0),
				status TEXT NOT NULL CHECK (status IN ('open', 'closed')),
				user_hash TEXT,
				gift_id TEXT NOT NULL CHECK (length(trim(gift_id)) > 0),
				gift_name TEXT,
				total_count INTEGER NOT NULL CHECK (total_count > 0),
				total_value REAL CHECK (total_value IS NULL OR total_value >= 0),
				first_ingest_sequence INTEGER NOT NULL CHECK (first_ingest_sequence > 0),
				last_ingest_sequence INTEGER NOT NULL CHECK (last_ingest_sequence >= first_ingest_sequence),
				started_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL CHECK (updated_at >= started_at),
				closed_at INTEGER,
				aggregate_event_id TEXT,
				normalizer_version TEXT NOT NULL CHECK (length(trim(normalizer_version)) > 0),
				PRIMARY KEY(session_id, combo_key),
				FOREIGN KEY(session_id, aggregate_event_id)
					REFERENCES live_events(session_id, id) ON DELETE RESTRICT,
				CHECK (
					(status = 'open' AND closed_at IS NULL AND aggregate_event_id IS NULL)
					OR
					(status = 'closed' AND closed_at IS NOT NULL AND closed_at >= updated_at AND aggregate_event_id IS NOT NULL)
				)
			)`,
			`CREATE INDEX idx_gift_combo_states_status_updated
				ON gift_combo_states(session_id, status, updated_at)`,
			`CREATE INDEX idx_gift_combo_states_last_sequence
				ON gift_combo_states(session_id, last_ingest_sequence)`,
			`CREATE UNIQUE INDEX idx_gift_combo_states_aggregate
				ON gift_combo_states(aggregate_event_id) WHERE aggregate_event_id IS NOT NULL`,
			`CREATE TRIGGER trg_gift_combo_closed_immutable
				BEFORE UPDATE ON gift_combo_states
				WHEN OLD.status = 'closed' AND (
					NEW.session_id IS NOT OLD.session_id OR
					NEW.combo_key IS NOT OLD.combo_key OR
					NEW.status IS NOT OLD.status OR
					NEW.user_hash IS NOT OLD.user_hash OR
					NEW.gift_id IS NOT OLD.gift_id OR
					NEW.gift_name IS NOT OLD.gift_name OR
					NEW.total_count IS NOT OLD.total_count OR
					NEW.total_value IS NOT OLD.total_value OR
					NEW.first_ingest_sequence IS NOT OLD.first_ingest_sequence OR
					NEW.last_ingest_sequence IS NOT OLD.last_ingest_sequence OR
					NEW.started_at IS NOT OLD.started_at OR
					NEW.updated_at IS NOT OLD.updated_at OR
					NEW.closed_at IS NOT OLD.closed_at OR
					NEW.aggregate_event_id IS NOT OLD.aggregate_event_id OR
					NEW.normalizer_version IS NOT OLD.normalizer_version
				)
				BEGIN
					SELECT RAISE(ABORT, 'gift combo is already closed');
				END`,
			`ALTER TABLE capture_gaps RENAME TO capture_gaps_v2`,
			`DROP INDEX idx_capture_gaps_session_start`,
			`CREATE TABLE capture_gaps (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				media_segment_id TEXT REFERENCES media_segments(id) ON DELETE SET NULL,
				kind TEXT NOT NULL CHECK (kind IN ('message_disconnect', 'recording_restart', 'stream_unavailable', 'disk_full', 'process_crash', 'clock_uncertain', 'event_persistence')),
				started_at INTEGER NOT NULL,
				ended_at INTEGER,
				start_offset_ms INTEGER NOT NULL,
				end_offset_ms INTEGER,
				severity TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'error')),
				recovered INTEGER NOT NULL DEFAULT 0 CHECK (recovered IN (0, 1)),
				reason_code TEXT NOT NULL,
				details_json TEXT NOT NULL DEFAULT '{}',
				dedupe_key TEXT NOT NULL CHECK (length(trim(dedupe_key)) > 0),
				UNIQUE(session_id, dedupe_key)
			)`,
			`INSERT INTO capture_gaps(
				id, session_id, media_segment_id, kind, started_at, ended_at,
				start_offset_ms, end_offset_ms, severity, recovered, reason_code,
				details_json, dedupe_key
			)
			SELECT id, session_id, media_segment_id, kind, started_at, ended_at,
				start_offset_ms, end_offset_ms, severity, recovered, reason_code,
				details_json, CASE WHEN id IS NULL
					THEN 'legacy-rowid:' || rowid
					ELSE 'legacy:' || id END
			FROM capture_gaps_v2`,
			`DROP TABLE capture_gaps_v2`,
			`CREATE INDEX idx_capture_gaps_session_start
				ON capture_gaps(session_id, start_offset_ms)`,
			`CREATE INDEX idx_capture_gaps_kind_recovered
				ON capture_gaps(session_id, kind, recovered, start_offset_ms)`,
		},
	},
	{
		Version: 4,
		Name:    "media_manifest_and_recording_roots",
		Statements: []string{
			`CREATE TABLE recording_roots (
				id TEXT PRIMARY KEY,
				absolute_path TEXT NOT NULL CHECK (length(trim(absolute_path)) > 0),
				canonical_key TEXT NOT NULL UNIQUE
					CHECK (length(canonical_key) = 64 AND canonical_key NOT GLOB '*[^0-9a-f]*'),
				volume_identity TEXT NOT NULL
					CHECK (length(volume_identity) = 64 AND volume_identity NOT GLOB '*[^0-9a-f]*'),
				status TEXT NOT NULL CHECK (status IN ('ready')),
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL CHECK (updated_at >= created_at),
				last_verified_at INTEGER NOT NULL CHECK (last_verified_at >= created_at)
			)`,
			`CREATE INDEX idx_recording_roots_status_verified
				ON recording_roots(status, last_verified_at, id)`,
			`CREATE TABLE session_media (
				session_id TEXT PRIMARY KEY REFERENCES live_sessions(id) ON DELETE CASCADE,
				root_id TEXT REFERENCES recording_roots(id) ON DELETE RESTRICT,
				relative_path TEXT NOT NULL CHECK (length(trim(relative_path)) > 0),
				relative_key TEXT GENERATED ALWAYS AS (lower(relative_path)) STORED,
				state TEXT NOT NULL CHECK (state IN ('open', 'finalizing', 'completed', 'incomplete')),
				manifest_revision INTEGER NOT NULL DEFAULT 0 CHECK (manifest_revision >= 0),
				manifest_dirty INTEGER NOT NULL DEFAULT 1 CHECK (manifest_dirty IN (0, 1)),
				media_epoch_at INTEGER,
				attempts_json TEXT NOT NULL DEFAULT '[]',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL CHECK (updated_at >= created_at)
			)`,
			`CREATE UNIQUE INDEX idx_session_media_internal_path
				ON session_media(relative_key) WHERE root_id IS NULL`,
			`CREATE UNIQUE INDEX idx_session_media_external_path
				ON session_media(root_id, relative_key) WHERE root_id IS NOT NULL`,
			`CREATE INDEX idx_session_media_dirty
				ON session_media(manifest_dirty, state, session_id)`,
			`CREATE TRIGGER trg_session_media_location_immutable
				BEFORE UPDATE ON session_media
				WHEN NEW.root_id IS NOT OLD.root_id OR NEW.relative_path IS NOT OLD.relative_path
				BEGIN
					SELECT RAISE(ABORT, 'session media location is immutable');
				END`,
			`ALTER TABLE media_segments ADD COLUMN attempt_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE media_segments ADD COLUMN attempt_sequence INTEGER
				CHECK ((attempt_id = '' AND attempt_sequence IS NULL)
					OR (length(trim(attempt_id)) > 0 AND attempt_sequence > 0))`,
			`ALTER TABLE media_segments ADD COLUMN source_relative_path TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE media_segments ADD COLUMN probe_version TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE media_segments ADD COLUMN error_code TEXT
				CHECK (error_code IS NULL OR length(trim(error_code)) > 0)`,
			`CREATE UNIQUE INDEX idx_media_segments_attempt_sequence
				ON media_segments(session_id, attempt_id, attempt_sequence)
				WHERE attempt_id <> ''`,
			`CREATE UNIQUE INDEX idx_media_segments_relative_path_key
				ON media_segments(session_id, lower(relative_path))`,
			`CREATE UNIQUE INDEX idx_media_segments_source_path
				ON media_segments(session_id, lower(source_relative_path))
				WHERE source_relative_path <> ''`,
			`CREATE UNIQUE INDEX idx_media_segments_session_id
				ON media_segments(session_id, id)`,
			`CREATE TABLE media_artifacts (
				id TEXT PRIMARY KEY,
				session_id TEXT NOT NULL REFERENCES live_sessions(id) ON DELETE CASCADE,
				media_segment_id TEXT NOT NULL,
				kind TEXT NOT NULL CHECK (kind IN ('asr_wav', 'playback_mp4')),
				relative_path TEXT NOT NULL CHECK (length(trim(relative_path)) > 0),
				container TEXT NOT NULL DEFAULT '',
				codec TEXT NOT NULL DEFAULT '',
				duration_ms INTEGER NOT NULL DEFAULT 0 CHECK (duration_ms >= 0),
				size_bytes INTEGER NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
				sample_rate INTEGER NOT NULL DEFAULT 0 CHECK (sample_rate >= 0),
				channels INTEGER NOT NULL DEFAULT 0 CHECK (channels >= 0),
				sha256 TEXT NOT NULL DEFAULT '' CHECK (length(sha256) IN (0, 64)),
				source_sha256 TEXT NOT NULL DEFAULT '' CHECK (length(source_sha256) IN (0, 64)),
				status TEXT NOT NULL
					CHECK (status IN ('pending', 'pending_transcode', 'complete', 'failed', 'missing', 'not_applicable')),
				error_code TEXT CHECK (error_code IS NULL OR length(trim(error_code)) > 0),
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL CHECK (updated_at >= created_at),
				UNIQUE(media_segment_id, kind),
				UNIQUE(session_id, relative_path),
				FOREIGN KEY(session_id, media_segment_id)
					REFERENCES media_segments(session_id, id) ON DELETE CASCADE
			)`,
			`CREATE INDEX idx_media_artifacts_session_status
				ON media_artifacts(session_id, status, kind, media_segment_id)`,
			`CREATE UNIQUE INDEX idx_media_artifacts_relative_path_key
				ON media_artifacts(session_id, lower(relative_path))`,
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
