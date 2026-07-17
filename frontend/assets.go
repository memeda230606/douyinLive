package frontend

import (
	"embed"
	"io/fs"
)

// Assets 由 Wails 嵌入桌面二进制。dist/.gitkeep 使纯 Go 门禁在全新检出中也可编译。
//
//go:embed all:dist
var embeddedAssets embed.FS

// Assets 以 dist 为根，确保 Wails 直接读取 index.html，而不依赖路径猜测。
var Assets = mustSub(embeddedAssets, "dist")

func mustSub(source fs.FS, directory string) fs.FS {
	result, err := fs.Sub(source, directory)
	if err != nil {
		panic(err)
	}
	return result
}
