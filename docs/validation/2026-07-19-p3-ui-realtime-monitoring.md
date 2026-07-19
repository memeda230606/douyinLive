# P3-UI-001 实时监控界面验收

- 日期：2026-07-19
- 权威工作副本：`GJS-20250801EFK:D:\douyinLive`
- 代码基线：`7700e9f1a522a404fe25aa652b1b88c187dd53d4` 之后的 P3-UI 工作树
- 状态：`DONE`
- 结论：实时事件、录制进度、有序房间状态、前端界面、冷启动与关闭生命周期均通过自动化和真实 Windows GUI 验收，独立终审 P0/P1/P2=0；P3-ACC 在线连续录制与真实故障链路未在本任务中冒充完成。

## 1. 验收范围

本任务关闭以下 P3-UI 范围：

- SQLite 成功提交后的隐私安全 `live:event` 批量发布。
- 当前录制场次的低频、单调、可 fencing `recording:progress`。
- 全局单调 `room:status.revision` 以及重连、恢复和错误告警单一有序来源。
- 前端严格运行时 schema、单例 Wails 事件桥、有界实时 store 和可访问实时监控页。
- Wails 冷启动 Bootstrap 屏障、关闭时序、验收标签隔离和真实 WebView2 像素证据。

本任务不包含 P3-ACC 的在线 10 分钟、真实断网、真实 FFmpeg 崩溃、恢复后新分片/缺口和真实下播收尾。

## 2. 完成实现

### 2.1 三个实时事件契约

| 事件 | 严格载荷 | 有序与隐私边界 |
| --- | --- | --- |
| `room:status` | `RoomRuntimeStatus`，包含 `revision`、状态、场次/操作关联、`recordingStatus`、`retryAt`、稳定 `errorCode` 和消息 | 全局 revision 严格单调；前端拒绝较旧状态。录制中断、重试、恢复与失败告警均由该单一状态源派生，不另发 `recording:error` |
| `live:event` | `LiveEventBatchDTO`，包含 `sessionId/emittedAt` 和最多 100 条白名单 source DTO | 只在 SQLite 真实提交后发布；平台/用户原始标识、method、dedupe/raw 引用、normalized JSON、protobuf、路径、Cookie、签名和流 URL 均不进入载荷 |
| `recording:progress` | `RecordingProgressDTO`，包含 `roomId/sessionId/operationId`、状态、时长、字节、分片、帧率、速度、重启数与更新时间 | 仅 `recording/reconnecting`；每个当前场次最多 1 Hz，整数饱和在 JavaScript 安全上限，旧场次/操作/attempt/generation 被拒绝 |

`live:event` 后端按每场次最多 100 条或 100 ms 组批，输入通道最多 2,000 条、发布队列最多 20 批。短窗、SQLite 和同批去重命中的 source 不发布第二次；慢或阻塞的 Wails 回调不会反压耐久写入，关闭后排队批次不能穿透 shutdown fence。

录制进度跨 FFmpeg attempt 单调累积时长、字节、分片和重启数；Finalize 在取得停止/排空所有权前先取消 watcher。停止或排空超时、旧 recorder 回调和不匹配 operation 均不能向当前 UI 继续发布。

### 2.2 前端实时页

- 应用只注册一个全局 Wails 事件桥，卸载时完整注销；三个事件全部先通过 `zod` strict schema，再进入 Zustand。
- 实时事件按 ID 去重，内存保留最近 2,000 条；进度保留最多 16 个场次，告警最多 100 条。淘汰仅影响 UI 副本，不改变 SQLite、checkpoint 或恢复事实。
- 房间状态按 `revision` 取最新，事件按 `sessionId` 隔离，进度必须精确匹配 `roomId/sessionId/operationId`。
- 房间卡片提供“查看实时”入口；实时页支持切换房间、聊天/礼物/点赞/进房/关注/系统六类筛选、虚拟化时间线、录制指标、重试倒计时和中断/恢复告警。
- 未知事件归入系统筛选；昵称、弹幕和错误消息均作为文本渲染。布局在宽屏双栏与窄屏顺序排列之间响应，状态同时使用图标、颜色和文字，关键控件具备焦点与 ARIA 标注。

### 2.3 Wails 启动与关闭

- 前端可能在 `OnStartup` 开始后、基础设施初始化完成前调用 `GetBootstrap`。桌面门面使用一次性 startup-ready 屏障，使已开始启动的 Bootstrap 查询等待初始化成功或失败结论，避免无限 `staleTime` 永久缓存“数据不可用/0 房间”。
- `main` 在进入 `wails.Run` 前建立 happens-before；启动与关闭共享门禁，关闭先取消初始化并等待应用达到稳定 `STOPPED` 后释放屏障，迟到或重复 startup 不能重新开放事件。
- 验收没有调用 `WindowReloadApp` 或刷新前端；结果 JSON 与截图来自同一次真实冷启动。
- `p3uiacceptance` 标签才暴露隔离 fixture 与结果绑定；普通生产构建不包含这些入口。结果文件使用固定 schema、字段白名单、11 项精确检查和一次写入约束。

## 3. 自动化门禁

