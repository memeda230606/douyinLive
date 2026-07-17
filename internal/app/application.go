// Package app 负责桌面应用装配、生命周期和面向绑定层的稳定 DTO。
package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/jwwsjlm/douyinLive/v2/internal/credentials"
	"github.com/jwwsjlm/douyinLive/v2/internal/diagnostics"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
)

const BootstrapAPIVersion = "v1"

type State string

const (
	StateCreated State = "CREATED"
	StateRunning State = "RUNNING"
	StateStopped State = "STOPPED"
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

// Application 是 Wails 绑定层与后续 Room/Settings/Capture 服务之间的装配边界。
type Application struct {
	initMu              sync.Mutex
	mu                  sync.RWMutex
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
	roomStatusPublisher room.StatusPublisher
	credentials         *credentials.FileStore
	logFile             *diagnostics.FileLogger
	logger              *slog.Logger
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
	if a.state == StateRunning {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	a.lifecycle, a.cancel = context.WithCancel(ctx)
	a.state = StateRunning
}

// Shutdown 幂等取消后台生命周期。后续服务必须在返回前完成有界收尾。
func (a *Application) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("shutdown context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	a.mu.Lock()
	cancel := a.cancel
	monitor := a.monitor
	store := a.store
	logFile := a.logFile
	logger := a.logger
	a.lifecycle = nil
	a.cancel = nil
	a.store = nil
	a.rooms = nil
	a.settings = nil
	a.monitor = nil
	a.credentials = nil
	a.logFile = nil
	a.initialized = false
	a.state = StateStopped
	a.dataStatus = DataStatusDTO{}
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	var monitorErr error
	if monitor != nil {
		monitorErr = monitor.Shutdown(ctx)
	}
	if logger != nil && logFile != nil {
		logger.InfoContext(ctx, "application infrastructure stopped",
			"component", "app", "error_code", "", "correlation_id", "shutdown")
	}
	var storeErr, logErr error
	if store != nil {
		storeErr = store.Close()
	}
	if logFile != nil {
		logErr = logFile.Close()
	}
	return errors.Join(monitorErr, storeErr, logErr)
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
			{ID: "diagnostics", Label: "诊断", Available: false},
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
