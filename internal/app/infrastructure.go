package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jwwsjlm/douyinLive/v2/internal/analysis"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/credentials"
	"github.com/jwwsjlm/douyinLive/v2/internal/diagnostics"
	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/playback"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

const (
	DataModeReadWrite                       = "READ_WRITE"
	recordingDependencyUnavailableErrorCode = "RECORDING_DEPENDENCY_UNAVAILABLE"
)

const captureManifestHealthLogSyncFailedErrorCode = "CAPTURE_MANIFEST_HEALTH_LOG_SYNC_FAILED"

var (
	ErrInfrastructureSuperseded = errors.New("application infrastructure initialization was superseded")
	ErrInfrastructureCleanup    = errors.New("APPLICATION_INFRASTRUCTURE_CLEANUP_FAILED")
)

type InfrastructureOptions struct {
	DataRoot           string
	Now                time.Time
	DisableDiagnostics bool
	ASRProvider        analysis.ASRProvider
}

// InitializeInfrastructure prepares the local data root, redacted JSONL logs,
// and the migrated SQLite store before the Wails window is created.
func (a *Application) InitializeInfrastructure(ctx context.Context, options InfrastructureOptions) (resultErr error) {
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
	liveEventPublisher := a.liveEventPublisher
	recordingProgressPublisher := a.recordingProgressPublisher
	commitHook := a.beforeInfrastructureCommit
	recorderFactoryBuilder := a.newRecorderFactory
	startupRecoveryRunner := a.recoverStartupSessions
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
	instanceLease, err := acquireApplicationInstanceLease(layout.Root)
	if err != nil {
		return fmt.Errorf("acquire application instance lease: %w", err)
	}
	leaseTransferred := false
	defer func() {
		if !leaseTransferred {
			resultErr = errors.Join(resultErr, instanceLease.Close())
		}
	}()

	logFile, err := openInfrastructureLogger(layout.LogsDir, options)
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
	appSettings, err := settingsService.GetSettings(ctx)
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("load application settings: %w", err)
	}
	roomService, err := room.NewService(store.Writer(), store.Reader(), credentialStore)
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize room service: %w", err)
	}
	playbackService, err := playback.NewServiceWithOptions(
		store.Reader(), playback.ServiceOptions{DataRoot: layout.Root},
	)
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize playback service: %w", err)
	}
	analysisService, err := analysis.NewServiceWithOptions(
		store.Writer(), store.Reader(), analysis.ServiceOptions{ASRProvider: options.ASRProvider},
	)
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize analysis service: %w", err)
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
	eventWriter, err := eventstore.NewWriter(store.Writer())
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize event writer: %w", err)
	}
	eventManager, err := eventstore.NewManager(ctx, eventstore.ManagerOptions{
		DataRoot: layout.Root, Writer: eventWriter, Credentials: credentialStore, Logger: logger,
		PrivacyOptions:     eventstore.PrivacyOptions{StoreDisplayName: appSettings.SaveDisplayNames},
		LiveEventPublisher: liveEventPublisher,
	})
	if err != nil {
		_ = store.Close()
		_ = logFile.Close()
		return fmt.Errorf("initialize event manager: %w", err)
	}
	repairReport, repairErr := captureRepository.RepairManifests(ctx)
	if repairErr != nil {
		logger.WarnContext(ctx, "capture manifest startup repair incomplete",
			"component", "capture", "error_code", capture.ManifestRepairIncompleteErrorCode,
			"correlation_id", "startup", "scanned", repairReport.Scanned,
			"repaired", repairReport.Repaired, "failed", repairReport.Failed)
	}
	processRecoverer, err := capture.NewSessionProcessRecoverer(captureRepository)
	if err != nil {
		return errors.Join(
			fmt.Errorf("initialize recorder process recovery: %w", err),
			cleanupUncommittedInfrastructure(nil, eventManager, store, logFile),
		)
	}
	var recorderFactory capture.RecorderFactory
	var mediaRecoverer capture.SessionMediaRecoverer
	var dependencyInfo capture.FFmpegDependencyInfo
	recorderOptions := capture.FFmpegRecorderFactoryOptions{
		DataRoot: layout.Root, RecordingRoot: appSettings.RecordingDirectory,
		BundledDir: recorderBundledDirectory(), Repository: captureRepository,
		MaxConcurrentRecordings: appSettings.MaxConcurrentRecordings,
	}
	var discoveryErr error
	if recorderFactoryBuilder == nil {
		components, err := capture.NewFFmpegRecorderComponents(ctx, recorderOptions)
		discoveryErr = err
		recorderFactory = components.RecorderFactory
		mediaRecoverer = components.MediaRecoverer
		dependencyInfo = components.DependencyInfo
	} else {
		recorderFactory, dependencyInfo, discoveryErr = recorderFactoryBuilder(ctx, recorderOptions)
	}
	if ctx.Err() != nil {
		return errors.Join(
			ctx.Err(),
			cleanupUncommittedInfrastructure(nil, eventManager, store, logFile),
		)
	}
	if discoveryErr != nil || recorderFactory == nil {
		logger.WarnContext(ctx, "recording dependency unavailable",
			"component", "capture", "error_code", recordingDependencyUnavailableErrorCode,
			"correlation_id", "startup")
	} else {
		logger.InfoContext(ctx, "recording dependency ready",
			"component", "capture", "error_code", "", "correlation_id", "startup",
			"ffmpeg_version", dependencyInfo.FFmpeg.Version,
			"ffmpeg_build_summary", dependencyInfo.FFmpeg.BuildSummary,
			"ffmpeg_sha256", dependencyInfo.FFmpeg.SHA256,
			"ffprobe_version", dependencyInfo.FFprobe.Version,
			"ffprobe_build_summary", dependencyInfo.FFprobe.BuildSummary,
			"ffprobe_sha256", dependencyInfo.FFprobe.SHA256)
	}
	if startupRecoveryRunner == nil {
		startupRecoveryRunner = capture.RecoverStartupSessions
	}
	startupRecoveryReport, startupRecoveryErr := startupRecoveryRunner(
		ctx,
		capture.StartupRecoveryOptions{
			Repository:       captureRepository,
			ProcessRecoverer: processRecoverer,
			MediaRecoverer:   mediaRecoverer,
			EventRecoverer: capture.SessionEventRecoveryFunc(func(
				recoveryCtx context.Context,
				session capture.LiveSession,
				minimumCutoff time.Time,
			) (time.Time, error) {
				cutoff, recoveryErr := eventManager.RecoverAndCloseSession(
					recoveryCtx, eventSessionDescriptor(session), minimumCutoff,
				)
				return cutoff, startupEventRecoveryError(recoveryErr)
			}),
			Reporter: capture.StartupRecoveryReporterFunc(func(event capture.StartupRecoveryEvent) {
				attributes := []any{
					"component", "capture", "error_code", event.ErrorCode,
					"correlation_id", event.SessionID, "session_id", event.SessionID,
					"room_config_id", event.RoomConfigID, "warning_codes", strings.Join(event.WarningCodes, ","),
					"cutoff_at", event.CutoffAtMS,
				}
				if event.State == capture.StartupRecoverySessionFailed {
					logger.WarnContext(ctx, "capture startup recovery incomplete", attributes...)
					return
				}
				logger.InfoContext(ctx, "capture startup recovery completed", attributes...)
			}),
			Now: func() time.Time { return options.Now },
		},
	)
	if ctx.Err() != nil {
		return errors.Join(
			ctx.Err(),
			cleanupUncommittedInfrastructure(nil, eventManager, store, logFile),
		)
	}
	if fatalRecoveryErr := startupRecoveryFailClosedError(startupRecoveryErr); fatalRecoveryErr != nil {
		logger.ErrorContext(ctx, "capture startup recovery failed closed",
			"component", "capture", "error_code", fatalRecoveryErr.Error(),
			"correlation_id", "startup", "scanned", startupRecoveryReport.Scanned,
			"recovered", startupRecoveryReport.Recovered, "failed", startupRecoveryReport.Failed,
			"warnings", startupRecoveryReport.Warnings)
		return errors.Join(
			fatalRecoveryErr,
			cleanupUncommittedInfrastructure(nil, eventManager, store, logFile),
		)
	}
	if startupRecoveryErr != nil {
		logger.WarnContext(ctx, "capture startup recovery finished with failures",
			"component", "capture", "error_code", "STARTUP_RECOVERY_INCOMPLETE",
			"correlation_id", "startup", "scanned", startupRecoveryReport.Scanned,
			"recovered", startupRecoveryReport.Recovered, "failed", startupRecoveryReport.Failed,
			"warnings", startupRecoveryReport.Warnings)
	} else {
		logger.InfoContext(ctx, "capture startup recovery ready",
			"component", "capture", "error_code", "", "correlation_id", "startup",
			"scanned", startupRecoveryReport.Scanned, "recovered", startupRecoveryReport.Recovered,
			"warnings", startupRecoveryReport.Warnings)
	}
	captureCoordinator, err := capture.NewCoordinator(captureRepository, capture.CoordinatorOptions{
		RecorderFactory: recorderFactory,
		EventSinkFactory: func(factoryCtx context.Context, session capture.LiveSession, request capture.OpenRequest) (capture.EventSink, error) {
			return eventManager.OpenSession(factoryCtx, eventSessionDescriptorForOpen(session, request))
		},
		ProgressPublisher: recordingProgressPublisher,
	})
	if err != nil {
		return errors.Join(
			fmt.Errorf("initialize capture coordinator: %w", err),
			cleanupUncommittedInfrastructure(nil, eventManager, store, logFile),
		)
	}

	if monitorRoot == nil {
		monitorRoot = ctx
	}
	monitorManager, err := room.NewMonitorManager(monitorRoot, roomService, logger, room.MonitorOptions{
		Coordinator: captureCoordinator,
		Publisher:   statusPublisher,
	})
	if err != nil {
		return errors.Join(
			fmt.Errorf("initialize room monitor manager: %w", err),
			cleanupUncommittedInfrastructure(nil, eventManager, store, logFile),
		)
	}
	if err := monitorManager.StartEnabled(ctx); err != nil {
		return errors.Join(
			fmt.Errorf("resume enabled room monitors: %w", err),
			cleanupUncommittedInfrastructure(monitorManager, eventManager, store, logFile),
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
		return errors.Join(commitErr, cleanupUncommittedInfrastructure(monitorManager, eventManager, store, logFile))
	}
	a.initialized = true
	a.store = store
	a.credentials = credentialStore
	a.settings = settingsService
	a.rooms = roomService
	a.monitor = monitorManager
	a.coordinator = captureCoordinator
	a.events = eventManager
	a.playback = playbackService
	a.analysis = analysisService
	a.logFile = logFile
	a.logger = logger
	a.dataStatus = DataStatusDTO{
		Ready:         true,
		SchemaVersion: store.SchemaVersion(),
		Mode:          DataModeReadWrite,
		LoggingReady:  true,
	}
	a.instanceLease = instanceLease
	leaseTransferred = true
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

func openInfrastructureLogger(logsDir string, options InfrastructureOptions) (*diagnostics.FileLogger, error) {
	if options.DisableDiagnostics {
		return diagnostics.NewDiscardFileLogger(), nil
	}
	return diagnostics.OpenFileLogger(logsDir, diagnostics.FileOptions{Now: options.Now})
}

func startupEventRecoveryError(err error) error {
	if errors.Is(err, eventstore.ErrRecoveryDeferred) {
		// Do not carry lower-layer causes across the application boundary: startup
		// recovery only needs the stable retry disposition.
		return capture.ErrStartupEventRecoveryDeferred
	}
	return err
}

func startupRecoveryFailClosedError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, capture.ErrStartupProcessRecovery):
		return capture.ErrStartupProcessRecovery
	case errors.Is(err, capture.ErrStartupEventRecoveryDeferred):
		return capture.ErrStartupEventRecoveryDeferred
	case errors.Is(err, capture.ErrStartupRecoveryConfiguration):
		return capture.ErrStartupRecoveryConfiguration
	case errors.Is(err, capture.ErrStartupRecoveryIncomplete),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// A component-local context error must never become a best-effort startup.
		// The caller context is checked before this helper.
		return capture.ErrStartupRecoveryIncomplete
	default:
		// Unknown startup-recovery failures have no proof that every recorder
		// process was inspected, so the application boundary remains fail-closed.
		return capture.ErrStartupRecoveryIncomplete
	}
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

