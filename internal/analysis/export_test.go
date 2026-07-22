package analysis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

type exportFixture struct {
	service   *Service
	layout    storage.Layout
	sessionID string
	report    ReportDTO
}

func newExportFixture(t *testing.T, beforePublish func() error) exportFixture {
	t.Helper()
	ctx := context.Background()
	layout, err := storage.PrepareLayout(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fixedNow := time.UnixMilli(1_700_000_100_123).UTC()
	service, err := NewServiceWithOptions(store.Writer(), store.Reader(), ServiceOptions{
		Now: func() time.Time { return fixedNow }, ExportRoot: layout.ExportsDir,
		beforeExportPublish: beforePublish,
	})
	if err != nil {
		t.Fatal(err)
	}
	roomID := mustUUIDv7(t)
	sessionID := mustUUIDv7(t)
	if _, err := store.Writer().Exec(`INSERT INTO rooms(id, live_id, alias, created_at, updated_at)
		VALUES (?, 'private-live-id-export', 'private-room-alias-export', 1, 1)`, roomID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Writer().Exec(`INSERT INTO live_sessions(
		id, room_config_id, platform_room_id, title, status, recording_status,
		operation_id, manifest_dirty, started_at, ended_at, capture_offset_ms,
		clock_source, integrity_score, data_path, schema_version, created_at, updated_at
	) VALUES (?, ?, 'private-platform-room-export', 'private-title-export', 'completed', 'completed',
		'private-operation-export', 0, 1700000000000, 1700000020000, 0, 'received', 0.9,
		'private/data/path/export', 1, 1, 1)`, sessionID, roomID); err != nil {
		t.Fatal(err)
	}
	eventID := mustUUIDv7(t)
	if _, err := store.Writer().Exec(`INSERT INTO live_events(
		id, session_id, ingest_sequence, event_role, method, kind, platform_message_id,
		dedupe_key, received_at, session_offset_ms, clock_confidence, user_hash,
		display_name, content, numeric_value, normalized_json, raw_file, raw_offset,
		raw_length, parse_status, normalizer_version
	) VALUES (?, ?, 1, 'source', 'WebcastChatMessage', 'chat', 'private-platform-message-export',
		'private-dedupe-export', 1700000001000, 1000, 1, 'aabbccddeeff0011',
		'private-display-name-export', '=HYPERLINK("https://private.example")', 7,
		'{"private":"private-normalized-export"}', 'private/raw/file/export', 10, 20,
		'parsed', 'event-normalizer/v1')`, eventID, sessionID); err != nil {
		t.Fatal(err)
	}
	report, err := service.AnalyzeSession(ctx, AnalyzeRequest{SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	segmentID := mustUUIDv7(t)
	if _, err := store.Writer().Exec(`INSERT INTO media_segments(
		id, session_id, sequence, relative_path, container, video_codec, audio_codec,
		started_at, ended_at, duration_ms, size_bytes, sha256, status
	) VALUES (?, ?, 1, 'private/media/path/export.mp4', 'mp4', 'h264', 'aac',
		1700000001000, 1700000011000, 10000, 12345,
		'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'complete')`, segmentID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Writer().Exec(`INSERT INTO transcript_segments(
		id, session_id, media_segment_id, start_ms, end_ms, text, confidence, speaker,
		provider, model, language, analysis_version, source_audio_sha256
	) VALUES (?, ?, ?, 2000, 3000, '+SUM(1,2)', 0.75, 'private-speaker-export',
		'local-test', 'model-v1', 'zh-CN', ?,
		'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb')`,
		mustUUIDv7(t), sessionID, segmentID, report.AnalysisVersion); err != nil {
		t.Fatal(err)
	}
	return exportFixture{service: service, layout: layout, sessionID: sessionID, report: report}
}

func TestExportAnalysisWritesStableAtomicPrivacySafePackage(t *testing.T) {
	fixture := newExportFixture(t, nil)
	ctx := context.Background()
	first, err := fixture.service.ExportAnalysis(ctx, ExportRequest{SessionID: fixture.sessionID})
	if err != nil {
		t.Fatalf("ExportAnalysis() error = %v", err)
	}
	second, err := fixture.service.ExportAnalysis(ctx, ExportRequest{SessionID: fixture.sessionID})
	if err != nil {
		t.Fatalf("second ExportAnalysis() error = %v", err)
	}
	if first.Version != ExportContractVersion || first.ExportID == second.ExportID ||
		first.DirectoryName == second.DirectoryName || first.GeneratedAt != "2023-11-14T22:15:00.123Z" || first.IncludeText {
		t.Fatalf("export results = first %+v second %+v", first, second)
	}
	wantNames := []string{"events.csv", "metric-buckets.csv", "transcripts.csv", "media-segments.csv", "manifest.json"}
	if len(first.Files) != len(wantNames) {
		t.Fatalf("files = %+v", first.Files)
	}
	firstContent := make(map[string][]byte, len(wantNames))
	for index, name := range wantNames {
		metadata := first.Files[index]
		if metadata.Name != name || metadata.SizeBytes <= 0 || len(metadata.SHA256) != 64 {
			t.Fatalf("file[%d] = %+v", index, metadata)
		}
		content, readErr := os.ReadFile(filepath.Join(fixture.layout.ExportsDir, first.DirectoryName, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != metadata.SHA256 || int64(len(content)) != metadata.SizeBytes {
			t.Fatalf("metadata mismatch for %s", name)
		}
		if strings.HasSuffix(name, ".csv") && !strings.HasPrefix(string(content), "\ufeff") {
			t.Fatalf("%s has no UTF-8 BOM", name)
		}
		secondContent, readErr := os.ReadFile(filepath.Join(fixture.layout.ExportsDir, second.DirectoryName, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if string(content) != string(secondContent) {
			t.Fatalf("%s is not stable across repeated export", name)
		}
		firstContent[name] = content
	}
	combined := ""
	for _, content := range firstContent {
		combined += string(content)
	}
	for _, forbidden := range []string{
		"private-live-id-export", "private-room-alias-export", "private-platform-room-export",
		"private-title-export", "private-operation-export", "private/data/path/export",
		"private-platform-message-export", "private-dedupe-export", "private-display-name-export",
		"private-normalized-export", "private/raw/file/export", "private/media/path/export.mp4",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"HYPERLINK", "private-speaker-export", "+SUM",
	} {
		if strings.Contains(combined, forbidden) {
			t.Fatalf("strict export leaked %q", forbidden)
		}
	}
	var manifest exportManifest
	decoder := json.NewDecoder(strings.NewReader(string(firstContent["manifest.json"])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.Version != ExportContractVersion || manifest.Schema != ExportSchema ||
		manifest.Session.ID != fixture.sessionID || manifest.Report.ID != fixture.report.ID ||
		manifest.Privacy.TextIncluded || manifest.Privacy.UserIdentifier != "hmac-sha256" || len(manifest.Files) != 4 {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestExportAnalysisIncludesExplicitTextWithSpreadsheetInjectionGuard(t *testing.T) {
	fixture := newExportFixture(t, nil)
	result, err := fixture.service.ExportAnalysis(context.Background(), ExportRequest{
		SessionID: fixture.sessionID, IncludeText: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(fixture.layout.ExportsDir, result.DirectoryName, "events.csv"))
	if err != nil {
		t.Fatal(err)
	}
	transcripts, err := os.ReadFile(filepath.Join(fixture.layout.ExportsDir, result.DirectoryName, "transcripts.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(events), "'=HYPERLINK") || !strings.Contains(string(transcripts), "'+SUM") ||
		!strings.Contains(string(transcripts), "private-speaker-export") {
		t.Fatalf("text export did not preserve guarded text\nevents=%s\ntranscripts=%s", events, transcripts)
	}
	manifest, err := os.ReadFile(filepath.Join(fixture.layout.ExportsDir, result.DirectoryName, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"textIncluded": true`) {
		t.Fatalf("manifest does not record explicit text inclusion: %s", manifest)
	}
}

func TestExportAnalysisFailsClosedAndRemovesUnpublishedDirectory(t *testing.T) {
	fixture := newExportFixture(t, func() error { return errors.New("injected publish failure with private path") })
	_, err := fixture.service.ExportAnalysis(context.Background(), ExportRequest{SessionID: fixture.sessionID})
	if !errors.Is(err, ErrExportFailed) || strings.Contains(err.Error(), "private path") {
		t.Fatalf("publish error = %v", err)
	}
	entries, readErr := os.ReadDir(fixture.layout.ExportsDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("unpublished export residuals = %v", entries)
	}
}

func TestExportAnalysisRejectsUnavailableRootAndCancellation(t *testing.T) {
	fixture := newExportFixture(t, nil)
	fixture.service.exportRoot = ""
	if _, err := fixture.service.ExportAnalysis(context.Background(), ExportRequest{SessionID: fixture.sessionID}); !errors.Is(err, ErrExportUnavailable) {
		t.Fatalf("unavailable root error = %v", err)
	}
	fixture.service.exportRoot = fixture.layout.ExportsDir
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixture.service.ExportAnalysis(ctx, ExportRequest{SessionID: fixture.sessionID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled export error = %v", err)
	}
	entries, err := os.ReadDir(fixture.layout.ExportsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancelled export created entries: %v", entries)
	}
}
