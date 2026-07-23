# 工程、测试与 Windows 发布计划

> 上级计划：[总开发计划](00-master-development-plan.md)
> 相关计划：[桌面 UI](01-desktop-ui-development-plan.md) · [采集与录制](02-capture-and-recording-development-plan.md) · [数据与分析](03-data-and-analysis-development-plan.md)
> 实施状态（2026-07-23）：P6-UPD-001 的 OSS、签名协议、客户端、独立更新助手、发布工具与设置 UI 已实现并通过自动化门禁；0.2.0 人工引导安装和 0.2.1 真实 canary 升级/24 小时观察仍按发布顺序执行。
> 最近验收：[P6 OSS 自动更新实现记录](validation/2026-07-23-p6-oss-auto-update.md)

## 1. 目标

本计划定义开发环境、依赖治理、自动化测试、故障注入、性能基线、Windows 安装包、升级回滚和发布门禁。目标是在不破坏现有 Go 库与 WebSocket 服务的前提下，持续交付可安装、可恢复、可诊断的桌面程序。

## 2. 权威环境与操作约束

- 权威代码工作副本固定为 Windows 主机 `GJS-20250801EFK` 的 `D:\douyinLive`。
- 所有代码修改、调试、测试、构建和维护均在该目录通过 SSH 完成。
- 2026-07-16 已完成初始工具链：OpenSSH 可找到 Go 1.26.4、Wails 2.13.0、Node 24.18.0、npm 11.16.0、pnpm 9.12.3、FFmpeg/ffprobe 8.1.2 和 NSIS 3.12；`wails doctor` 识别 WebView2 150.0.4078.65 并判定开发环境就绪。
- 运行任何 Go/Wails 命令前先执行 `where go`；找不到时报告“工具链不可用”，不能把启动失败归类为源码测试失败。
- 初始安装与必要 `PATH` 调整已于 2026-07-16 获得用户授权并完成；后续工具链升级、卸载或全局版本切换仍需用户明确授权。
- 文档和 fixture 不保存真实 Cookie、签名、有效流 URL、主播隐私或原始平台用户 ID。

## 3. 工具链基线

| 组件 | 基线 | 验证命令 | 策略 |
| --- | --- | --- | --- |
| Go | 1.26.4（与 `go.mod` 一致） | `where go`、`go version` | 官方 MSI SHA-256 `55902c036634c7ab3159cf259af692abc86989aaefcc7f75bef888f3263031c4` |
| Wails | v2.13.0 | `wails version`、`wails doctor` | v3 稳定前不采用 |
| Node.js | 24.18.0 LTS | `node --version` | 由现有 NVM for Windows 管理；使用项目声明和锁文件 |
| pnpm | 当前 9.12.3；前端创建时由 `packageManager` 固定 | `pnpm --version` | 禁止混用 npm/yarn 更新锁文件 |
| WebView2 | Evergreen Runtime（验证版本 150.0.4078.65） | `wails doctor` | 安装包锁定并随附官方 Evergreen Bootstrapper；缺失时静默安装并复检 |
| FFmpeg/ffprobe | Gyan 8.1.2 Essentials | `ffmpeg -version`、`ffprobe -version` | ZIP SHA-256 `db580001caa24ac104c8cb856cd113a87b0a443f7bdf47d8c12b1d740584a2ec`；GPLv3 |
| NSIS | 3.12 | `makensis /VERSION` | 仅用于 Windows 安装包；Wails doctor 已识别 |
| Git | 支持当前仓库 | `git --version` | 构建注入 commit |

Go 和 Node 版本同时记录在构建产物元数据中。依赖升级通过单独变更完成，不与业务功能混合；升级必须包含 changelog 审阅、许可证扫描和回归结果。

## 4. 开发前置检查

在 Windows 同一个 SSH 会话中依次检查：

```bat
where go
go version
where node
node --version
where pnpm
pnpm --version
where ffmpeg
where ffprobe
where wails
wails doctor
```

缺失项处理：

- Go 缺失：停止 Go/Wails 构建步骤，记录环境阻塞；不自行安装或改 PATH。
- FFmpeg 缺失：允许编译不含随附二进制的开发版本，但录制集成测试标记“未运行”，不能判定通过。
- WebView2 缺失：后端单元测试可继续，桌面 E2E 和安装包验证阻塞。
- Node/pnpm 缺失或版本不匹配：不生成或手改依赖锁文件。

