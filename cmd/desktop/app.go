package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
	"github.com/jwwsjlm/douyinLive/v2/internal/capture"
	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const shutdownTimeout = 10 * time.Second

// DesktopApp 是 Wails 唯一绑定门面。业务能力由 internal/app 及后续服务实现。
type DesktopApp struct {
	application           *application.Application
	infrastructureOptions application.InfrastructureOptions
	emitEvent             func(context.Context, string, ...interface{})
	acceptingEvents       atomic.Bool
	startupMu             sync.Mutex
	startupReady          chan struct{}
	startupReadyOnce      sync.Once
	startupArmed          bool
	startupStarted        bool
	startupFinished       bool
	shutdownStarted       bool
	startupCancel         context.CancelFunc
	shutdownFinalizerOnce sync.Once
}

func NewDesktopApp(app *application.Application) *DesktopApp {
	return newDesktopApp(app, application.InfrastructureOptions{})
}

func newDesktopApp(app *application.Application, options application.InfrastructureOptions) *DesktopApp {
	return &DesktopApp{
		application: app, infrastructureOptions: options,
		emitEvent: runtime.EventsEmit,
	}
}

func (a *DesktopApp) startup(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	startupCtx, started := a.beginStartup(ctx)
	if !started {
		return
	}
	defer a.finishStartup()
	a.application.SetRoomStatusPublisher(func(status room.RoomRuntimeStatus) {
		a.emit(ctx, room.StatusEventName, status)
	})
	a.application.SetLiveEventPublisher(func(batch eventstore.LiveEventBatchDTO) {
		a.emit(ctx, eventstore.LiveEventEventName, batch)
	})
	a.application.SetRecordingProgressPublisher(func(progress capture.RecordingProgressDTO) {
		a.emit(ctx, capture.RecordingProgressEventName, progress)
	})
	if err := a.application.InitializeInfrastructure(startupCtx, a.infrastructureOptions); err != nil {
		slog.Error("桌面数据基础初始化失败",
			"component", "desktop",
			"error_code", "DATABASE_INIT_FAILED",
			"correlation_id", "startup",
		)
	} else if a.acceptanceHookAllowed() {
		a.startAcceptanceHook(startupCtx)
	}
}

func (a *DesktopApp) emit(ctx context.Context, eventName string, payload interface{}) {
	if a == nil || !a.acceptingEvents.Load() || a.emitEvent == nil {
		return
	}
	a.emitEvent(ctx, eventName, payload)
}

func (a *DesktopApp) shutdown(context.Context) {
	a.beginShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := a.application.Shutdown(ctx); err != nil {
		slog.Error("桌面应用关闭失败", "component", "desktop", "error_code", "SHUTDOWN_FAILED")
	}
	if a.application.State() == application.StateStopped {
		a.finishShutdown()
		return
	}
	a.shutdownFinalizerOnce.Do(func() {
		go func() {
			_ = a.application.Shutdown(context.Background())
			a.finishShutdown()
		}()
	})
}

// armStartup establishes the Bootstrap happens-before edge before the app is
// handed to Wails. Direct unit-test facades that are never armed stay usable.
func (a *DesktopApp) armStartup() {
	if a == nil {
		return
	}
	a.startupMu.Lock()
	defer a.startupMu.Unlock()
	if a.shutdownStarted {
		return
	}
	if a.startupReady == nil {
		a.startupReady = make(chan struct{})
	}
	a.startupArmed = true
}

func (a *DesktopApp) beginStartup(parent context.Context) (context.Context, bool) {
	if a == nil {
		return nil, false
	}
	if parent == nil {
		parent = context.Background()
	}
	a.startupMu.Lock()
	defer a.startupMu.Unlock()
	if a.shutdownStarted || a.startupStarted {
		return nil, false
	}
	if a.startupReady == nil {
		a.startupReady = make(chan struct{})
	}
	a.startupArmed = true
	a.startupStarted = true
	startupCtx, cancel := context.WithCancel(parent)
	a.startupCancel = cancel
	a.acceptingEvents.Store(true)
	// Startup is short and must be ordered under the same gate as shutdown;
	// infrastructure initialization continues outside this lock.
	a.application.Startup(parent)
	return startupCtx, true
}

func (a *DesktopApp) beginShutdown() {
	if a == nil {
		return
	}
	a.startupMu.Lock()
	if a.shutdownStarted {
		a.acceptingEvents.Store(false)
		a.startupMu.Unlock()
		return
	}
	a.shutdownStarted = true
	a.acceptingEvents.Store(false)
	cancel := a.startupCancel
	a.startupMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *DesktopApp) finishStartup() {
	if a == nil {
		return
	}
	a.startupMu.Lock()
	defer a.startupMu.Unlock()
	a.startupFinished = true
	if !a.shutdownStarted {
		a.closeStartupReadyLocked()
	}
}

func (a *DesktopApp) finishShutdown() {
	if a == nil {
		return
	}
	if a.application.State() != application.StateStopped {
		return
	}
	a.startupMu.Lock()
	defer a.startupMu.Unlock()
	a.closeStartupReadyLocked()
}

func (a *DesktopApp) closeStartupReadyLocked() {
	if !a.startupArmed || a.startupReady == nil {
		return
	}
	a.startupReadyOnce.Do(func() { close(a.startupReady) })
}

func (a *DesktopApp) acceptanceHookAllowed() bool {
	if a == nil {
		return false
	}
	a.startupMu.Lock()
	defer a.startupMu.Unlock()
	return a.startupStarted && !a.shutdownStarted
}

