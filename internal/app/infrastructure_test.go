package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	douyinLive "github.com/jwwsjlm/douyinLive/v2"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
	"github.com/jwwsjlm/douyinlive-proto/generated/new_douyin"
	"google.golang.org/protobuf/proto"
)

func TestInitializeInfrastructureCreatesDataAndClosesCleanly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app-data")
	now := time.Date(2026, 7, 17, 13, 0, 0, 0, time.Local)
	application := New(Options{Name: "test", Version: "test"})
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: root, Now: now}); err != nil {
		t.Fatalf("InitializeInfrastructure() error = %v", err)
	}

	bootstrap := application.Bootstrap()
	if !bootstrap.Data.Ready || !bootstrap.Data.LoggingReady {
		t.Fatalf("data status is not ready: %#v", bootstrap.Data)
	}
	if bootstrap.Data.SchemaVersion != 3 || bootstrap.Data.Mode != DataModeReadWrite {
		t.Fatalf("unexpected data status: %#v", bootstrap.Data)
	}
	diagnosticsAvailable := false
	for _, capability := range bootstrap.Capabilities {
		if capability.ID == "diagnostics" {
			diagnosticsAvailable = capability.Available
			break
		}
	}
	if !diagnosticsAvailable {
		t.Fatal("diagnostics capability is unavailable after logging initialization")
	}
	if application.Store() == nil || application.Logger() == nil {
		t.Fatal("infrastructure accessors returned nil")
	}
	for _, path := range []string{
		filepath.Join(root, "app.db"),
		filepath.Join(root, "logs", "app-2026-07-17.jsonl"),
		filepath.Join(root, "config", "settings.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected artifact %q: %v", path, err)
		}
	}
	if application.RoomService() == nil || application.SettingsService() == nil || application.CredentialStore() == nil ||
		application.MonitorManager() == nil || application.CaptureCoordinator() == nil || application.EventStoreManager() == nil {
		t.Fatal("application services were not initialized")
	}

	application.Startup(context.Background())
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if application.Bootstrap().Data.Ready {
		t.Fatal("data status remained ready after shutdown")
	}
	logData, err := os.ReadFile(filepath.Join(root, "logs", "app-2026-07-17.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile(log) error = %v", err)
	}
	if !strings.Contains(string(logData), `"schema_version":3`) {
		t.Fatalf("log does not contain schema version: %s", logData)
	}
	if strings.Contains(string(logData), root) {
		t.Fatalf("log exposes data-root path: %s", logData)
	}
}

