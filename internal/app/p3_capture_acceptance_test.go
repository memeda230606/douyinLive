//go:build p3acceptance

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

const (
	p3AcceptanceOperationTimeout = 20 * time.Second
	p3AcceptanceSessionTimeout   = 60 * time.Second
)

func TestP3CaptureIsolationAcceptance(t *testing.T) {
	liveURL := strings.TrimSpace(os.Getenv("P3ACC_LIVE_URL"))
	if liveURL == "" {
		t.Skip("P3ACC_ENV_NOT_SET")
	}

	dataRoot := t.TempDir()
	application := New(Options{Name: "p3-acceptance", Version: "test"})
	application.Startup(context.Background())
	cleanupPending := true
	defer func() {
		if !cleanupPending {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), p3AcceptanceOperationTimeout)
		defer cancel()
		if err := application.Shutdown(ctx); err != nil {
			t.Errorf("P3ACC_SHUTDOWN_FAILED")
		}
	}()

	initCtx, initCancel := context.WithTimeout(context.Background(), p3AcceptanceOperationTimeout)
	err := application.InitializeInfrastructure(initCtx, InfrastructureOptions{DataRoot: dataRoot})
	initCancel()
	if err != nil {
		t.Fatalf("P3ACC_INIT_FAILED")
	}

	store := application.Store()
	roomService := application.RoomService()
	monitor := application.MonitorManager()
	if store == nil || roomService == nil || monitor == nil || application.CaptureCoordinator() == nil {
		t.Fatalf("P3ACC_INFRASTRUCTURE_INCOMPLETE")
	}

	createCtx, createCancel := context.WithTimeout(context.Background(), p3AcceptanceOperationTimeout)
	config, err := roomService.CreateRoom(createCtx, room.CreateRoomInput{
		LiveID:         liveURL,
		Alias:          "p3-capture-acceptance",
		MonitorEnabled: false,
		RecordEnabled:  false,
		RecordingProfile: room.RecordingProfile{
			Quality:        room.QualityAuto,
			SegmentMinutes: 10,
		},
	})
	createCancel()
	liveURL = ""
	if err != nil {
		t.Fatalf("P3ACC_ROOM_CREATE_FAILED code=%s", p3AcceptanceSafeErrorCode(room.ErrorCode(err)))
	}
	if config.RecordEnabled {
		t.Fatalf("P3ACC_RECORDING_POLICY_MISMATCH")
	}

	startCtx, startCancel := context.WithTimeout(context.Background(), p3AcceptanceOperationTimeout)
	err = monitor.StartMonitoring(startCtx, config.ID)
	startCancel()
	if err != nil {
		t.Fatalf("P3ACC_MONITOR_START_FAILED code=%s", p3AcceptanceSafeErrorCode(room.ErrorCode(err)))
	}

	sessionID, offline := p3AcceptanceWaitForSession(t, monitor, config.ID)
	stopCtx, stopCancel := context.WithTimeout(context.Background(), p3AcceptanceOperationTimeout)
	err = monitor.StopMonitoring(stopCtx, config.ID)
	stopCancel()
	if err != nil {
		t.Fatalf("P3ACC_MONITOR_STOP_FAILED code=%s", p3AcceptanceSafeErrorCode(room.ErrorCode(err)))
	}

	if offline {
		cleanupPending = false
		p3AcceptanceShutdownAndVerify(t, application, store, monitor)
		t.Skip("P3ACC_OFFLINE")
	}

	queryCtx, queryCancel := context.WithTimeout(context.Background(), p3AcceptanceOperationTimeout)
	session := p3AcceptanceReadOnlySession(t, queryCtx, store.Reader())
	var activeCount int
	if err := store.Reader().QueryRowContext(queryCtx,
		`SELECT COUNT(*) FROM live_sessions WHERE status IN ('starting', 'recording', 'finalizing')`,
	).Scan(&activeCount); err != nil {
		queryCancel()
		t.Fatalf("P3ACC_ACTIVE_QUERY_FAILED")
	}
	queryCancel()

	if session.ID != sessionID || session.RoomConfigID != config.ID || strings.TrimSpace(session.OperationID) == "" {
		t.Fatalf("P3ACC_SESSION_IDENTITY_MISMATCH")
	}
	if session.Status != capture.SessionCompleted {
		t.Fatalf("P3ACC_SESSION_STATUS_MISMATCH status=%s", p3AcceptanceSafeSessionStatus(session.Status))
	}
	if session.RecordingStatus != capture.RecordingDisabled {
		t.Fatalf("P3ACC_RECORDING_STATUS_MISMATCH status=%s", p3AcceptanceSafeRecordingStatus(session.RecordingStatus))
	}
	if session.EndedAt == nil {
		t.Fatalf("P3ACC_ENDED_AT_MISSING")
	}
	if activeCount != 0 {
		t.Fatalf("P3ACC_ACTIVE_SESSION_REMAINS count=%d", activeCount)
	}

	manifestPath, safe := p3AcceptanceManifestPath(dataRoot, session.DataPath)
	if !safe {
		t.Fatalf("P3ACC_DATA_PATH_UNSAFE")
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("P3ACC_MANIFEST_READ_FAILED")
	}
	var manifest capture.LiveSession
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("P3ACC_MANIFEST_DECODE_FAILED")
	}
	if !reflect.DeepEqual(manifest, session) {
		t.Fatalf("P3ACC_MANIFEST_DB_MISMATCH")
	}

	cleanupPending = false
	p3AcceptanceShutdownAndVerify(t, application, store, monitor)
}

