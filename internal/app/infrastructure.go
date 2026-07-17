package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/diagnostics"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

const DataModeReadWrite = "READ_WRITE"

type InfrastructureOptions struct {
	DataRoot string
	Now      time.Time
}

// InitializeInfrastructure prepares the local data root, redacted JSONL logs,
// and the migrated SQLite store before the Wails window is created.
func (a *Application) InitializeInfrastructure(ctx context.Context, options InfrastructureOptions) error {
	if ctx == nil {
		return errors.New("initialize infrastructure: context is nil")
	}
	a.initMu.Lock()
	defer a.initMu.Unlock()

	a.mu.RLock()
	alreadyInitialized := a.initialized
	a.mu.RUnlock()
	if alreadyInitialized {
		return errors.New("application infrastructure is already initialized")
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}

	layout, err := storage.PrepareLayout(options.DataRoot)
	if err != nil {
		return fmt.Errorf("prepare application data layout: %w", err)
	}
	logFile, err := diagnostics.OpenFileLogger(layout.LogsDir, diagnostics.FileOptions{Now: options.Now})
	if err != nil {
		return fmt.Errorf("initialize diagnostics logger: %w", err)
	}
	logger := logFile.Logger
	logger.InfoContext(ctx, "application infrastructure initializing",
		"component", "app", "error_code", "", "correlation_id", "startup")

	store, err := storage.Open(ctx, layout, storage.OpenOptions{
		Now:           options.Now,
		MaxReadConns:  4,
		CreateBackups: true,
	})
	if err != nil {
		logger.ErrorContext(ctx, "database initialization failed",
			"component", "storage", "error_code", "DATABASE_INIT_FAILED", "correlation_id", "startup", "err", err)
		_ = logFile.Close()
		return fmt.Errorf("initialize sqlite store: %w", err)
	}

	a.mu.Lock()
	a.initialized = true
	a.store = store
	a.logFile = logFile
	a.logger = logger
	a.dataStatus = DataStatusDTO{
		Ready:         true,
		SchemaVersion: store.SchemaVersion(),
		Mode:          DataModeReadWrite,
		LoggingReady:  true,
	}
	a.mu.Unlock()

	logger.InfoContext(ctx, "application infrastructure ready",
		"component", "storage",
		"error_code", "",
		"correlation_id", "startup",
		"schema_version", store.SchemaVersion(),
		"journal_mode", "WAL",
	)
	return nil
}
