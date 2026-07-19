// Package app 负责桌面应用装配、生命周期和面向绑定层的稳定 DTO。
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/credentials"
	"github.com/jwwsjlm/douyinLive/v2/internal/diagnostics"
	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

const BootstrapAPIVersion = "v1"

type State string

const (
	StateCreated  State = "CREATED"
	StateRunning  State = "RUNNING"
	StateStopping State = "STOPPING"
	StateStopped  State = "STOPPED"
)

type Options struct {
	Name    string
	Version string
}

type CapabilityDTO struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
}

type DataStatusDTO struct {
	Ready         bool   `json:"ready"`
	SchemaVersion int    `json:"schemaVersion"`
	Mode          string `json:"mode"`
	LoggingReady  bool   `json:"loggingReady"`
}

type BootstrapDTO struct {
	APIVersion   string          `json:"apiVersion"`
	Name         string          `json:"name"`
	Version      string          `json:"version"`
	State        State           `json:"state"`
	Data         DataStatusDTO   `json:"data"`
	Capabilities []CapabilityDTO `json:"capabilities"`
}

type applicationShutdownState struct {
	done chan struct{}
	err  error
}

// Application 是 Wails 绑定层与后续 Room/Settings/Capture 服务之间的装配边界。
type Application struct {
	initMu              sync.Mutex
	mu                  sync.RWMutex
	lifecycleGeneration uint64
	name                string
	version             string
	state               State
	cancel              context.CancelFunc
	lifecycle           context.Context
	initialized         bool
	dataStatus          DataStatusDTO
	store               *storage.Store
	rooms               *room.Service
	settings            *settings.Service
	monitor             *room.MonitorManager
	coordinator         capture.CaptureCoordinator
	events              *eventstore.Manager
	roomStatusPublisher room.StatusPublisher
	credentials         *credentials.FileStore
	logFile             *diagnostics.FileLogger
	logger              *slog.Logger
	instanceLease       applicationInstanceLease
	shutdown            *applicationShutdownState
	// beforeInfrastructureCommit is a package-private test barrier. Production
	// leaves it nil; it must never perform application work.
	beforeInfrastructureCommit func()
	// newRecorderFactory is a package-private dependency seam. Production leaves
	// it nil and uses capture.NewFFmpegRecorderFactory.
	newRecorderFactory func(context.Context, capture.FFmpegRecorderFactoryOptions) (
		capture.RecorderFactory, capture.FFmpegDependencyInfo, error)
	// recoverStartupSessions is a package-private dependency seam. Production
	// leaves it nil and uses capture.RecoverStartupSessions.
	recoverStartupSessions func(context.Context, capture.StartupRecoveryOptions) (
		capture.StartupRecoveryReport, error)
	// beforeShutdownCleanup is a package-private test barrier. Production leaves it nil.
	beforeShutdownCleanup func()
}