func p3AcceptanceWaitForSession(t *testing.T, monitor *room.MonitorManager, roomID string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), p3AcceptanceSessionTimeout)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	last := room.RoomRuntimeStatus{State: room.RuntimeStopped}

	for {
		status, err := monitor.GetRoomStatus(ctx, roomID)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			t.Fatalf("P3ACC_STATUS_QUERY_FAILED code=%s", p3AcceptanceSafeErrorCode(room.ErrorCode(err)))
		}
		last = status
		if status.State == room.RuntimeWaiting && status.ErrorCode == "ROOM_OFFLINE" {
			return "", true
		}
		if (status.State == room.RuntimeLive || status.State == room.RuntimeRecording) && status.SessionID != "" {
			return status.SessionID, false
		}
		if status.State == room.RuntimeError {
			t.Fatalf("P3ACC_MONITOR_ERROR state=%s code=%s",
				p3AcceptanceSafeRuntimeState(status.State), p3AcceptanceSafeErrorCode(status.ErrorCode))
		}

		select {
		case <-ctx.Done():
			break
		case <-ticker.C:
			continue
		}
		break
	}

	t.Fatalf("P3ACC_SESSION_TIMEOUT state=%s code=%s",
		p3AcceptanceSafeRuntimeState(last.State), p3AcceptanceSafeErrorCode(last.ErrorCode))
	return "", false
}

