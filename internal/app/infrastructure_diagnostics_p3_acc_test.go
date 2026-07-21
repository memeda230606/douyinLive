//go:build p3accacceptance

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestP3ACCAcceptanceDiscardDiagnosticsCreatesNoLog(t *testing.T) {
	logsDirectory := filepath.Join(t.TempDir(), "logs")
	logger, err := openInfrastructureLogger(logsDirectory, InfrastructureOptions{DisableDiagnostics: true})
	if err != nil {
		t.Fatalf("openInfrastructureLogger() error = %v", err)
	}
	if logger.Path != "" {
		t.Fatalf("discard logger path = %q, want empty", logger.Path)
	}
	logger.Logger.InfoContext(context.Background(), "private marker",
		"live_id", "P3ACC_PRIVATE_LIVE_ID",
		"room_id", "P3ACC_PRIVATE_ROOM_ID",
		"content", "P3ACC_PRIVATE_CONTENT",
	)
	if err := logger.Sync(); err != nil {
		t.Fatalf("discard logger Sync() error = %v", err)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("discard logger Close() error = %v", err)
	}
	if _, err := os.Stat(logsDirectory); !os.IsNotExist(err) {
		t.Fatalf("discard diagnostics created logs directory or returned unexpected error: %v", err)
	}
}