func New(options Options) *Application {
	if options.Name == "" {
		options.Name = "抖音直播分析"
	}
	if options.Version == "" {
		options.Version = "dev"
	}
	return &Application{
		name:    options.Name,
		version: options.Version,
		state:   StateCreated,
		logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

// Startup 保存由 Wails 管理的生命周期上下文，并为后续后台服务派生可取消上下文。
func (a *Application) Startup(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == StateRunning || a.state == StateStopping {
		return
	}
	a.lifecycleGeneration++
	if ctx == nil {
		ctx = context.Background()
	}
	a.lifecycle, a.cancel = context.WithCancel(ctx)
	a.shutdown = nil
	a.state = StateRunning
}

type applicationCleanup struct {
	cancel  context.CancelFunc
	monitor *room.MonitorManager
	events  *eventstore.Manager
	store   *storage.Store
	logFile *diagnostics.FileLogger
	logger  *slog.Logger
	hook    func()
	lease   applicationInstanceLease
}

// Shutdown starts one shared cleanup for the current lifecycle generation.
// The caller context only bounds this call's wait; it never owns cleanup.
func (a *Application) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("shutdown context is nil")
	}

	a.mu.Lock()
	if a.state == StateStopped {
		shutdown := a.shutdown
		a.mu.Unlock()
		return shutdown.err
	}
	if a.state != StateStopping {
		a.lifecycleGeneration++
		cleanup := applicationCleanup{
			cancel: a.cancel, monitor: a.monitor, events: a.events, store: a.store,
			logFile: a.logFile, logger: a.logger, hook: a.beforeShutdownCleanup,
			lease: a.instanceLease,
		}
		a.store = nil
		a.rooms = nil
		a.settings = nil
		a.monitor = nil
		a.coordinator = nil
		a.events = nil
		a.credentials = nil
		a.logFile = nil
		a.logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
		a.initialized = false
		a.instanceLease = nil
		a.state = StateStopping
		a.dataStatus = DataStatusDTO{}
		a.shutdown = &applicationShutdownState{done: make(chan struct{})}
		go a.runShutdown(cleanup, a.shutdown)
	}
	shutdown := a.shutdown
	a.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-shutdown.done:
		return shutdown.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Application) runShutdown(cleanup applicationCleanup, shutdown *applicationShutdownState) {
	if cleanup.hook != nil {
		cleanup.hook()
	}
	var monitorErr error
	if cleanup.monitor != nil {
		monitorErr = cleanup.monitor.Shutdown(context.Background())
	}
	var eventErr error
	if cleanup.events != nil {
		eventErr = cleanup.events.Shutdown(context.Background())
	}
	if cleanup.cancel != nil {
		cleanup.cancel()
	}
	if cleanup.logger != nil && cleanup.logFile != nil {
		cleanup.logger.Info("application infrastructure stopped",
			"component", "app", "error_code", "", "correlation_id", "shutdown")
	}
	var storeErr, logErr error
	if cleanup.store != nil {
		storeErr = cleanup.store.Close()
	}
	if cleanup.logFile != nil {
		logErr = cleanup.logFile.Close()
	}
	var leaseErr error
	if cleanup.lease != nil {
		leaseErr = cleanup.lease.Close()
	}
	result := errors.Join(monitorErr, eventErr, storeErr, logErr, leaseErr)
	stoppedLifecycle, cancelStoppedLifecycle := context.WithCancel(context.Background())
	cancelStoppedLifecycle()
	a.mu.Lock()
	shutdown.err = result
	a.lifecycle = stoppedLifecycle
	a.cancel = nil
	a.state = StateStopped
	close(shutdown.done)
	a.mu.Unlock()
}

func (a *Application) State() State {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.state
}

func (a *Application) Bootstrap() BootstrapDTO {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return BootstrapDTO{
		APIVersion: BootstrapAPIVersion,
		Name:       a.name,
		Version:    a.version,
		State:      a.state,
		Data:       a.dataStatus,
		Capabilities: []CapabilityDTO{
			{ID: "overview", Label: "总览", Available: true},
			{ID: "rooms", Label: "直播间", Available: a.rooms != nil},
			{ID: "sessions", Label: "历史场次", Available: false},
			{ID: "analysis", Label: "分析", Available: false},
			{ID: "diagnostics", Label: "诊断", Available: a.dataStatus.LoggingReady},
			{ID: "settings", Label: "设置", Available: a.settings != nil},
		},
	}
}

func (a *Application) Store() *storage.Store {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.store
}

func (a *Application) Logger() *slog.Logger {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.logger
}

func (a *Application) Context() context.Context {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.lifecycle == nil {
		return context.Background()
	}
	return a.lifecycle
}

func (a *Application) RoomService() *room.Service {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.rooms
}

func (a *Application) SettingsService() *settings.Service {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.settings
}

func (a *Application) CredentialStore() *credentials.FileStore {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.credentials
}

func (a *Application) MonitorManager() *room.MonitorManager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.monitor
}

func (a *Application) CaptureCoordinator() capture.CaptureCoordinator {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.coordinator
}

func (a *Application) EventStoreManager() *eventstore.Manager {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.events
}

// SetRoomStatusPublisher must be called before infrastructure initialization.
// Desktop production uses it to bridge sanitized status DTOs to Wails events.
func (a *Application) SetRoomStatusPublisher(publisher room.StatusPublisher) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.initialized {
		return
	}
	a.roomStatusPublisher = publisher
}