func eventSessionDescriptor(session capture.LiveSession) eventstore.SessionDescriptor {
	return eventstore.SessionDescriptor{
		SessionID: session.ID, DataPath: session.DataPath,
		PlatformRoomID: session.PlatformRoomID,
		StartedAt:      time.UnixMilli(session.StartedAt).UTC(),
	}
}

func eventSessionDescriptorForOpen(
	session capture.LiveSession,
	request capture.OpenRequest,
) eventstore.SessionDescriptor {
	descriptor := eventSessionDescriptor(session)
	if !request.StartedAt.IsZero() {
		descriptor.StartedAt = request.StartedAt
	}
	return descriptor
}

func recorderBundledDirectory() string {
	executable, err := os.Executable()
	if err != nil || executable == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(executable), "ffmpeg")
}

func cleanupUncommittedInfrastructure(monitor *room.MonitorManager, events *eventstore.Manager, store *storage.Store, logFile *diagnostics.FileLogger) error {
	var monitorErr, eventErr, storeErr, logErr error
	if monitor != nil {
		monitorErr = monitor.Shutdown(context.Background())
	}
	if events != nil {
		eventErr = events.Shutdown(context.Background())
	}
	if store != nil {
		storeErr = store.Close()
	}
	if logFile != nil {
		logErr = logFile.Close()
	}
	return stableInfrastructureCleanupError(monitorErr, eventErr, storeErr, logErr)
}

func stableInfrastructureCleanupError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return ErrInfrastructureCleanup
		}
	}
	return nil
}
