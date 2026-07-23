# P5 Windows 一键安装验证记录

日期：2026-07-23

## 1. 目标

让首次使用的新 Windows 10/11 x64 用户只运行一个安装程序即可完成应用安装。安装包继续内置主程序、FFmpeg/ffprobe、数据库回滚工具和发布文档，并补齐 WebView2 缺失时的自动安装。

## 2. WebView2 供应链门禁

正式构建使用 Microsoft 官方 Evergreen Bootstrapper 下载入口，并将其身份锁定在 `build/webview2-bootstrapper-windows.lock.json`：

- 文件：`MicrosoftEdgeWebview2Setup.exe`
- 文件大小：1,691,856 字节
- SHA-256：`0223fa1e8d5bd5e4344fb8734e60d088e79f262c0a24444d01f240bc996f04e5`
- Authenticode：`Valid`
- 签名主体：`CN=Microsoft Corporation, O=Microsoft Corporation, L=Redmond, S=Washington, C=US`

构建脚本和 Go 发布门禁均要求精确大小与 SHA-256 一致；Windows 构建脚本还要求 Microsoft Authenticode 签名有效。任何一项不匹配都失败关闭。

## 3. 安装行为

NSIS 在复制应用文件前检测 WebView2：

1. 已安装时直接进入正常安装。
2. 缺失时从安装包临时释放锁定的官方 Bootstrapper，以 `/silent /install` 静默运行。
3. 安装完成后进行有界复检。
4. 引导程序失败或复检仍不可用时返回固定失败，不进入应用文件写入阶段。

Bootstrapper 安装 WebView2 时需要访问 Microsoft 服务。完全离线环境必须提前安装 WebView2 Evergreen Standalone Runtime；应用其余依赖均已随安装包提供。

## 4. 构建结果

执行：

- `scripts/build-release.ps1 -Version 0.1.0 -Output release -Source local-one-click-installer -WebView2Bootstrapper D:/douyinLive-deps/webview2/MicrosoftEdgeWebview2Setup.exe -AllowDirty`
- Wails production 构建连续两次 hash/size 一致。
- 发布门禁输出 `RELEASE_GATE_PASSED`。

正式安装程序：

- 路径：`D:\douyinLive\release\v0.1.0\douyin-live-desktop-0.1.0-windows-amd64-installer.exe`
- 大小：94,597,359 字节
- SHA-256：`64cde79b412724ed70cebef559b73af99e9354c13cabf935abac581076781263`
- Authenticode：`NotSigned`；当前内部可运行交付范围允许未签名，公开发行仍需签名。

## 5. 验证结果

`scripts/test-windows-installer.ps1` 使用独立临时目录、独立卸载键和测试专用编译分支执行，最终输出 `WINDOWS_INSTALLER_MATRIX_PASSED`，7/7 通过：

- `fresh-install`
- `in-place-upgrade`
- `uninstall-preserves-data`
- `purge-needs-second-confirmation`
- `confirmed-purge`
