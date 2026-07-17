package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if bootstrap.Data.SchemaVersion != 1 || bootstrap.Data.Mode != DataModeReadWrite {
		t.Fatalf("unexpected data status: %#v", bootstrap.Data)
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
	if application.RoomService() == nil || application.SettingsService() == nil || application.CredentialStore() == nil {
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
	if !strings.Contains(string(logData), `"schema_version":1`) {
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