func TestInitializeInfrastructureWiresRecorderFactoryWithoutExposingPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app-data")
	application := New(Options{Name: "recorder-wiring", Version: "test"})
	var received capture.FFmpegRecorderFactoryOptions
	calls := 0
	application.newRecorderFactory = func(
		ctx context.Context,
		options capture.FFmpegRecorderFactoryOptions,
	) (capture.RecorderFactory, capture.FFmpegDependencyInfo, error) {
		if err := ctx.Err(); err != nil {
			return nil, capture.FFmpegDependencyInfo{}, err
		}
		calls++
		received = options
		return func(context.Context, capture.LiveSession, capture.OpenRequest, capture.CaptureSource) (capture.Recorder, error) {
				return nil, capture.ErrRecordingUnavailable
			}, capture.FFmpegDependencyInfo{
				FFmpeg: capture.FFmpegBinaryInfo{
					Version: "8.1.2", BuildSummary: "configuration: --enable-gpl",
					SHA256: strings.Repeat("a", 64),
				},
				FFprobe: capture.FFmpegBinaryInfo{
					Version: "8.1.2", BuildSummary: "configuration: --enable-version3",
					SHA256: strings.Repeat("b", 64),
				},
			}, nil
	}
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: root}); err != nil {
		t.Fatalf("InitializeInfrastructure() error = %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown(context.Background()) })
	if calls != 1 {
		t.Fatalf("recorder factory builder calls = %d, want 1", calls)
	}
	if received.DataRoot != filepath.Clean(root) || received.MaxConcurrentRecordings != 1 {
		t.Fatalf("recorder factory options = %s", received)
	}
	if received.BundledDir != "" && !filepath.IsAbs(received.BundledDir) {
		t.Fatalf("bundled directory is not absolute")
	}
	logData, err := os.ReadFile(filepath.Join(root, "logs", time.Now().Format("app-2006-01-02.jsonl")))
	if err != nil {
		t.Fatalf("ReadFile(log) error = %v", err)
	}
	if !strings.Contains(string(logData), `"ffmpeg_version":"8.1.2"`) ||
		!strings.Contains(string(logData), strings.Repeat("a", 64)) ||
		!strings.Contains(string(logData), `"ffmpeg_build_summary":"configuration: --enable-gpl"`) ||
		!strings.Contains(string(logData), `"ffprobe_build_summary":"configuration: --enable-version3"`) {
		t.Fatalf("recording dependency evidence missing from log: %s", logData)
	}
	if strings.Contains(string(logData), root) ||
		(received.BundledDir != "" && strings.Contains(string(logData), received.BundledDir)) {
		t.Fatalf("recording dependency log exposes a path: %s", logData)
	}
}

func TestInitializeInfrastructureDefersCustomRecordingRootFailClosed(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "app-data")
	layout, err := storage.PrepareLayout(root)
	if err != nil {
		t.Fatal(err)
	}
	settingsService, err := settings.Open(layout.ConfigDir, layout.Root, layout.RoomsDir)
	if err != nil {
		t.Fatal(err)
	}
	current, err := settingsService.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	customRoot := filepath.Join(t.TempDir(), "custom-recordings")
	if _, err := settingsService.UpdateSettings(ctx, settings.UpdateSettingsInput{
		RecordingDirectory: customRoot, DefaultQuality: current.DefaultQuality,
		DefaultSegmentMinutes:   current.DefaultSegmentMinutes,
		MaxConcurrentRecordings: current.MaxConcurrentRecordings,
		MinimumFreeSpaceGiB:     current.MinimumFreeSpaceGiB,
		SaveDisplayNames:        current.SaveDisplayNames,
	}); err != nil {
		t.Fatal(err)
	}
	application := New(Options{Name: "custom-root", Version: "test"})
	builderCalls := 0
	application.newRecorderFactory = func(context.Context, capture.FFmpegRecorderFactoryOptions) (
		capture.RecorderFactory, capture.FFmpegDependencyInfo, error,
	) {
		builderCalls++
		return nil, capture.FFmpegDependencyInfo{}, errors.New("must not discover dependencies")
	}
	if err := application.InitializeInfrastructure(ctx, InfrastructureOptions{DataRoot: root}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Shutdown(context.Background()) })
	if builderCalls != 0 {
		t.Fatalf("custom root initialized recorder factory %d times", builderCalls)
	}
	logData, err := os.ReadFile(filepath.Join(root, "logs", time.Now().Format("app-2006-01-02.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), `"error_code":"RECORDING_ROOT_DEFERRED"`) {
		t.Fatalf("custom root deferral is not observable: %s", logData)
	}
	if strings.Contains(string(logData), customRoot) {
		t.Fatalf("custom recording root leaked to diagnostics: %s", logData)
	}
}

func TestEventSessionDescriptorForOpenPreservesRequestClockLineage(t *testing.T) {
	startedAt := time.Now()
	session := capture.LiveSession{
		ID: "session", DataPath: "rooms/room/sessions/session",
		PlatformRoomID: "platform-room",
		StartedAt:      startedAt.Add(-time.Hour).UnixMilli(),
	}
	descriptor := eventSessionDescriptorForOpen(session, capture.OpenRequest{
		StartedAt: startedAt,
	})
	if descriptor.StartedAt != startedAt {
		t.Fatalf("live started_at lost request clock lineage: got=%v want=%v",
			descriptor.StartedAt, startedAt)
	}
	receivedAt := startedAt.Add(1500 * time.Millisecond)
	if offset := receivedAt.Sub(descriptor.StartedAt); offset != 1500*time.Millisecond {
		t.Fatalf("live offset = %v, want 1.5s", offset)
	}
	fallback := eventSessionDescriptorForOpen(session, capture.OpenRequest{})
	wantFallback := time.UnixMilli(session.StartedAt).UTC()
	if !fallback.StartedAt.Equal(wantFallback) || fallback.StartedAt.Location() != time.UTC {
		t.Fatalf("zero request fallback = %v, want %v", fallback.StartedAt, wantFallback)
	}
}

func TestInitializeInfrastructureAppliesSaveDisplayNames(t *testing.T) {
	const nickname = "privacy-visible-name"
	for _, test := range []struct {
		name string
		save bool
		want string
	}{
		{name: "enabled", save: true, want: nickname},
		{name: "disabled", save: false, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root := filepath.Join(t.TempDir(), "app-data")
			layout, err := storage.PrepareLayout(root)
			if err != nil {
				t.Fatalf("PrepareLayout() error = %v", err)
			}
			configured, err := settings.Open(layout.ConfigDir, layout.Root, layout.RoomsDir)
			if err != nil {
				t.Fatalf("settings.Open() error = %v", err)
			}
			current, err := configured.GetSettings(ctx)
			if err != nil {
				t.Fatalf("GetSettings() error = %v", err)
			}
			if current.SaveDisplayNames != test.save {
				_, err = configured.UpdateSettings(ctx, settings.UpdateSettingsInput{
					RecordingDirectory:      current.RecordingDirectory,
					DefaultQuality:          current.DefaultQuality,
					DefaultSegmentMinutes:   current.DefaultSegmentMinutes,
					MaxConcurrentRecordings: current.MaxConcurrentRecordings,
					MinimumFreeSpaceGiB:     current.MinimumFreeSpaceGiB,
					SaveDisplayNames:        test.save,
				})
				if err != nil {
					t.Fatalf("UpdateSettings() error = %v", err)
				}
			}

			application := New(Options{Name: "privacy-test", Version: "test"})
			application.Startup(ctx)
			if err := application.InitializeInfrastructure(ctx, InfrastructureOptions{DataRoot: root}); err != nil {
				t.Fatalf("InitializeInfrastructure() error = %v", err)
			}
			t.Cleanup(func() {
				if err := application.Shutdown(context.Background()); err != nil {
					t.Errorf("Shutdown() error = %v", err)
				}
			})

			roomConfig, err := application.RoomService().CreateRoom(ctx, room.CreateRoomInput{
				LiveID: "privacy-room", Alias: "privacy",
				RecordingProfile: room.RecordingProfile{Quality: room.QualityAuto, SegmentMinutes: 10},
			})
			if err != nil {
				t.Fatalf("CreateRoom() error = %v", err)
			}
			repository, err := capture.NewSQLiteRepository(
				application.Store().Writer(), application.Store().Reader(), layout.Root,
			)
			if err != nil {
				t.Fatalf("NewSQLiteRepository() error = %v", err)
			}
			session, err := repository.Create(ctx, capture.CreateSessionInput{
				RoomConfigID: roomConfig.ID, OperationID: mustAppUUIDv7(t),
			})
			if err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			sink, err := application.EventStoreManager().OpenSession(ctx, eventSessionDescriptor(session))
			if err != nil {
				t.Fatalf("OpenSession() error = %v", err)
			}
			payload, err := proto.Marshal(&new_douyin.Webcast_Im_ChatMessage{
				Common:  &new_douyin.Webcast_Im_Common{MsgId: 7001},
				User:    &new_douyin.Webcast_Data_User{WebcastUid: "privacy-user", Nickname: nickname},
				Content: "privacy-content",
			})
			if err != nil {
				t.Fatalf("proto.Marshal() error = %v", err)
			}
			sink.Accept(&douyinLive.LiveMessage{
				Raw:        &new_douyin.Webcast_Im_Message{Method: "WebcastChatMessage", Payload: payload},
				ReceivedAt: time.UnixMilli(session.StartedAt).UTC().Add(time.Second),
			})
			if err := sink.FlushAndClose(ctx); err != nil {
				t.Fatalf("FlushAndClose() error = %v", err)
			}
			var displayName string
			if err := application.Store().Reader().QueryRow(`SELECT COALESCE(display_name, '')
				FROM live_events WHERE session_id = ? AND event_role = 'source'`, session.ID).Scan(&displayName); err != nil {
				t.Fatalf("query persisted display name: %v", err)
			}
			if displayName != test.want {
				t.Fatalf("display_name = %q, want %q when saveDisplayNames=%v", displayName, test.want, test.save)
			}
		})
	}
}