## 5. 构建结构与命令

### 5.1 保留入口

- `go test ./...` 必须覆盖根库和现有 `cmd/main`。
- `go build ./cmd/main` 继续生成原 WebSocket 服务。
- 桌面入口位于 `cmd/desktop`，Wails 配置位于仓库根或桌面专用配置文件。
- 前端构建产物由 Wails 嵌入，不提交 `node_modules` 或临时 dist。

### 5.2 建议命令

```bat
go test ./...
pnpm --dir frontend install --frozen-lockfile
pnpm --dir frontend typecheck
pnpm --dir frontend test
pnpm --dir frontend build
wails build -clean
```

开发模式使用 Wails 的开发服务器，但不得让前端直接连接公网抖音接口。发布构建禁用开发工具和任意远程导航。

### 5.3 版本信息

桌面“关于”和诊断包记录：

- 产品 SemVer、Git commit、构建时间、构建来源。
- Go、Wails、Node 前端构建版本。
- FFmpeg 版本与 SHA-256。
- 数据库 schema、报告算法和配置 schema 版本。

## 6. 依赖与供应链

- Go 使用 `go.mod/go.sum`；前端使用唯一 `pnpm-lock.yaml`。
- CI 使用只读/冻结锁文件安装，禁止隐式更新。
- 发布生成 SBOM，列出 Go modules、npm packages、Wails、WebView2 依赖说明和 FFmpeg 构建许可证。
- FFmpeg 优先选用满足当前功能且许可可分发的构建；是否包含 GPL 组件必须由许可证清单明确，不能仅写“FFmpeg”。
- 随附二进制在构建配置中固定下载来源、版本和 SHA-256；构建时校验，不从不可信镜像动态获取。
- 前端 CSP 默认 `default-src 'self'`，禁止任意远程脚本；需要打开外链时交给系统浏览器。

## 7. 测试分层

### 7.1 快速门禁

每次提交运行：

- Go 单元测试和 race-sensitive 组件的并发测试。
- 前端类型检查和 Vitest；当前 `package.json` 未定义 lint 脚本，后续引入后再纳入门禁，不能把未运行的 lint 写成通过。
- 流解析、状态机、数据迁移和指标 golden test。
- 敏感字符串扫描和 Markdown 链接检查。

目标：10 分钟内完成，任何失败阻止合并。

### 7.2 集成测试

- 用本地 HTTP/WebSocket fixture 模拟直播页、`web/enter`、消息流、断线和下播。
- 用本地 FFmpeg 生成测试源，验证分片、探测、封装和音频代理。
- 使用临时数据根运行 SQLite、spool、崩溃恢复和导出。
- Wails 绑定层使用 fake services 验证 DTO、错误码和事件节流。

测试不得依赖仍有效的真实流 URL。访问真实直播间的测试只允许手工、短时、用户授权，并且结果不进入仓库。

### 7.3 桌面 E2E

- 首次启动与目录选择。
- 房间新增、编辑、删除、启停监控。
- 模拟开播、实时事件、录制、重连、下播收尾和历史回放。
- 报告生成、取消、重试和导出。
- 托盘、关闭确认、应用重启和未完成场次恢复。
- Windows 100%/150%/200% DPI、深浅主题和最小窗口。

### 7.4 限时稳定性测试

- 10 分钟录制测试作为每次录制器变更的门禁。
- 30 分钟单房间监听/周期录制测试作为候选发布门禁。
- 60 分钟多房间等待与模拟开播测试作为正式版本前稳定性验证。
- 每 1 分钟采样内存、CPU、goroutine、句柄、数据库 WAL、磁盘吞吐、事件队列和 UI 延迟。

### 7.5 P3-ACC 阶段关闭证据（2026-07-21）

