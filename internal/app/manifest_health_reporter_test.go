package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
)

func TestManifestHealthLogReporterSyncsOncePerBatchAndImmediatelyAtRuntime(t *testing.T) {
	syncCalls := 0
	reporter := &manifestHealthLogReporter{
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		syncFile: func() error {
			syncCalls++
			return nil
		},
	}
	if err := reporter.BeginManifestHealthBatch(); err != nil {
		t.Fatalf("BeginManifestHealthBatch() error = %v", err)
	}
	for _, event := range []capture.ManifestHealthEvent{
		{SessionID: "session-a", State: capture.ManifestHealthRepairRequired, ErrorCode: capture.ManifestRepairRequiredErrorCode, Outstanding: 1},
		{SessionID: "session-b", State: capture.ManifestHealthRepairRequired, ErrorCode: capture.ManifestRepairRequiredErrorCode, Outstanding: 2},
		{SessionID: "session-a", State: capture.ManifestHealthRepairCleared, ErrorCode: capture.ManifestRepairClearedErrorCode, Outstanding: 1},
		{SessionID: "session-b", State: capture.ManifestHealthRepairCleared, ErrorCode: capture.ManifestRepairClearedErrorCode, Outstanding: 0},
	} {
		if err := reporter.ReportManifestHealth(event); err != nil {
			t.Fatalf("ReportManifestHealth() error = %v", err)
		}
	}
	if syncCalls != 0 {
		t.Fatalf("batch performed %d early Sync calls", syncCalls)
	}
	if err := reporter.EndManifestHealthBatch(); err != nil {
		t.Fatalf("EndManifestHealthBatch() error = %v", err)
	}
	if syncCalls != 1 {
		t.Fatalf("batch Sync calls = %d, want 1", syncCalls)
	}
	if err := reporter.ReportManifestHealth(capture.ManifestHealthEvent{
		SessionID: "runtime", State: capture.ManifestHealthRepairCleared,
		ErrorCode: capture.ManifestRepairClearedErrorCode,
	}); err != nil {
		t.Fatalf("runtime ReportManifestHealth() error = %v", err)
	}
	if syncCalls != 2 {
		t.Fatalf("runtime edge Sync calls = %d, want 2", syncCalls)
	}
}

func TestStoppingApplicationRejectsNewInfrastructureWithoutResurrection(t *testing.T) {
	application := New(Options{Name: "stopping-infrastructure", Version: "test"})
	application.Startup(context.Background())
	firstRoot := filepath.Join(t.TempDir(), "first")
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: firstRoot}); err != nil {
		t.Fatalf("InitializeInfrastructure(first) error = %v", err)
	}
	cleanupEntered := make(chan struct{})
	cleanupRelease := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(cleanupRelease)
		}
	}()
	application.beforeShutdownCleanup = func() {
		close(cleanupEntered)
		<-cleanupRelease
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := application.Shutdown(shutdownCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short Shutdown() error = %v, want deadline exceeded", err)
	}
	select {
	case <-cleanupEntered:
	case <-time.After(time.Second):
		t.Fatal("shutdown cleanup did not enter the barrier")
	}
	if state := application.State(); state != StateStopping {
		t.Fatalf("state = %s, want STOPPING", state)
	}
	secondRoot := filepath.Join(t.TempDir(), "second")
	if err := application.InitializeInfrastructure(context.Background(), InfrastructureOptions{DataRoot: secondRoot}); !errors.Is(err, ErrInfrastructureSuperseded) {
		t.Fatalf("InitializeInfrastructure(second) error = %v, want ErrInfrastructureSuperseded", err)
	}
	if _, err := os.Stat(filepath.Join(secondRoot, "app.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("STOPPING initialization created a database: %v", err)
	}
	if application.Store() != nil || application.MonitorManager() != nil || application.CaptureCoordinator() != nil || application.Bootstrap().Data.Ready {
		t.Fatalf("STOPPING application exposed resources: %#v", application.Bootstrap())
	}
	close(cleanupRelease)
	released = true
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("shared Shutdown() completion error = %v", err)
	}
	if state := application.State(); state != StateStopped {
		t.Fatalf("final state = %s, want STOPPED", state)
	}
	if application.Store() != nil || application.MonitorManager() != nil || application.CaptureCoordinator() != nil || application.Bootstrap().Data.Ready {
		t.Fatalf("completed shutdown resurrected resources: %#v", application.Bootstrap())
	}
}

var _ capture.ManifestHealthBatchReporter = (*manifestHealthLogReporter)(nil)
