package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/credentials"
	"github.com/jwwsjlm/douyinLive/v2/internal/diagnostics"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

const DataModeReadWrite = "READ_WRITE"

const captureManifestHealthLogSyncFailedErrorCode = "CAPTURE_MANIFEST_HEALTH_LOG_SYNC_FAILED"

var ErrInfrastructureSuperseded = errors.New("application infrastructure initialization was superseded")

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
	state := a.state
	generation := a.lifecycleGeneration
	monitorRoot := a.lifecycle
	statusPublisher := a.roomStatusPublisher
	commitHook := a.beforeInfrastructureCommit
	a.mu.RUnlock()
	if alreadyInitialized {
		return errors.New("application infrastructure is already initialized")
	}
	if state == StateStopped || state == StateStopping {
		return ErrInfrastructureSuperseded
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

	credentialStore, err := credentials.OpenFileStore(filepath.Join(layout.ConfigDir, "credentials.dat"))
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize credential store: %w", err)
	}
	settingsService, err := settings.Open(layout.ConfigDir, layout.Root, layout.RoomsDir)
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize settings service: %w", err)
	}
	roomService, err := room.NewService(store.Writer(), store.Reader(), credentialStore)
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize room service: %w", err)
	}
	captureRepository, err := capture.NewSQLiteRepositoryWithOptions(
		store.Writer(), store.Reader(), layout.Root,
		capture.SQLiteRepositoryOptions{ManifestHealthReporter: newManifestHealthReporter(logFile)},
	)
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize capture repository: %w", err)
	}
	repairReport, repairErr := captureRepository.RepairManifests(ctx)
	if repairErr != nil {
		logger.WarnContext(ctx, "capture manifest startup repair incomplete",
			"component", "capture", "error_code", capture.ManifestRepairIncompleteErrorCode,
			"correlation_id", "startup", "scanned", repairReport.Scanned,
			"repaired", repairReport.Repaired, "failed", repairReport.Failed)
	}
	captureCoordinator, err := capture.NewCoordinator(captureRepository, capture.CoordinatorOptions{})
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize capture coordinator: %w", err)
	}

	if monitorRoot == nil {
		monitorRoot = ctx
	}
	monitorManager, err := room.NewMonitorManager(monitorRoot, roomService, logger, room.MonitorOptions{
		Coordinator: captureCoordinator,
		Publisher:   statusPublisher,
	})
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize room monitor manager: %w", err)
	}
	if err := monitorManager.StartEnabled(ctx); err != nil {
		return errors.Join(
			fmt.Errorf("resume enabled room monitors: %w", err),
			cleanupUncommittedInfrastructure(monitorManager, store, logFile),
		)
	}
	if commitHook != nil {
		commitHook()
	}

	a.mu.Lock()
	superseded := a.lifecycleGeneration != generation || a.state == StateStopped || a.state == StateStopping
	contextErr := ctx.Err()
	if superseded || contextErr != nil {
		a.mu.Unlock()
		commitErr := contextErr
		if superseded {
			commitErr = errors.Join(ErrInfrastructureSuperseded, contextErr)
		}
		return errors.Join(commitErr, cleanupUncommittedInfrastructure(monitorManager, store, logFile))
	}
	a.initialized = true
	a.store = store
	a.credentials = credentialStore
	a.settings = settingsService
	a.rooms = roomService
	a.monitor = monitorManager
	a.coordinator = captureCoordinator
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

type manifestHealthLogReporter struct {
	mu         sync.Mutex
	logger     *slog.Logger
	syncFile   func() error
	batchDepth int
	pending    bool
	lastEvent  capture.ManifestHealthEvent
}

func newManifestHealthReporter(logFile *diagnostics.FileLogger) capture.ManifestHealthReporter {
	if logFile == nil || logFile.Logger == nil {
		return nil
	}
	return &manifestHealthLogReporter{
		logger:   logFile.Logger,
		syncFile: logFile.Sync,
	}
}

func (r *manifestHealthLogReporter) ReportManifestHealth(event capture.ManifestHealthEvent) error {
	if r == nil || r.logger == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	attributes := []any{
		"component", "capture",
		"error_code", event.ErrorCode,
		"correlation_id", event.SessionID,
		"session_id", event.SessionID,
		"outstanding", event.Outstanding,
	}
	switch event.State {
	case capture.ManifestHealthRepairRequired:
		r.logger.Warn("capture session manifest requires repair", attributes...)
	case capture.ManifestHealthRepairCleared:
		r.logger.Info("capture session manifest repair cleared", attributes...)
	default:
		return nil
	}
	if r.batchDepth > 0 {
		r.pending = true
		r.lastEvent = event
		return nil
	}
	return r.syncHealthLogLocked(event)
}

func (r *manifestHealthLogReporter) BeginManifestHealthBatch() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.batchDepth++
	r.mu.Unlock()
	return nil
}

func (r *manifestHealthLogReporter) EndManifestHealthBatch() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.batchDepth == 0 {
		return errors.New("manifest health batch was not started")
	}
	r.batchDepth--
	if r.batchDepth > 0 || !r.pending {
		return nil
	}
	event := r.lastEvent
	r.pending = false
	return r.syncHealthLogLocked(event)
}

func (r *manifestHealthLogReporter) syncHealthLogLocked(event capture.ManifestHealthEvent) error {
	if r.syncFile == nil {
		return nil
	}
	if err := r.syncFile(); err == nil {
		return nil
	}
	r.logger.Error("capture manifest health log sync failed",
		"component", "capture",
		"error_code", captureManifestHealthLogSyncFailedErrorCode,
		"correlation_id", event.SessionID,
		"session_id", event.SessionID,
		"outstanding", event.Outstanding)
	if err := r.syncFile(); err != nil {
		return errors.New("capture manifest health log sync failed")
	}
	return nil
}
func cleanupUncommittedInfrastructure(monitor *room.MonitorManager, store *storage.Store, logFile *diagnostics.FileLogger) error {
	var monitorErr, storeErr, logErr error
	if monitor != nil {
		monitorErr = monitor.Shutdown(context.Background())
	}
	if store != nil {
		storeErr = store.Close()
	}
	if logFile != nil {
		logErr = logFile.Close()
	}
	return errors.Join(monitorErr, storeErr, logErr)
}