- 真实在线运行证明十分钟稳定资源窗口、FFmpeg 崩溃恢复、隔离 relay 网络故障恢复、新 attempt 与 gap。
- 人工停止与 UI finalizing 为 `USER_OBSERVED`；自然下播等待和最终机器视觉 ACK 由用户明确豁免，记录为 `USER_WAIVED/NOT_RUN`。
- 不声明严格控制器 `PASS`/`passed=true`，不把人工观察冒充 `PrintWindow` 或机器视觉结果；严格控制器合同保持不变。
- default、`p2acceptance`、`p3acceptance`、`p3uiacceptance`、`p3accacceptance` 的 Go test/vet/build，前端 20 项测试/typecheck/build，两套 PowerShell、显式 Scheduled Task/MIC、`cmd/main` loopback 与四产物构建通过。
- 当前 OpenSSH 为 `CGO_ENABLED=0` 且无 GCC，`go test -race` 未启动；这是工具链限制，不是源码失败。
- 任务、相关进程、交互式测试临时目录为零；正式数据根保留，因本次没有约 19 GB 不可恢复删除授权，不声明控制器零数据残留。
- 本次例外只关闭项目阶段，不形成自动验收成功分支；正式发布门禁仍可要求按原合同完整复验。

### 7.6 P4 一体化验收证据（2026-07-22）

- `p4accacceptance` 仅在验收标签暴露隔离夹具；同一次交互式冷启动严格结果 9/9，通过真实两段媒体解码、跨段续播、校准时间轴、报告复用、ASR disabled、5 文件隐私导出和布局检查。
- 精确窗口使用 `PrintWindow(PW_RENDERFULLCONTENT)` 生成 1440×900 PNG，结果内 SHA-256 与文件复算均为 `c6d4d4dcc632e79a4365303608b3bc46a4b208de4f7e52c23afe33ba6326c05e`，并完成人工视觉复核。
- Go 全量 test/vet/build、标签 test/vet、前端 10 文件 36 项测试/typecheck/build和 production Wails 均通过；生产 EXE 为 48,712,704 字节，SHA-256 `c509622776f43db78ddd2589bb91a1d5e3d74110477ce50343b0c018c45be2a8`。
- 验收计划任务、精确应用进程与隔离运行根全部清零；race 因当前 `CGO_ENABLED=0` 且无 GCC 未启动。完整事实见[P4 一体化验收记录](validation/2026-07-22-p4-phase-acceptance.md)。

### 7.7 P5 发布工程验证证据（2026-07-22）

- 发布构建器从精确 SemVer、完整 Git commit 与 commit 时间生成不可变身份；默认拒绝脏工作树，使用 `-trimpath`、空 build ID 和固定 `SOURCE_DATE_EPOCH` 连续构建两次并要求 EXE hash/size 完全一致。
- 本地开发验证的两次 production Wails EXE 均为 48,669,184 字节，SHA-256 均为 `b3dcb7eb25c994b0cc6de28402fa5849db5670ae93529fa52f1ba0beedc4db16`；该验证显式允许脏工作树，正式 tag/CI 仍强制清洁构建。
- 诊断页严格展示产品版本、完整 commit、构建时间/来源、Go/Wails/Node、FFmpeg 版本/hash/许可证和数据库、设置、分析、导出 schema；WebView 增加 CSP、`nosniff`、拒绝 frame 与 no-referrer 安全头。
- 实际桌面编译闭包与前端安装闭包共 250 个组件进入 SPDX 2.3 SBOM、许可证 JSON 和第三方 notices；上游未携带许可证声明的 1 个间接模块明确记录为 `NOASSERTION`，不猜测或遗漏。
- 锁定 Gyan FFmpeg 8.1.2 Essentials 的官方归档来源、归档 SHA-256、ffmpeg/ffprobe SHA-256 与 GPL-3.0-or-later；最终暂存快照 389 个跟踪文本文件敏感扫描为 0 命中，历史巨型响应样例改为脱敏 fixture 索引。
- GitHub tag 流程新增 Windows 双构建、锁定 FFmpeg 下载校验、证据归档与 Release 附件上传；全量 Go test/vet/build、前端 10 文件 36 项测试/typecheck/build 和发布门禁均通过。race 因当前 `CGO_ENABLED=0` 且无 GCC 未启动。完整事实见[P5 发布工程验证记录](validation/2026-07-22-p5-release-engineering.md)。

### 7.8 P5 Windows 安装升级验证证据（2026-07-22）

