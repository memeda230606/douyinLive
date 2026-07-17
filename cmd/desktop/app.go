package main

import (
	"context"
	"log/slog"
	"time"

	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
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
	a.application.Startup(ctx)
	if err := a.application.InitializeInfrastructure(ctx, a.infrastructureOptions); err != nil {
		slog.Error("桌面数据基础初始化失败",
			"component", "desktop",
			"error_code", "DATABASE_INIT_FAILED",
			"correlation_id", "startup",
		)
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