func TestInitializeInfrastructureIsSingleUseUntilShutdown(t *testing.T) {
	application := New(Options{})
	options := InfrastructureOptions{DataRoot: t.TempDir()}
	if err := application.InitializeInfrastructure(context.Background(), options); err != nil {
		t.Fatalf("first InitializeInfrastructure() error = %v", err)
	}
	defer application.Shutdown(context.Background())
	if err := application.InitializeInfrastructure(context.Background(), options); err == nil {
		t.Fatal("second InitializeInfrastructure() error = nil, want duplicate error")
	}
}

func TestInfrastructurePersistsManifestHealthAndClearsItAfterRestart(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "app-data")
	sessionID, blockedSessionDir := seedBlockedSessionManifest(t, ctx, root)
	now := time.Date(2026, 7, 17, 13, 30, 0, 0, time.Local)

	first := New(Options{Name: "test", Version: "test"})
	first.Startup(ctx)
	if err := first.InitializeInfrastructure(ctx, InfrastructureOptions{DataRoot: root, Now: now}); err != nil {
		t.Fatalf("first InitializeInfrastructure() error = %v", err)
	}
	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown() error = %v", err)
	}
	logPath := filepath.Join(root, "logs", "app-"+now.Local().Format("2006-01-02")+".jsonl")
	firstLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read first health log: %v", err)
	}
	firstText := string(firstLog)
	if !strings.Contains(firstText, `"error_code":"`+capture.ManifestRepairRequiredErrorCode+`"`) ||
		!strings.Contains(firstText, `"error_code":"`+capture.ManifestRepairIncompleteErrorCode+`"`) {
		t.Fatalf("first startup log lacks persistent manifest health diagnostics: %s", firstText)
	}
	if strings.Contains(firstText, capture.ManifestRepairClearedErrorCode) {
		t.Fatalf("blocked repair was incorrectly logged as cleared: %s", firstText)
	}
	if strings.Contains(firstText, root) || strings.Contains(strings.ToLower(firstText), "not a directory") {
		t.Fatalf("manifest health log leaked an absolute path or raw error: %s", firstText)
	}

	if err := os.Remove(blockedSessionDir); err != nil {
		t.Fatalf("remove manifest blocker: %v", err)
	}
	second := New(Options{Name: "test", Version: "test"})
	second.Startup(ctx)
	if err := second.InitializeInfrastructure(ctx, InfrastructureOptions{DataRoot: root, Now: now}); err != nil {
		t.Fatalf("second InitializeInfrastructure() error = %v", err)
	}
	if err := second.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown() error = %v", err)
	}
	combinedLog, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read combined health log: %v", err)
	}
	combinedText := string(combinedLog)
	if got := strings.Count(combinedText, `"error_code":"`+capture.ManifestRepairRequiredErrorCode+`"`); got != 2 {
		t.Fatalf("required event count = %d, want 2: %s", got, combinedText)
	}
	if got := strings.Count(combinedText, `"error_code":"`+capture.ManifestRepairClearedErrorCode+`"`); got != 1 {
		t.Fatalf("cleared event count = %d, want 1: %s", got, combinedText)
	}
	if got := strings.Count(combinedText, `"error_code":"`+capture.ManifestRepairIncompleteErrorCode+`"`); got != 1 {
		t.Fatalf("incomplete event count = %d, want 1: %s", got, combinedText)
	}
	if !strings.Contains(combinedText, `"session_id":"`+sessionID+`"`) {
		t.Fatalf("health log lacks sanitized session correlation: %s", combinedText)
	}
	if strings.Contains(combinedText, root) {
		t.Fatalf("combined health log leaked data root: %s", combinedText)
	}
	if _, err := os.Stat(filepath.Join(blockedSessionDir, "session.json")); err != nil {
		t.Fatalf("restart did not restore manifest: %v", err)
	}
}