- 发布构建器新增 Windows x64 当前用户范围 NSIS 安装包，固定随附主程序、离线数据库回滚工具、锁定 FFmpeg/ffprobe、许可证、SPDX SBOM、第三方 notices、FFmpeg 锁和安装回滚说明；NSIS 以 `/WX /INPUTCHARSET UTF8` 编译。
- WebView2 在写文件前检查；交互缺失仅打开 Microsoft 官方 Evergreen Runtime 地址，静默缺失固定返回 74。低于 Windows 10 和非原生 x64 分别固定返回 64/65，但本次不冒充 Windows 10 实机或 arm64 验证。
- 独立 nonce 安装矩阵 `fresh-install`、`in-place-upgrade`、`uninstall-preserves-data`、`purge-needs-second-confirmation`、`confirmed-purge`、`webview2-missing` 共 6/6 通过；结束时矩阵目录、快捷方式和卸载键归零。
- 默认卸载不删除 `%LOCALAPPDATA%\DouyinLive` 业务数据，静默卸载没有数据删除开关；不可恢复删除只在交互式可选节经第二次确认执行。测试专用直达分支不编译进生产安装器。
- 新增 `douyin-live-dbrollback.exe` 和严格 `storage.RestoreBackup`：只接受数据根内合法一致备份，拒绝外部/链接/损坏/较新 schema 与活动 WAL/SHM，原子保留当前库后发布备份，失败恢复原库。v5→v6→v5 真实回滚与反例门禁通过。
- `go test ./...`、`go vet ./...`、`go build ./...`、前端 10 文件 36 项测试/typecheck/build、双次 production Wails 可复现构建和最终 NSIS 构建通过；race 因当前 `CGO_ENABLED=0` 且无 GCC 未启动。完整事实见[P5 Windows 安装升级验证记录](validation/2026-07-22-p5-windows-installation.md)。

### 7.9 P5 稳定性与发布故障验证证据（2026-07-22）

- `p5stbacceptance` 正式门禁必须由进程级 `P5STB_RUN_60M=1` 显式授权，固定运行 3600 秒并原子生成 `P5-STB-001/v1` 结果；本次 `P5STB_60M_RUNNING/PASSED` 齐全，测试体耗时 3601.37 秒，61 个分钟样本全部有效。
- 4 个合成直播间中 2 个持续等待、2 个周期模拟开播/下播，共形成 13 个真实 SQLite 场次和 18984 条持久化事件；状态发布 P95 延迟 0 ms，分钟采样事件队列均回到 0。
- 资源证据：平均 CPU 0.235%；工作集 36409344→62525440 字节、峰值 67682304；私有内存峰值 101707776；句柄 191→222、峰值 224；goroutine 10→11、峰值 15；WAL 峰值 4255992 字节，无超过阈值或持续失控增长。
- 数据库 `BEGIN IMMEDIATE` 忙门禁期间事件队列瞬时峰值 26，释放唯一连接后数据库可用且最终事件持久化；合成网络连接中断后观察到 `RECONNECTING` 并恢复 `LIVE`；子测试进程以退出码 93 强制结束后，同一数据根重启恢复活动场次为 0。
- Windows FFmpeg 磁盘满文本 `There is not enough space on the disk` 与通用 `Disk full` 现在稳定映射 `RECORDER_LOCAL_RESOURCE`，与既有永久本地资源错误“不重试录制但继续消息链路”回归共同通过；不泛化匹配未知数字错误。
- 结果 JSON SHA-256 为 `b0edc69d6f6b46d16afa7b1da24adaba344ccd02753ac7a4709dc0d4c0cbd5d9`；严格夹具只含合成数据，结果复核后删除独立运行根。race 仍因当前 `CGO_ENABLED=0` 且无 GCC 未启动。完整事实见[P5 稳定性验证记录](validation/2026-07-22-p5-stability.md)。
### 7.10 P5 最终发布验收状态（2026-07-22）

- 发布包新增并强制携带用户指南、隐私说明、已知限制和发布清单；`P5-ACC-001/v2` 按 `internal-runnable` 目标把 clean/reproducible、版本/commit、manifest 文件 hash/size、敏感扫描和必需文件作为硬门禁。
- 最终暂存快照的 dirty 开发候选成功生成 15 个文件，250 组件、406 个跟踪文本文件零敏感命中；新增文档进入 manifest 和 NSIS，安装矩阵仍为 6/6。
- 当前三个 EXE 均为 `NotSigned`，OpenSSH 会话没有 `signtool` 或可用代码签名证书；Defender 被第三方杀毒接管，命令行扫描返回 `0x80004005`；没有 Windows 10 x64 最终包结果。
- 用户于 2026-07-22 明确交付目标仅为可运行、不上架商店；因此签名、双引擎扫描和 Windows 10 独立矩阵在 `internal-runnable` 模式中记录为 warning，不再阻塞完成。若选择 `public-signed`，三项继续失败关闭。
- 公开 GitHub Release 作业继续保留有效 Authenticode/时间戳与 Defender 严格门禁；当前直接/内部可运行交付不依赖该公开发布作业。完整事实见[P5 最终发布验收记录](validation/2026-07-22-p5-final-release-acceptance.md)。