func (a *DesktopApp) awaitStartup() {
	if a == nil {
		return
	}
	a.startupMu.Lock()
	armed := a.startupArmed
	ready := a.startupReady
	a.startupMu.Unlock()
	if armed && ready != nil {
		<-ready
	}
}

// GetBootstrap 返回不含凭据和完整流地址的前端启动信息。
func (a *DesktopApp) GetBootstrap() application.BootstrapDTO {
	a.awaitStartup()
	return a.application.Bootstrap()
}

// GetState 为诊断和窗口生命周期测试提供稳定状态，不暴露内部对象。
func (a *DesktopApp) GetState() application.State {
	return a.application.State()
}

func (a *DesktopApp) ListRooms() ([]room.RoomConfig, error) {
	service, err := a.roomService()
	if err != nil {
		return nil, err
	}
	return service.ListRooms(a.application.Context())
}

func (a *DesktopApp) GetRoom(id string) (room.RoomConfig, error) {
	service, err := a.roomService()
	if err != nil {
		return room.RoomConfig{}, err
	}
	return service.GetRoom(a.application.Context(), id)
}

func (a *DesktopApp) CreateRoom(input room.CreateRoomInput) (room.RoomConfig, error) {
	service, err := a.roomService()
	if err != nil {
		return room.RoomConfig{}, err
	}
	created, err := service.CreateRoom(a.application.Context(), input)
	if err != nil {
		return room.RoomConfig{}, err
	}
	if created.MonitorEnabled {
		manager, managerErr := a.monitorManager()
		if managerErr != nil {
			return created, managerErr
		}
		err = manager.ReconcileRoom(a.application.Context(), created.ID)
	}
	return created, err
}

func (a *DesktopApp) UpdateRoom(id string, input room.UpdateRoomInput) (room.RoomConfig, error) {
	service, err := a.roomService()
	if err != nil {
		return room.RoomConfig{}, err
	}
	updated, err := service.UpdateRoom(a.application.Context(), id, input)
	if err != nil {
		return room.RoomConfig{}, err
	}
	if manager := a.application.MonitorManager(); manager != nil {
		err = manager.ReconcileRoom(a.application.Context(), id)
	}
	return updated, err
}

func (a *DesktopApp) DeleteRoom(id string, deleteData bool) error {
	service, err := a.roomService()
	if err != nil {
		return err
	}
	manager := a.application.MonitorManager()
	if manager != nil {
		if err := manager.DetachRoom(a.application.Context(), id); err != nil {
			return err
		}
	}
	if err := service.DeleteRoom(a.application.Context(), id, deleteData); err != nil {
		if manager != nil {
			_ = manager.ReconcileRoom(a.application.Context(), id)
		}
		return err
	}
	return nil
}

func (a *DesktopApp) StartMonitoring(id string) error {
	manager, err := a.monitorManager()
	if err != nil {
		return err
	}
	return manager.StartMonitoring(a.application.Context(), id)
}

func (a *DesktopApp) StopMonitoring(id string) error {
	manager, err := a.monitorManager()
	if err != nil {
		return err
	}
	return manager.StopMonitoring(a.application.Context(), id)
}

func (a *DesktopApp) GetRoomStatus(id string) (room.RoomRuntimeStatus, error) {
	manager, err := a.monitorManager()
	if err != nil {
		return room.RoomRuntimeStatus{}, err
	}
	return manager.GetRoomStatus(a.application.Context(), id)
}

func (a *DesktopApp) SetRoomCookie(input room.SetRoomCookieInput) (room.CookieStatus, error) {
	service, err := a.roomService()
	if err != nil {
		return room.CookieStatus{}, err
	}
	return service.SetRoomCookie(a.application.Context(), input)
}

func (a *DesktopApp) ClearRoomCookie(id string) error {
	service, err := a.roomService()
	if err != nil {
		return err
	}
	return service.ClearRoomCookie(a.application.Context(), id)
}

func (a *DesktopApp) GetSettings() (settings.AppSettings, error) {
	service, err := a.settingsService()
	if err != nil {
		return settings.AppSettings{}, err
	}
	return service.GetSettings(a.application.Context())
}

func (a *DesktopApp) UpdateSettings(input settings.UpdateSettingsInput) (settings.AppSettings, error) {
	service, err := a.settingsService()
	if err != nil {
		return settings.AppSettings{}, err
	}
	updated, err := service.UpdateSettings(a.application.Context(), input)
	if err != nil {
		return settings.AppSettings{}, err
	}
	if manager := a.application.EventStoreManager(); manager != nil {
		manager.SetStoreDisplayName(updated.SaveDisplayNames)
	}
	return updated, nil
}

func (a *DesktopApp) roomService() (*room.Service, error) {
	service := a.application.RoomService()
	if service == nil {
		return nil, errors.New("ROOM_SERVICE_UNAVAILABLE: 房间服务尚未就绪")
	}
	return service, nil
}

func (a *DesktopApp) settingsService() (*settings.Service, error) {
	service := a.application.SettingsService()
	if service == nil {
		return nil, errors.New("SETTINGS_SERVICE_UNAVAILABLE: 设置服务尚未就绪")
	}
	return service, nil
}

func (a *DesktopApp) monitorManager() (*room.MonitorManager, error) {
	manager := a.application.MonitorManager()
	if manager == nil {
		return nil, errors.New("MONITOR_SERVICE_UNAVAILABLE: 监控服务尚未就绪")
	}
	return manager, nil
}