func TestShutdownSupersedesInfrastructureBeforeCommit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app-data")
	now := time.Date(2026, 7, 17, 13, 0, 0, 0, time.Local)
	application := New(Options{Name: "test", Version: "test"})
	application.Startup(context.Background())

	ready := make(chan struct{})
	release := make(chan struct{})
	application.beforeInfrastructureCommit = func() {
		close(ready)
		<-release
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	initResult := make(chan error, 1)
	go func() {
		initResult <- application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: root, Now: now})
	}()

	select {
	case <-ready:
	case err := <-initResult:
		t.Fatalf("InitializeInfrastructure() returned before commit barrier: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("InitializeInfrastructure() did not reach commit barrier")
	}
	shutdownResult := make(chan error, 1)
	go func() { shutdownResult <- application.Shutdown(context.Background()) }()
	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown() waited for the blocked infrastructure commit hook")
	}
	if application.State() != StateStopped || application.Store() != nil || application.MonitorManager() != nil || application.CaptureCoordinator() != nil || application.Bootstrap().Data.Ready {
		t.Fatalf("shutdown exposed uncommitted infrastructure: state=%s bootstrap=%#v", application.State(), application.Bootstrap())
	}
	close(release)

	select {
	case err := <-initResult:
		if !errors.Is(err, ErrInfrastructureSuperseded) {
			t.Fatalf("InitializeInfrastructure() error = %v, want ErrInfrastructureSuperseded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("superseded initialization did not clean up")
	}
	if application.Store() != nil || application.MonitorManager() != nil || application.CaptureCoordinator() != nil || application.Bootstrap().Data.Ready {
		t.Fatalf("superseded initialization wrote resources back: %#v", application.Bootstrap())
	}

	databasePath := filepath.Join(root, "app.db")
	if err := os.Rename(databasePath, databasePath+".closed"); err != nil {
		t.Fatalf("database remains open after superseded initialization: %v", err)
	}
	logs, err := filepath.Glob(filepath.Join(root, "logs", "*.jsonl"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("diagnostic log glob = %v, %v", logs, err)
	}
	if err := os.Rename(logs[0], logs[0]+".closed"); err != nil {
		t.Fatalf("diagnostic log remains open after superseded initialization: %v", err)
	}
}

func TestStoppedApplicationRequiresStartupBeforeReinitializing(t *testing.T) {
	application := New(Options{Name: "test", Version: "test"})
	application.Startup(context.Background())
	firstRoot := filepath.Join(t.TempDir(), "first")
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: firstRoot}); err != nil {
		t.Fatalf("first InitializeInfrastructure() error = %v", err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	secondRoot := filepath.Join(t.TempDir(), "second")
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: secondRoot}); !errors.Is(err, ErrInfrastructureSuperseded) {
		t.Fatalf("InitializeInfrastructure() while stopped error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(secondRoot, "app.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stopped initialization created database: %v", err)
	}

	application.Startup(context.Background())
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: secondRoot}); err != nil {
		t.Fatalf("InitializeInfrastructure() after Startup error = %v", err)
	}
	if application.Store() == nil || application.MonitorManager() == nil || application.CaptureCoordinator() == nil {
		t.Fatal("reinitialized application did not publish all infrastructure")
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("final Shutdown() error = %v", err)
	}
}

