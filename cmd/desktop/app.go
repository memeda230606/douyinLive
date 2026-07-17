package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const shutdownTimeout = 10 * time.Second

// DesktopApp 是 Wails 唯一绑定门面。业务能力由 internal/app 及后续服务实现。
type DesktopApp struct {
	application           *application.Application
	infrastructureOptions application.InfrastructureOptions
}

func NewDesktopApp(app *application.Application) *DesktopApp {
	return newDesktopApp(app, application.InfrastructureOptions{})
}

func newDesktopApp(app *application.Application, options application.InfrastructureOptions) *DesktopApp {
	return &DesktopApp{application: app, infrastructureOptions: options}
}

func (a *DesktopApp) startup(ctx context.Context) {
	a.application.SetRoomStatusPublisher(func(status room.RoomRuntimeStatus) {
		runtime.EventsEmit(ctx, room.StatusEventName, status)
	})
	a.application.Startup(ctx)
	if err := a.application.InitializeInfrastructure(ctx, a.infrastructureOptions); err != nil {
		slog.Error("桌面数据基础初始化失败",
			"component", "desktop",
			"error_code", "DATABASE_INIT_FAILED",
			"correlation_id", "startup",
		)
	} else {
		a.startAcceptanceHook(ctx)
	}
}

func (a *DesktopApp) shutdown(context.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := a.application.Shutdown(ctx); err != nil {
		slog.Error("桌面应用关闭失败", "component", "desktop", "error_code", "SHUTDOWN_FAILED")
	}
}

// GetBootstrap 返回不含凭据和完整流地址的前端启动信息。
func (a *DesktopApp) GetBootstrap() application.BootstrapDTO {
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
	return service.UpdateSettings(a.application.Context(), input)
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
