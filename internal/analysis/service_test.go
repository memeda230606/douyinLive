package analysis

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

func TestServicePersistsVersionedBucketsReusesFingerprintAndKeepsPrivacyBoundary(t *testing.T) {
	ctx := context.Background()
	layout, err := storage.PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	defer store.Close()
	fixedNow := time.UnixMilli(9_000).UTC()
	service, err := NewServiceWithOptions(store.Writer(), store.Reader(), ServiceOptions{Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	roomID := mustUUIDv7(t)
	sessionID := mustUUIDv7(t)
	if _, err := store.Writer().Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at)
		VALUES (?, 'private-live-id', 'visible room', 1, 1)`, roomID); err != nil {
		t.Fatalf("insert room: %v", err)
	}
	if _, err := store.Writer().Exec(`INSERT INTO live_sessions(
		id, room_config_id, platform_room_id, title, status, recording_status,
		operation_id, manifest_dirty, started_at, ended_at, capture_offset_ms,
		clock_source, integrity_score, data_path, schema_version, created_at, updated_at
	) VALUES (?, ?, 'private-platform-room', 'visible session', 'completed', 'completed',
		'private-operation', 0, 1000, 71000, 0, 'received', 1,
		'private/data/path', 1, 1, 1)`, sessionID, roomID); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	insertAnalysisEvent(t, store.Writer(), sessionID, mustUUIDv7(t), 1, 1_000, "chat", "private-user-hash")
	first, err := service.AnalyzeSession(ctx, AnalyzeRequest{SessionID: sessionID})
	if err != nil {
		t.Fatalf("AnalyzeSession() error = %v", err)
	}
	if first.Version != ContractVersion || first.Status != "completed" || len(first.Buckets) != 7 || first.Summary.Totals.ChatCount != 1 {
		t.Fatalf("first report = %+v", first)
	}
	reused, err := service.AnalyzeSession(ctx, AnalyzeRequest{SessionID: sessionID})
	if err != nil {
		t.Fatalf("AnalyzeSession() reuse error = %v", err)
	}
	if reused.ID != first.ID || reused.AnalysisVersion != first.AnalysisVersion {
		t.Fatalf("fingerprint reuse = (%s, %s), want (%s, %s)", reused.ID, reused.AnalysisVersion, first.ID, first.AnalysisVersion)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	for _, forbidden := range []string{"private-user-hash", "private-live-id", "private-platform-room", "private-operation", "private/data/path"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("analysis DTO leaked %q: %s", forbidden, encoded)
		}
	}

	insertAnalysisEvent(t, store.Writer(), sessionID, mustUUIDv7(t), 2, 21_000, "chat", "another-private-user")
	second, err := service.AnalyzeSession(ctx, AnalyzeRequest{SessionID: sessionID})
	if err != nil {
		t.Fatalf("AnalyzeSession() changed input error = %v", err)
	}
	if second.ID == first.ID || second.AnalysisVersion == first.AnalysisVersion || second.Summary.Totals.ChatCount != 2 {
		t.Fatalf("changed-input report = %+v, first = %+v", second, first)
	}
	var reportCount, versionCount, bucketCount int
	if err := store.Reader().QueryRow(`SELECT COUNT(*), COUNT(DISTINCT analysis_version)
		FROM analysis_reports WHERE session_id = ?`, sessionID).Scan(&reportCount, &versionCount); err != nil {
		t.Fatalf("count reports: %v", err)
	}
	if err := store.Reader().QueryRow(`SELECT COUNT(*) FROM metric_buckets WHERE session_id = ?`, sessionID).Scan(&bucketCount); err != nil {
		t.Fatalf("count buckets: %v", err)
	}
	if reportCount != 2 || versionCount != 2 || bucketCount != 14 {
		t.Fatalf("versioned rows reports=%d versions=%d buckets=%d", reportCount, versionCount, bucketCount)
	}
	latest, err := service.GetAnalysisReport(ctx, sessionID)
	if err != nil || latest.ID != second.ID {
		t.Fatalf("GetAnalysisReport() = (%s, %v), want %s", latest.ID, err, second.ID)
	}
}

func TestServiceRejectsActiveMissingAndMalformedSessions(t *testing.T) {
	ctx := context.Background()
	layout, err := storage.PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewService(store.Writer(), store.Reader())
	if err != nil {
		t.Fatal(err)
	}
	missingID := mustUUIDv7(t)
	if _, err := service.AnalyzeSession(ctx, AnalyzeRequest{SessionID: missingID}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing session error = %v", err)
	}
	if _, err := service.AnalyzeSession(ctx, AnalyzeRequest{SessionID: "not-a-v7"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("malformed session error = %v", err)
	}
	roomID := mustUUIDv7(t)
	activeID := mustUUIDv7(t)
	if _, err := store.Writer().Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at)
		VALUES (?, 'active-live', 'active', 1, 1)`, roomID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Writer().Exec(`INSERT INTO live_sessions(
		id, room_config_id, title, status, recording_status, operation_id,
		manifest_dirty, started_at, capture_offset_ms, clock_source,
		integrity_score, data_path, schema_version, created_at, updated_at
	) VALUES (?, ?, 'active', 'recording', 'recording', 'active-operation',
		0, 1000, 0, 'received', 1, 'active/data', 1, 1, 1)`, activeID, roomID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AnalyzeSession(ctx, AnalyzeRequest{SessionID: activeID}); !errors.Is(err, ErrAnalysisNotReady) {
		t.Fatalf("active session error = %v", err)
	}
	if _, err := service.GetAnalysisReport(ctx, activeID); !errors.Is(err, ErrReportNotFound) {
		t.Fatalf("missing report error = %v", err)
	}
}

func insertAnalysisEvent(t *testing.T, writer interface {
	Exec(string, ...any) (sql.Result, error)
}, sessionID, eventID string, sequence, offset int64, kind, userHash string) {
	t.Helper()
	if _, err := writer.Exec(`INSERT INTO live_events(
		id, session_id, ingest_sequence, event_role, method, kind, dedupe_key,
		received_at, session_offset_ms, clock_confidence, user_hash,
		normalized_json, parse_status, normalizer_version
	) VALUES (?, ?, ?, 'source', 'test-method', ?, ?, ?, ?, 1, ?, '{}', 'parsed', 'event-normalizer/v1')`,
		eventID, sessionID, sequence, kind, "private-dedupe-"+eventID, 1000+offset, offset, userHash,
	); err != nil {
		t.Fatalf("insert analysis event: %v", err)
	}
}

func mustUUIDv7(t *testing.T) string {
	t.Helper()
	value, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return value.String()
}