| 门禁 | 结果 |
| --- | --- |
| `where go` | `C:\Program Files\Go\bin\go.exe` 可用 |
| `go test ./... -count=1`、`go vet ./...`、`go build ./...` | 全部通过 |
| P2 与 P3 UI 标签的 desktop test/vet | 通过；验收 hook 的生产隔离经 `go list` 验证 |
| eventstore LiveEvent/Publisher 重点回归各 `-count=20` | 通过；提交后发布、重复 parity、有界批次、阻塞 callback 与关闭 fence 均覆盖 |
| capture Progress 重点回归 `-count=20` | 通过；1 Hz、跨 attempt 单调、JS 安全整数、Finalize/Stop/Flush 超时和 fencing 均覆盖 |
| room revision 重点回归 `-count=100` | 通过；查询/启动并发仍保持全局严格单调 |
| desktop Bootstrap 生命周期回归 `-count=100` | 通过；启动、取消、关闭、重复与迟到 startup 均覆盖 |
| Node/pnpm | Node 24.18.0、pnpm 9.12.3 |
| `pnpm test` | 6 个测试文件、20 项测试通过 |
| `pnpm typecheck`、`pnpm build` | TypeScript 与 Vite 生产构建通过 |
| `node --check cmd/desktop/p3_ui_acceptance.js` | 通过 |
| Wails v2.13.0 标签验收构建与 `windows/amd64` 生产构建 | 通过 |
| `git diff --check` | 通过；生成绑定只保留经审核的语义变化 |
| 独立终审 | P0=0、P1=0、P2=0 |

## 4. 真实 Windows GUI 证据

验收在隔离数据根、真实 Wails/WebView2 窗口中执行，不连接直播平台、不使用刷新，并通过真实运行时事件依次注入初始录制、重连和恢复状态。

严格结果 `P3-UI-ACC-001/v1` 为成功，11/11 检查全部为真：`roomVisible`、`statusFence`、`timelineCapacity`、`sessionFence`、`eventFilter`、`progressMetrics`、`operationFence`、`retryCountdown`、`gapAlerts`、`privacySafe`、`layoutUsable`。最终可见事件 2,000 条、礼物筛选 333 条、告警 2 条，状态为“录制中”。

| 证据 | 结果 |
| --- | --- |
| 严格结果 JSON SHA-256 | `fa98aae2646d70cff878af9582e8699b7b8b8d4d5969f617651564ff8cdc4d55` |
| 精确 HWND `PrintWindow(PW_RENDERFULLCONTENT)` 截图 | 1044×788，50,575 字节，SHA-256 `a7d14795901fbdec920e8034582c0f9fd2b1afdd7a8fbeec2ddba9fc829c658d` |
| 像素复核 | 显示“P3 实时验收房间”“录制中”、2,000 条、六类筛选、录制指标和最新事件；与同一运行的结果 JSON 一致 |
| `WM_CLOSE` | 54 ms 内自然退出 |
| 清理 | 验收进程 0、计划任务 0、隔离数据根 0 残留 |
| 生产 EXE | `cmd/desktop/build/bin/douyin-live-desktop.exe`，48,097,792 字节 |
| 生产 EXE SHA-256 | `4b0711dbef5778f19ea55975ce99323c4f03830caf19785545d2baaf88fdc4d9` |

远程桌面断开时，WebView2 DOM 可完成更新而桌面 framebuffer 仍停留旧帧；本次先核对严格结果，再按精确 PID 枚举 HWND 并使用 `PrintWindow(PW_RENDERFULLCONTENT)` 取证，避免把 `CopyFromScreen` 的陈旧像素误判为应用回退。

## 5. 隐私与安全结论

- UI 事件 DTO、错误、状态、日志和验收结果不含 Cookie、签名、完整流 URL、绝对媒体路径、原始平台用户标识、用户哈希或 protobuf/raw 载荷。
- 实时内容只使用白名单字段并以纯文本渲染；测试注入的 HTML 样式恶意文本不会创建 DOM 元素。
- 验收 fixture 使用固定虚构 Live ID、UUIDv7 场次/操作和隔离数据根，不连接或写入真实直播间。
- 验收入口通过 build tag 与生产二进制隔离；关闭后事件发射 fence 生效，未留下进程、任务或测试数据。

## 6. 已知限制与 P3-ACC 边界

- 当前 Windows OpenSSH Go 环境为 `CGO_ENABLED=0`，且 `where gcc` 找不到编译器；`go test -race` 会在启动阶段报 `-race requires cgo`。这是竞态检测工具链不可用，不是源码测试失败；本任务未安装 GCC、未改变 CGO。
- P3-UI 验收使用真实 Windows 桌面/WebView2 与真实 Wails 事件通道，但使用本地确定性 fixture，因此不证明线上平台连接、连续录制、真实断网、真实 FFmpeg 崩溃、恢复后媒体分片/缺口或真实下播收尾。
- 当前任务 P3-ACC-001 仍需使用获授权的测试直播间完成上述在线验收；若该房间离线，可按主计划改用无需授权的公开在线直播间。在线结果未产生前不得把 PHASE-3 标记为完成。

## 7. 结论

P3-UI-001 的 2 个任务点完成，PHASE-3 进度由 26/30（87%）更新为 28/30（93%），总体进度由 66% 更新为 68%。当前任务转为 P3-ACC-001；P3-UI 的实时界面里程碑已完成，但整个桌面 UI 计划仍需历史、分析、导出及发布阶段门禁，不能整体标记完成。
