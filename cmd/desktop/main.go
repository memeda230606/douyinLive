package main

import (
	"log"

	frontendassets "github.com/jwwsjlm/douyinLive/v2/frontend"
	"github.com/jwwsjlm/douyinLive/v2/internal/analysis"
	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
	"github.com/jwwsjlm/douyinLive/v2/internal/buildinfo"
	"github.com/jwwsjlm/douyinLive/v2/internal/settings"
	"github.com/jwwsjlm/douyinLive/v2/internal/storage"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

func main() {
	build := buildinfo.Current(
		storage.LatestSchemaVersion(), settings.SettingsVersion,
		analysis.AlgorithmVersion, analysis.ExportSchema,
	)
	core := application.New(application.Options{Name: "抖音直播分析", Build: build})
	infrastructureOptions, err := desktopInfrastructureOptions()
	if err != nil {
		log.Printf("桌面验收配置无效: P3ACC_CONFIG_INVALID")
		return
	}
	windowsOptions, err := desktopWindowsOptions()
	if err != nil {
		log.Printf("桌面验收配置无效: P3ACC_CONFIG_INVALID")
		return
	}
	desktop := newDesktopApp(core, infrastructureOptions)
	desktop.armStartup()

	err = wails.Run(&options.App{
		Title:             "抖音直播分析",
		Width:             1440,
		Height:            900,
		MinWidth:          1024,
		MinHeight:         700,
		DisableResize:     false,
		Fullscreen:        false,
		Frameless:         false,
		StartHidden:       false,
		HideWindowOnClose: false,
		BackgroundColour:  &options.RGBA{R: 14, G: 20, B: 32, A: 1},
		AssetServer: &assetserver.Options{
			Assets: frontendassets.Assets,
			Middleware: assetserver.ChainMiddleware(
				desktop.securityHeadersMiddleware,
				desktop.playbackMediaMiddleware,
			),
		},
		Windows:    windowsOptions,
		OnStartup:  desktop.startup,
		OnShutdown: desktop.shutdown,
		Bind:       []interface{}{desktop},
	})
	if err != nil {
		log.Printf("桌面应用启动失败: %v", err)
	}
}