### 7.11 P5 一键安装补强证据（2026-07-23）

- 正式安装包锁定并嵌入 Microsoft 官方 WebView2 Evergreen Bootstrapper；发布构建同时校验精确文件大小、SHA-256 和 Microsoft Authenticode 签名。
- 新系统缺少 WebView2 时，NSIS 在写入应用文件前静默运行官方引导程序并重新检测；安装失败或运行库仍不可用时固定失败关闭，不留下应用文件。
- 隔离安装矩阵扩展为 7/7：全新安装、原位升级、默认保留数据、删除二次确认、确认清理、WebView2 自动安装成功、WebView2 自动安装失败回滚。
- 正式 `0.1.0` 安装包为 94,597,359 字节，SHA-256 `64cde79b412724ed70cebef559b73af99e9354c13cabf935abac581076781263`；Go 全量 test/vet/build 和发布门禁通过。完整事实见[P5 一键安装验证记录](validation/2026-07-23-p5-webview2-one-click-installer.md)。

## 8. 测试夹具

### 8.1 直播接口 fixture

- `doRequest.example.json` 只保留脱敏 fixture 索引；具体最小样本位于 `testdata/stream_resolver`，测试不再携带或解析巨型真实响应。
- 覆盖 FLV、HLS、SDK 嵌套 JSON、H.265、附加流、无流、字段类型变化和 URL 过期。
- 所有 URL 使用 `.invalid` 域名，query 使用占位值。

### 8.2 消息 fixture

- 使用已知 protobuf schema 构造聊天、礼物、点赞、进场、关注、控制、未知和损坏 payload。
- 包含重复 message ID、无 ID、礼物连击、乱序平台时间和接收时间跳变。
- 用户名和内容使用虚构数据，平台 ID 使用随机值。

### 8.3 媒体 fixture

- 5–30 秒彩条/静音/正弦波测试流。
- H.264/AAC、H.265/AAC、无音频、时间戳非零和人为损坏尾部。
- fixture 由脚本本地生成，不提交大型二进制；生成命令和预期 ffprobe 输出版本化。

## 9. 故障注入矩阵

| 故障 | 注入方式 | 预期行为 |
| --- | --- | --- |
| 网络断开 | fixture server 关闭连接 | 状态转 RECONNECTING，记录消息缺口 |
| 流地址过期 | 输入返回 403/EOF | 重新解析并新建分片 |
| FFmpeg 崩溃 | 测试进程指定退出码 | 有界重启、记录 attempt 和缺口 |
| 数据库锁定 | 独占写事务 | spool 继续，超时后降级告警 |
| 磁盘写满 | 限额临时卷/fault FS | 停止录制，监听尽量继续 |
| Cookie 失效 | 接口返回授权/风控错误 | 稳定错误码，不循环高频重试 |
| 程序强制退出 | 终止桌面进程 | Job 清理子进程，下次启动恢复 |
| 分片损坏 | 截断 `.partial` | 保留文件并标记 corrupt |
| 系统时间跳变 | fake clock 前后跳 | 单调偏移不倒退 |
| UI 消费变慢 | 暂停事件消费者 | 后端批次有界，磁盘采集不受影响 |

每个故障测试断言状态转换、用户提示、诊断码、数据缺口和敏感信息脱敏，而不只断言“没有崩溃”。

## 10. 性能与可靠性基线

### 10.1 性能目标

- UI 空闲 CPU 平均低于 2%，内存低于 300 MiB。
- 单房间消息监听 P95 UI 延迟低于 1 秒。
- stream copy 录制时应用自身平均 CPU 低于 10%，FFmpeg 单独统计。
- 事件 SQLite 提交 P95 低于 2 秒；实时回调不执行同步数据库写。
- 历史 100 万事件场次首次打开在 3 秒内展示概要，事件分页在 500 ms 目标内返回。

### 10.2 泄漏检测

- 记录每次房间启停前后的 goroutine、HTTP 连接、文件句柄、计时器和子进程。
- 重复启停 20 次后资源回到容许基线。
- 前端切换实时/历史页面 30 次后事件监听器数量不增长。
- 60 分钟正式稳定性测试内存趋势不得持续线性增长。

