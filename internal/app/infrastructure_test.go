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
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
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
	if bootstrap.Data.SchemaVersion != 2 || bootstrap.Data.Mode != DataModeReadWrite {
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
	if application.RoomService() == nil || application.SettingsService() == nil || application.CredentialStore() == nil || application.MonitorManager() == nil || application.CaptureCoordinator() == nil {
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
	if !strings.Contains(string(logData), `"schema_version":2`) {
		t.Fatalf("log does not contain schema version: %s", logData)
	}
	if strings.Contains(string(logData), root) {
		t.Fatalf("log exposes data-root path: %s", logData)
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
