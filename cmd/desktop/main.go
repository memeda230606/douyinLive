package main

import (
	"log"

	frontendassets "github.com/jwwsjlm/douyinLive/v2/frontend"
	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

var version = "dev"

func main() {
	core := application.New(application.Options{Name: "抖音直播分析", Version: version})
	desktop := NewDesktopApp(core)
	desktop.armStartup()

	err := wails.Run(&options.App{
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
		},
		OnStartup:  desktop.startup,
		OnShutdown: desktop.shutdown,
		Bind:       []interface{}{desktop},
	})
	if err != nil {
		log.Printf("桌面应用启动失败: %v", err)
	}
}