## 11. 可观测性与诊断

- Go 日志使用 `slog` JSONL，字段包含时间、级别、组件、room/session/attempt、error_code 和 correlation_id。
- 默认日志等级 info，调试模式自动过期，避免长期输出敏感或海量日志。
- 日志轮转：单文件上限和总保留空间可配置，默认保留 14 天。
- 诊断包包含版本、依赖检查、脱敏配置、最近日志、数据库 schema/完整性摘要、场次 manifest 和系统信息。
- 诊断包排除凭据文件、Cookie、完整流 URL、签名、原始 payload、视频和完整弹幕正文。
- 脱敏后再次扫描 `Cookie`、`msToken`、`a_bogus`、`signature` 和 URL query 形态；命中则拒绝生成并记录错误。

## 12. Windows 安装包

### 12.1 包含内容

- 桌面主程序和 Wails 嵌入前端。
- 经许可审核的 FFmpeg/ffprobe，或在不随附版本中提供明确的路径配置流程。
- 数据库迁移、默认配置和许可证/SBOM。
- 不包含 Go 工具链、Node、pnpm、源码或真实测试数据。

### 12.2 安装行为

- 首版提供 x64 安装包；arm64 只在独立构建和测试矩阵完成后增加。
- 默认按用户安装，避免不必要管理员权限。
- 检查 WebView2；缺失时自动静默安装随包锁定的 Microsoft 官方 Evergreen Bootstrapper 并复检，失败则在写应用文件前中止。完全离线环境需预先安装 WebView2 Evergreen Standalone Runtime。
- 应用数据位于用户数据目录，与程序安装目录分离。
- 卸载默认保留数据库和媒体；用户主动选择删除时二次确认并显示预计大小。

### 12.3 代码签名

- 商店或公开签名发行使用 Windows 代码签名证书；签名发生在可复现构建完成后。
- 签名前后记录 SHA-256；发布清单记录安装包、便携包和 SBOM hash。
- 直接/内部可运行交付允许未签名包，但必须明显标记、从可信渠道取得并核对 manifest SHA-256；签名不是当前项目完成门槛。

## 13. 升级、迁移与回滚

1. 启动升级版本前创建 SQLite 一致性备份和配置快照。
2. 执行可重入迁移；任何失败停止启动并保留原数据库。
3. 不自动降级 schema；回滚程序必须搭配升级前备份。
4. 媒体和 session manifest 保持向后可读；新增字段向前兼容忽略。
5. Wails/前端升级不得更改已有 Wails 事件名称，破坏性变更需新事件版本。

测试矩阵覆盖：前一个正式版本 → 当前版本升级、升级中断、磁盘不足、迁移失败和使用备份回滚。

## 14. CI/CD 流程

```text
提交
  -> 快速门禁
  -> Windows 集成测试
  -> 桌面 E2E
  -> 构建候选产物
  -> SBOM/许可证/敏感扫描
  -> 30 分钟稳定性（发布候选）
  -> 人工验收
  -> 可选签名/病毒扫描
  -> 发布与校验和
```

- PR 构建不访问生产凭据和真实直播间。
- 发布只从受保护 tag 触发，版本与 Git tag 必须一致。
- 构建日志、测试报告和校验和归档；机密通过受控 secret store 注入。
- 发布失败不覆盖上一稳定版下载和更新元数据。

## 15. 发布门禁

当前直接/内部可运行交付必须同时满足：

- Go 原有测试、桌面后端、前端、集成和 E2E 全部通过。
- 60 分钟正式稳定性测试通过；无持续内存、句柄或 goroutine 增长。
- 流解析、FFmpeg 崩溃、断网、数据库忙、磁盘满和强制退出恢复通过。
- 无 P0/P1 缺陷；P2 必须有明确规避方式和发布说明。
- 数据库升级/备份/回滚演练通过。
- 安装、升级、卸载和 WebView2 缺失流程通过 Windows 矩阵。
- SBOM、许可证与敏感信息扫描通过；文件清单和 SHA-256 可复核。
- 商店/公开签名发行若未来启用，再把代码签名、病毒扫描和额外 Windows 兼容矩阵提升为硬门禁。
- 文档、版本信息、校验和、已知限制和隐私说明完整。

## 16. 发布后策略

