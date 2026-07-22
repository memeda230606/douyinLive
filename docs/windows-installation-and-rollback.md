# Windows 安装、升级、卸载与数据库回滚

本文适用于 DouyinLive Desktop 0.1.x 的 Windows 10/11 x64 安装包。开发验证包未签名；正式发布候选必须在可复现构建完成后签名，并在发布清单中同时记录签名前后 SHA-256。

## 安装内容与边界

- 安装范围固定为当前用户，不请求管理员权限，默认目录为 `%LOCALAPPDATA%\Programs\DouyinLive Desktop`。
- 安装目录包含桌面主程序、`ffmpeg\ffmpeg.exe`、`ffmpeg\ffprobe.exe`、离线数据库回滚工具和 `licenses` 下的 SBOM、许可证清单、第三方 notices、FFmpeg 锁及本文。
- 安装包只支持原生 Windows x64；不包含 Go、Node、pnpm、源码、Cookie、有效流 URL 或测试数据。
- 业务数据固定在 `%LOCALAPPDATA%\DouyinLive`，与程序目录分离；用户选择的外部媒体目录始终由用户自行管理。

## 首次安装与 WebView2

安装器会在写入任何程序文件前检查 Windows 版本、CPU 架构和 Microsoft Edge WebView2 Evergreen Runtime。WebView2 缺失时，交互安装会给出说明并只打开微软官方 Evergreen Runtime 地址；安装器随后以失败结束，用户安装运行时后需重新执行安装包。静默安装不会访问浏览器，返回退出码 `74`。安装器不从第三方镜像下载运行时。

安装成功后应核对“应用和功能”中的版本、安装目录、桌面/开始菜单快捷方式，以及安装目录内主程序、两项 FFmpeg 工具、回滚工具和六项合规文档。FFmpeg 的 SHA-256 必须与随附锁文件一致。

## 覆盖升级

升级使用同一当前用户安装目录，覆盖程序和随附工具，不修改 `%LOCALAPPDATA%\DouyinLive` 或任何外部媒体目录。启动新版本时：

1. SQLite 先执行 `quick_check`；
2. 若现有 schema 低于程序目标，先在 `backups` 创建 `VACUUM INTO` 一致性备份；
3. 每个迁移在独立事务中执行，失败时回滚该事务并停止启动；
4. 迁移完成后再次执行 `quick_check`。

升级前应退出桌面程序并另存重要外部媒体。安装器不会自动降级数据库 schema。

## 卸载与数据保留

默认卸载只移除程序目录、快捷方式、卸载注册表项和 WebView2 应用缓存；数据库、内部媒体、配置、日志、导出和备份全部保留。卸载器的可选组件页提供“删除数据库、内部媒体、配置和日志”，默认不选中；选择后还会显示预计大小并要求第二次确认。外部媒体目录永不由卸载器删除。

静默卸载没有受支持的数据删除开关，`uninstall.exe /S` 始终保留数据；不可恢复删除只允许在交互式可选组件页完成二次确认。NSIS 会把卸载器复制到临时目录执行，且启动包装进程可能早于真实临时卸载进程退出并返回 `0`，所以自动化必须有界等待安装目录与卸载注册表达到预期终态，不能只看包装进程退出码。

## 使用升级前备份回滚数据库

数据库回滚必须与旧版本程序配套，不能让旧程序直接打开较新的 schema。先完全退出桌面程序并确认没有 `app.db-wal` 或 `app.db-shm`，再从安装目录运行：

```powershell
./douyin-live-dbrollback.exe -backup "$env:LOCALAPPDATA\DouyinLive\backups\app-v5-YYYYMMDDTHHMMSS.mmmZ.db" -confirm RESTORE_BACKUP
```

工具只接受数据根 `backups` 的直接子文件和严格的 `app-vN-UTC.db` 名称；它校验当前库和备份的 `quick_check`、schema 顺序、常规文件类型以及 WAL/SHM 离线状态。通过后先把当前 `app.db` 同卷改名为 `app.db.pre-rollback-<UTC>`，再原子发布备份副本并复验。任一步失败会恢复原数据库；成功后仍保留被替换的新库，人工确认旧程序和数据无误后再决定是否删除。

若使用非默认数据根，额外传入绝对 `-data-root`。命令输出只报告 schema 版本和保留文件的 basename，不输出配置、媒体路径或业务内容。

## 固定退出码

| 退出码 | 含义 | 操作 |
| ---: | --- | --- |
| 64 | Windows 版本不支持 | 使用 Windows 10/11 |
| 65 | 不是原生 x64 | 使用 x64 安装包或等待独立架构版本 |
| 74 | 缺少 WebView2 | 从微软官方地址安装 Evergreen Runtime 后重试 |
| 75 | 数据清理未二次确认或未完成 | 保留数据，或明确复核后重新选择清理 |

安装、升级、卸载或回滚失败时，不要手工覆盖 `app.db`、删除 WAL/SHM 或清理外部媒体；保留安装器退出码、脱敏日志和 `backups`，再使用诊断包处理。
