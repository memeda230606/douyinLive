# P5-WIN-001 Windows 安装、升级、卸载与数据库回滚验证

> 日期：2026-07-22
> 权威工作副本：`GJS-20250801EFK:D:\douyinLive`
> 验证系统：Microsoft Windows 11 x64（10.0.22631.2861）
> 工具链：Go 1.26.4、Wails 2.13.0、Node 24.18.0、pnpm 9.12.3、FFmpeg/ffprobe 8.1.2、NSIS 3.12

## 1. 结论

P5-WIN-001 完成。发布门禁现在生成当前用户范围的 Windows x64 NSIS 安装包，同时随附锁定的 FFmpeg/ffprobe、离线数据库回滚工具、SPDX SBOM、许可证清单、第三方 notices、FFmpeg 锁和安装/回滚说明。安装、覆盖升级、默认卸载保留、交互式二次确认清理、WebView2 缺失和数据库 schema 回滚均有可重复自动化证据。

本次验证不宣称 Windows 10 实机或 arm64 通过；安装器对低于 Windows 10 和非原生 x64 分别 fail closed 为固定退出码 64/65。当前发行目标仍为 Windows 10/11 x64，arm64 未进入发布范围。

## 2. 安装包合同

- `project.nsi` 固定 `RequestExecutionLevel user`，程序目录与 `%LOCALAPPDATA%\DouyinLive` 数据目录分离。
- 编译使用 `/WX /INPUTCHARSET UTF8`；任一 NSIS warning 都阻止发布，避免 UTF-8 无 BOM 脚本被默认 ACP 错解。
- 主程序、回滚工具和 FFmpeg 位于固定相对位置；安装后的 FFmpeg SHA-256 必须与 `ffmpeg-windows-amd64.lock.json` 完全一致。
- WebView2 检查发生在写文件之前；缺失时交互安装只打开微软官方 Evergreen Runtime 地址，静默安装返回 74 且不产生安装目录或卸载键。
- 默认卸载删除程序、快捷方式、卸载键和 WebView2 应用缓存，但保留数据库、内部媒体、配置、日志、导出与备份。不可恢复数据删除只在交互式可选组件页经第二次确认执行；静默卸载没有数据删除开关。
- release manifest 同时记录便携 EXE、安装包、回滚工具及每个输出文件的 SHA-256/大小；GitHub Windows tag 作业固定校验 NSIS 3.12 并运行同一隔离矩阵。

## 3. 隔离安装矩阵

命令：

```powershell
./scripts/test-windows-installer.ps1 -ReleaseDirectory release/v0.1.0 -CurrentVersion 0.1.0
```

每次运行生成独立 32 位十六进制 nonce，产品名、HKCU 卸载键、程序目录、数据目录、快捷方式和安装包均隔离。矩阵输出 `WINDOWS_INSTALLER_MATRIX_PASSED`，6 项均通过：

1. `fresh-install`：0.0.9 当前用户安装，11 项必要 payload 与卸载版本存在，FFmpeg hash 匹配。
2. `in-place-upgrade`：同目录升级到 0.1.0，卸载版本更新，数据 sentinel 保留。
3. `uninstall-preserves-data`：普通静默卸载后程序目录和 32/64 位卸载键收敛为零，数据 sentinel 保留。
4. `purge-needs-second-confirmation`：测试专用直达分支仅给出第一确认时返回 75，程序和数据均不改变。
5. `confirmed-purge`：双确认分支删除程序和隔离数据根，卸载键归零。
6. `webview2-missing`：强制缺失分支返回 74，安装目录和卸载键均不存在。

NSIS 卸载包装进程会复制到 `%TEMP%\~nsu*.tmp\Un.exe`，只转发标准 `/S`，并可能早于真实子进程返回 0。矩阵因此对普通卸载执行有界终态等待；测试双确认分支使用仓库外复制的隔离卸载器和 `_?=` 直达执行，以取得真实子进程退出码。每轮结束验证矩阵临时根、桌面/开始菜单快捷方式和 `DouyinLiveMatrix*` 卸载键为零。

## 4. 数据库升级与回滚

新增 `douyin-live-dbrollback.exe` 和 `storage.RestoreBackup`：

- 只接受绝对数据根 `backups` 下、严格命名的直接常规文件；拒绝外部路径、符号链接、损坏库、新于当前库的 schema 以及活动 `app.db-wal`/`app.db-shm`。
- 当前库和备份均执行只读打开、`quick_check` 与 schema 校验；备份先复制、Sync 并复验。
- 发布时先把当前 `app.db` 同卷改名为 `app.db.pre-rollback-<UTC>`，再原子发布备份；发布或复验失败会恢复原库。
- 成功后保留被替换的较新数据库，仅输出 schema 和 basename，不输出数据根、媒体或业务内容。

`go test ./internal/storage ./internal/releasegate -count=20` 通过；其中真实 v5→v6 启动升级先生成 v5 一致性备份，离线回滚恢复 v5 并保留可重新打开的 v6 数据库，外部备份、活动 sidecar 和损坏备份均 fail closed。既有迁移失败事务回滚门禁继续通过。

## 5. 完整门禁与产物

- `go test ./...`：通过。
- `go vet ./...`、`go build ./...`：通过。
- 前端 `typecheck`、10 文件 36 项 Vitest、production build：通过。
- 发布构建连续两次 production Wails EXE 一致；最终安装包由 NSIS `/WX` 成功生成。
- 便携 EXE：`48669696` 字节，SHA-256 `241e5e0e68b4e7cab78d49719c8648f53890b3c65b0f2ea21368b19e215426a4`。
- 数据库回滚工具：`6693376` 字节，SHA-256 `2e1030f9f725a618f49b7ffdb6caa223ff878f2255b806495b59e60f2dc38768`。
- Windows x64 安装包：`93001336` 字节，SHA-256 `a2d3364d9bc607fc90739ec068e321eea171e21b2619f92cc824612aa6f16372`。
- release manifest：250 个组件，最终暂存快照敏感扫描 `396` 个跟踪文本文件、0 命中。
- 当前 OpenSSH 环境 `CGO_ENABLED=0` 且无 GCC，`go test -race` 未启动；这是已知工具链限制，不是源码测试失败。

## 6. 剩余发布工作

P5-WIN-001 关闭后，PHASE-5 完成 6/10 点，项目总完成度 96%。下一任务 P5-STB-001 将执行 60 分钟多房间等待/模拟开播稳定性、每分钟资源趋势与数据库忙、磁盘满、网络、强制退出恢复门禁；Windows 10 实机和正式代码签名仍属于最终发布环境证据，不在本任务中冒充完成。