- 首次启动和升级后记录本地健康状态，不默认上传遥测。
- 崩溃报告由用户主动导出和发送；未来若增加遥测必须单独征得同意。
- 严重采集兼容问题优先通过解析器/配置更新发布补丁，不绕过签名与测试门禁。
- 每个正式版本至少保留一个可回滚安装包和对应数据库兼容说明。
- 发布后复盘错误码分布、恢复成功率、录制缺口和用户诊断反馈，形成下一版本输入。

## 17. OSS 自动更新

### 17.1 实现与验证状态

- 固定杭州私有 Bucket `douyinlive-updates-cn-hangzhou-1e8d9993065b`，仅匿名开放 `channels/*` 与 `releases/*` 的 HTTPS GetObject；启用版本控制、SSE-OSS/AES256、通道非当前版本 90 天清理和未完成分片 7 天清理。
- 发布 RAM 用户只允许指定对象前缀的 GetObject/GetObjectVersion/PutObject，不具备 Bucket 删除、策略、ACL、版本控制或生命周期权限；凭据与 Ed25519 私钥分别以 DPAPI LocalMachine 加密，并用受保护 ACL 限制为当前用户和 SYSTEM。
- 更新协议使用单对象 Ed25519 签名信封，严格覆盖 SemVer、平台、Origin/前缀、大小、SHA-256、发布清单和 `highestSeenVersion`；客户端拒绝重定向、未知/重复字段、尾随 JSON、超限响应、同版/降级和签名回放。
- 更新服务实现 30 秒首次检查、6 小时加抖动、设置关闭时零周期请求、录制忙状态暂停下载、ETag/Range 续传、完整 SHA-256 和无路径 DTO；Wails/React 提供状态、检查、准备、取消、确认安装和全局就绪提示。
- 独立 `douyin-live-updater.exe` 重新验签并检查安装器，等待父进程退出，执行同卷程序目录备份、NSIS 安装、注册表/目标 EXE/健康 nonce 校验；失败恢复程序目录，schema 变化时调用精确 SQLite 备份回滚，数据库回滚失败则保留证据且拒绝启动旧版。
- 全量 Go test/vet/build、前端 10 文件 37 项测试/typecheck/build、production Wails、0.2.0 脏树候选发布门禁和 NSIS 隔离安装矩阵 7/7 通过。完整事实见[P6 OSS 自动更新实现记录](validation/2026-07-23-p6-oss-auto-update.md)。

### 17.2 发布顺序

1. 只在干净工作树、精确 `vX.Y.Z` tag 和完整发布门禁通过后生成安装器、更新助手、发布清单与签名信封。
2. 发布工具从 Windows DPAPI 文件读取 OSS 发布凭据与 Ed25519 私钥，先上传 `releases/vX.Y.Z/*`，匿名 HTTPS 回读并复核大小、SHA-256 和签名。
3. 所有版本化对象验证完成后，最后单次覆盖 `channels/canary.json` 或 `channels/stable.json`；通道对象使用 `no-cache`，版本对象使用一年 immutable 缓存。
4. 首次人工安装 0.2.0 引导版；0.2.1 先进入 canary，完成真实升级、重启、失败回滚与 24 小时稳定观察后，把完全相同的已验证信封提升到 stable。
5. 通道只向更高版本前进；已升级客户端不降级，问题版本以更高 SemVer 修复。签名密钥轮换先发布同时信任新旧公钥的客户端，再切换签名方。

### 17.3 发布门禁与 OSS 限制

- 自动更新发布必须通过 Ed25519 信封/对象 hash、固定 Origin/前缀、OSS 匿名权限负例和敏感扫描；AccessKey、RAM 凭据与私钥不得进入仓库、产物或日志。
- stable 提升前必须有同一 0.2.1 canary 信封完成 0.2.0→0.2.1 实机升级、活动录制拒绝、断网续传、坏签名/坏包拒绝、失败恢复与 24 小时观察证据。
- OSS 开启版本控制后，服务端会忽略 `x-oss-forbid-overwrite`。版本化发布物采用“发布工具先 HeadObject 拒绝已存在 key + 单发布身份 + 版本历史可恢复”的受控发布门；它能防止正常流程覆盖，但不是 OSS 侧不可绕过的 WORM。
- 若未来要求存储层强制不可变，应把版本产物与可覆盖通道指针拆到不同 Bucket，并只对发布 Bucket 启用保留策略。