func p3AcceptanceReadOnlySession(t *testing.T, ctx context.Context, reader *sql.DB) capture.LiveSession {
	t.Helper()
	var count int
	if err := reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_sessions`).Scan(&count); err != nil {
		t.Fatalf("P3ACC_SESSION_COUNT_FAILED")
	}
	if count != 1 {
		t.Fatalf("P3ACC_SESSION_COUNT_MISMATCH count=%d", count)
	}

	var session capture.LiveSession
	var platformRoomID sql.NullString
	var endedAt, mediaEpochAt sql.NullInt64
	err := reader.QueryRowContext(ctx, `SELECT id, room_config_id, operation_id, platform_room_id, title,
		status, recording_status, started_at, ended_at, media_epoch_at, capture_offset_ms,
		clock_source, integrity_score, data_path, schema_version, created_at, updated_at
		FROM live_sessions LIMIT 1`).Scan(
		&session.ID, &session.RoomConfigID, &session.OperationID, &platformRoomID, &session.Title,
		&session.Status, &session.RecordingStatus, &session.StartedAt, &endedAt, &mediaEpochAt,
		&session.CaptureOffsetMS, &session.ClockSource, &session.IntegrityScore, &session.DataPath,
		&session.SchemaVersion, &session.CreatedAt, &session.UpdatedAt,
	)
	if err != nil {
		t.Fatalf("P3ACC_SESSION_READ_FAILED")
	}
	session.PlatformRoomID = platformRoomID.String
	if endedAt.Valid {
		session.EndedAt = &endedAt.Int64
	}
	if mediaEpochAt.Valid {
		session.MediaEpochAt = &mediaEpochAt.Int64
	}
	return session
}

func p3AcceptanceManifestPath(dataRoot, dataPath string) (string, bool) {
	if dataPath == "" || strings.TrimSpace(dataPath) != dataPath || strings.ContainsAny(dataPath, `\:`) {
		return "", false
	}
	clean := path.Clean(dataPath)
	if clean != dataPath || clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return "", false
	}

	root, err := filepath.Abs(filepath.Clean(dataRoot))
	if err != nil {
		return "", false
	}
	manifest, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(clean), "session.json"))
	if err != nil {
		return "", false
	}
	relative, err := filepath.Rel(root, manifest)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", false
	}
	return manifest, true
}

func p3AcceptanceShutdownAndVerify(t *testing.T, application *Application, store *storage.Store, monitor *room.MonitorManager) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), p3AcceptanceOperationTimeout)
	defer cancel()
	if err := application.Shutdown(ctx); err != nil {
		t.Fatalf("P3ACC_SHUTDOWN_FAILED")
	}
	if application.State() != StateStopped || application.Bootstrap().Data.Ready {
		t.Fatalf("P3ACC_LIFECYCLE_NOT_STOPPED")
	}
	if application.Store() != nil || application.RoomService() != nil || application.SettingsService() != nil ||
		application.CredentialStore() != nil || application.MonitorManager() != nil || application.CaptureCoordinator() != nil {
		t.Fatalf("P3ACC_INFRASTRUCTURE_LEAK")
	}
	if err := store.Writer().PingContext(context.Background()); err == nil {
		t.Fatalf("P3ACC_WRITER_HANDLE_LEAK")
	}
	if err := store.Reader().PingContext(context.Background()); err == nil {
		t.Fatalf("P3ACC_READER_HANDLE_LEAK")
	}
	if _, err := monitor.GetRoomStatus(context.Background(), ""); room.ErrorCode(err) != "MONITOR_MANAGER_SHUTTING_DOWN" {
		t.Fatalf("P3ACC_MONITOR_LEAK")
	}
	if err := os.Rename(store.Path(), store.Path()+".closed"); err != nil {
		t.Fatalf("P3ACC_DATABASE_HANDLE_LEAK")
	}
}

func p3AcceptanceSafeRuntimeState(state room.RuntimeState) string {
	switch state {
	case room.RuntimeStopped, room.RuntimeWaiting, room.RuntimeStarting, room.RuntimeLive,
		room.RuntimeRecording, room.RuntimeReconnecting, room.RuntimeFinalizing, room.RuntimeError:
		return string(state)
	default:
		return "UNKNOWN"
	}
}

func p3AcceptanceSafeErrorCode(code string) string {
	switch code {
	case "":
		return "NONE"
	case "ROOM_OFFLINE", "ROOM_OFFLINE_CONFIRMING", "ROOM_CHECK_FAILED", "ROOM_NOT_FOUND",
		"COOKIE_INVALID", "CAPTURE_OPEN_FAILED", "CAPTURE_REBIND_FAILED", "ROOM_CONNECTION_INTERRUPTED",
		"CAPTURE_FINALIZING", "MONITOR_LIMIT_REACHED", "MONITOR_MANAGER_SHUTTING_DOWN":
		return code
	default:
		return "UNKNOWN"
	}
}

func p3AcceptanceSafeSessionStatus(status capture.SessionStatus) string {
	switch status {
	case capture.SessionStarting, capture.SessionRecording, capture.SessionFinalizing,
		capture.SessionCompleted, capture.SessionInterrupted, capture.SessionFailed:
		return string(status)
	default:
		return "unknown"
	}
}

func p3AcceptanceSafeRecordingStatus(status capture.RecordingStatus) string {
	switch status {
	case capture.RecordingPending, capture.RecordingDisabled, capture.RecordingStarting,
		capture.RecordingActive, capture.RecordingUnavailable, capture.RecordingReconnecting,
		capture.RecordingFinalizing, capture.RecordingCompleted, capture.RecordingIncomplete,
		capture.RecordingFailed:
		return string(status)
	default:
		return "unknown"
	}
}