func TestCancelledInfrastructureContextCannotCommitAfterBarrier(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cancelled")
	application := New(Options{Name: "test", Version: "test"})
	application.Startup(context.Background())
	ready := make(chan struct{})
	release := make(chan struct{})
	application.beforeInfrastructureCommit = func() {
		close(ready)
		<-release
	}
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	initCtx, cancelInit := context.WithCancel(context.Background())
	initResult := make(chan error, 1)
	go func() {
		initResult <- application.InitializeInfrastructure(initCtx, InfrastructureOptions{DataRoot: root})
	}()
	select {
	case <-ready:
	case err := <-initResult:
		t.Fatalf("InitializeInfrastructure() returned before cancellation barrier: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("InitializeInfrastructure() did not reach cancellation barrier")
	}
	cancelInit()
	close(release)
	select {
	case err := <-initResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("InitializeInfrastructure() error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled initialization did not clean up")
	}
	if application.Store() != nil || application.MonitorManager() != nil || application.CaptureCoordinator() != nil || application.Bootstrap().Data.Ready {
		t.Fatalf("cancelled initialization committed resources: %#v", application.Bootstrap())
	}
	databasePath := filepath.Join(root, "app.db")
	if err := os.Rename(databasePath, databasePath+".closed"); err != nil {
		t.Fatalf("database remains open after cancelled initialization: %v", err)
	}
	logs, err := filepath.Glob(filepath.Join(root, "logs", "*.jsonl"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("diagnostic log glob = %v, %v", logs, err)
	}
	if err := os.Rename(logs[0], logs[0]+".closed"); err != nil {
		t.Fatalf("diagnostic log remains open after cancelled initialization: %v", err)
	}
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() after cancelled initialization error = %v", err)
	}
}

func seedBlockedSessionManifest(t *testing.T, ctx context.Context, root string) (string, string) {
	t.Helper()
	layout, err := storage.PrepareLayout(root)
	if err != nil {
		t.Fatalf("PrepareLayout() error = %v", err)
	}
	store, err := storage.Open(ctx, layout, storage.OpenOptions{})
	if err != nil {
		t.Fatalf("storage.Open() error = %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
	}()
	roomID := mustAppUUIDv7(t)
	if _, err := store.Writer().Exec(`INSERT INTO rooms(
		id, live_id, alias, created_at, updated_at
	) VALUES (?, ?, 'manifest-health', 1, 1)`, roomID, "live-"+roomID); err != nil {
		t.Fatalf("insert room: %v", err)
	}
	repository, err := capture.NewSQLiteRepository(store.Writer(), store.Reader(), layout.Root)
	if err != nil {
		t.Fatalf("NewSQLiteRepository() error = %v", err)
	}
	session, err := repository.Create(ctx, capture.CreateSessionInput{
		RoomConfigID: roomID, OperationID: mustAppUUIDv7(t),
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close seeded store: %v", err)
	}
	closed = true
	sessionDir := filepath.Join(layout.Root, filepath.FromSlash(session.DataPath))
	if err := os.RemoveAll(sessionDir); err != nil {
		t.Fatalf("remove seeded manifest directory: %v", err)
	}
	if err := os.WriteFile(sessionDir, []byte("block manifest repair"), 0o600); err != nil {
		t.Fatalf("create manifest repair blocker: %v", err)
	}
	return session.ID, sessionDir
}

func mustAppUUIDv7(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7() error = %v", err)
	}
	return id.String()
}
