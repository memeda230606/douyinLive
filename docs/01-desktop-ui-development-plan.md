# 桌面 UI 与交互开发计划

> 上级计划：[总开发计划](00-master-development-plan.md)
> 相关计划：[采集与录制](02-capture-and-recording-development-plan.md) · [数据与分析](03-data-and-analysis-development-plan.md) · [工程与发布](04-engineering-testing-and-release-plan.md)
> 实施状态（2026-07-21）：P4-ANA-001 已完成严格版本化分析报告、十秒趋势、质量提示、候选证据与回放跳转；项目总进度 82%，下一任务为 P4-ASR-001 未配置降级。
> 最近验收：[P3-UI 实时监控界面](validation/2026-07-19-p3-ui-realtime-monitoring.md)

## 1. 目标与边界

本计划定义 Windows 桌面端的工程结构、Go 与前端边界、页面信息架构、设计系统、状态呈现和 UI 验收方式。桌面端负责“配置、观察、控制、查询和解释”，不在浏览器线程内执行直播连接、数据库写入、FFmpeg 管理或分析任务。

首版必须让非技术用户在不看命令行的情况下完成添加房间、等待开播、确认录制、查看实时事件、回放历史场次、阅读基础报告和导出诊断包。

## 2. 技术栈与目录

### 2.1 固定技术栈

- 桌面框架：Wails v2，初始锁定 `v2.13.0`。
- 后端：现有 Go module；桌面应用与 `douyinLive` 库同进程。
- 前端：React、TypeScript、Vite。
- 组件与样式：Tailwind CSS、shadcn/ui、Lucide Icons。
- 图表：Apache ECharts，动态加载折线、柱状、散点和标记区域。
- 服务端状态：TanStack Query；本地 UI 状态：Zustand。
- 表单：React Hook Form + schema 校验器；Go 后端再次执行同等约束。
- 测试：Go `testing`、Vitest、Testing Library、Playwright/Wails 桌面冒烟。

### 2.2 目标目录

```text
cmd/
├── main/                     # 现有 WebSocket 服务，保持兼容
└── desktop/                  # Wails 桌面入口
    └── main.go
internal/
├── app/                      # 应用装配与关闭协调
├── room/                     # RoomService
├── capture/                  # CaptureService
├── eventstore/               # EventService
├── playback/                 # PlaybackService
├── analysis/                 # AnalysisService
└── settings/                 # SettingsService
frontend/
├── src/app/                  # 路由、Provider、全局错误边界
├── src/components/           # 通用组件
├── src/features/             # 按业务域组织页面与 hooks
├── src/generated/            # Wails 自动生成绑定，禁止手改
├── src/lib/                  # 格式化、事件适配、类型守卫
├── src/styles/               # tokens 与全局样式
└── tests/                    # 前端测试与 fixture
```

`cmd/desktop` 仅装配依赖和 Wails options。业务逻辑必须位于 `internal/`；Wails 绑定对象只做参数校验、调用服务和错误转换。

## 3. Go 应用服务接口

Wails 仅绑定以下门面。方法返回可序列化 DTO，不返回通道、数据库实体、protobuf、`error` 内部堆栈或包含凭据的对象。

### 3.1 `RoomService`

```go
ListRooms(ctx context.Context) ([]RoomConfig, error)
CreateRoom(ctx context.Context, input CreateRoomInput) (RoomConfig, error)
UpdateRoom(ctx context.Context, id string, input UpdateRoomInput) (RoomConfig, error)
DeleteRoom(ctx context.Context, id string, deleteData bool) error
StartMonitoring(ctx context.Context, id string) error
StopMonitoring(ctx context.Context, id string) error
GetRoomStatus(ctx context.Context, id string) (RoomRuntimeStatus, error)
```

约束：直播间标识去除 URL 包装和空白后必须非空；重复 Live ID 返回 `ROOM_ALREADY_EXISTS`；删除正在录制的房间必须先停止并由 UI 二次确认。

### 3.2 `CaptureService`

```go
GetRecordingStatus(ctx context.Context, roomID string) (RecordingStatus, error)
StartRecording(ctx context.Context, roomID string) (LiveSession, error)
StopRecording(ctx context.Context, roomID string) error
ListStreamVariants(ctx context.Context, roomID string) ([]StreamVariant, error)
```

自动录制启用时，`StartRecording` 通常由监督器调用；UI 的手动调用只允许在已开播且未录制状态。`StreamVariant` 不包含完整 URL。

### 3.3 查询与分析服务

```go
ListSessions(ctx context.Context, query SessionQuery) (Page[LiveSession], error)
GetSession(ctx context.Context, id string) (LiveSessionDetail, error)
ListEvents(ctx context.Context, query EventQuery) (Page[LiveEvent], error)
GetPlaybackManifest(ctx context.Context, sessionID string) (PlaybackManifest, error)
StartAnalysis(ctx context.Context, sessionID string, input AnalysisInput) (AnalysisJob, error)
GetAnalysisReport(ctx context.Context, sessionID string) (AnalysisReport, error)
CancelAnalysis(ctx context.Context, jobID string) error
ExportSession(ctx context.Context, input ExportInput) (ExportResult, error)
```

分页使用游标而不是页码；默认 100 条，最大 500 条。历史事件查询必须指定 `session_id`，可选时间范围和消息类型。

### 3.4 设置与诊断

```go
GetSettings(ctx context.Context) (AppSettings, error)
UpdateSettings(ctx context.Context, input UpdateSettingsInput) (AppSettings, error)
ValidateStoragePath(ctx context.Context, path string) (StoragePathStatus, error)
CheckDependencies(ctx context.Context) (DependencyStatus, error)
ListDiagnostics(ctx context.Context, query DiagnosticQuery) (Page[DiagnosticEvent], error)
CreateDiagnosticBundle(ctx context.Context, input DiagnosticBundleInput) (ExportResult, error)
```

Cookie 更新使用单独输入字段，读取设置时只返回 `configured: true/false` 和末次更新时间，不回传明文。

## 4. 错误契约

绑定层把所有业务错误转换为：

```ts
type AppError = {
  code: string;
  message: string;
  suggestion?: string;
  retryable: boolean;
  correlationId: string;
  fieldErrors?: Record<string, string>;
};
```

前端只根据 `code` 决定交互，不解析中文文本。首版稳定错误码包括：

- `ROOM_NOT_FOUND`、`ROOM_ALREADY_EXISTS`、`ROOM_OFFLINE`、`ROOM_CHECK_FAILED`。
- `COOKIE_INVALID`、`STREAM_NOT_FOUND`、`STREAM_EXPIRED`。
- `RECORDING_ALREADY_RUNNING`、`RECORDING_NOT_RUNNING`、`FFMPEG_NOT_FOUND`、`FFMPEG_FAILED`。
- `STORAGE_NOT_WRITABLE`、`DISK_SPACE_LOW`、`DATABASE_BUSY`、`DATA_CORRUPTED`。
- `ANALYSIS_NOT_READY`、`ASR_NOT_CONFIGURED`、`OPERATION_CANCELLED`。

无法识别的内部错误统一映射为 `INTERNAL_ERROR` 并附 `correlationId`，详细堆栈只写脱敏日志。

## 5. Wails 事件与前端消费

### 5.1 事件接入

应用启动时只注册一次全局事件桥；页面通过 store 订阅，不直接重复调用 Wails `EventsOn`。卸载应用时注销全部监听。P3-UI 已落地并验证的实时契约只有以下 3 个，所有载荷先经过严格运行时 schema 校验：

| 事件 | 载荷与前端行为 |
| --- | --- |
| `room:status` | 严格 `RoomRuntimeStatus`：以全局单调 `revision` 排序，以 `roomId/sessionId/operationId` 关联；`recordingStatus/retryAt/errorCode/message` 驱动状态、重试倒计时以及录制中断/恢复告警 |
| `live:event` | 严格 `LiveEventBatchDTO`：按 `sessionId` 合并最多 100 条白名单 source 事件，按 `event.id` 去重并追加实时缓冲 |
| `recording:progress` | 严格 `RecordingProgressDTO`：只接受 `recording/reconnecting`，必须与当前 `roomId/sessionId/operationId` 精确匹配，更新时长、字节、分片、帧率、速度和重启数 |

录制错误、重连与恢复不再另发 `recording:error`；它们统一由有序 `room:status` 的稳定 `errorCode` 和 `revision` 派生房间级告警，避免两个事件源产生重复或乱序提示。`analysis:progress`、`analysis:completed` 与 `system:alert` 仍属于后续阶段的计划契约，不计入 P3-UI 已完成范围。

### 5.2 背压与丢帧策略

- `live:event` 后端以最多 100 条或 100 ms 为一批发送。
- 前端按 `event.id` 去重；内存仅保留最近 2,000 条，超出后丢弃最旧 UI 副本，不影响磁盘数据。
- 后端 UI 投递与 SQLite 耐久提交隔离：只有真实提交的 source 事件可发布，慢或阻塞的 Wails 回调不能反压持久化链路；关闭后禁止旧回调继续消费排队批次。
- `recording:progress` 每个当前场次最多 1 Hz；跨 attempt 的时长、字节、分片和重启数保持单调，JavaScript 整数边界限制在 `2^53-1`。
- 前端以 `revision` 拒绝过期房间状态，以 `sessionId` 拒绝越界场次事件，以 `roomId/sessionId/operationId` 精确拒绝过期录制进度；状态告警最多保留 100 条，录制进度最多保留 16 个场次。
- 页面不可见时停止动画和高频图表重绘，但继续接收最后状态。
- 互动折线每秒最多重算一次；大于 30 分钟的历史图表使用已聚合指标桶。

## 6. 信息架构与导航

### 6.1 应用框架

- 左侧主导航：总览、直播间、历史场次、分析、诊断、设置。
- 顶部状态区：全局监听数、录制数、磁盘余量、后台任务和通知入口。
- 主内容区最小宽度 960 px；窗口小于 1100 px 时导航折叠为图标栏。
- 默认窗口 1440×900，最小窗口 1024×700；记忆上次正常尺寸和位置，越界时回到主屏幕。

### 6.2 总览仪表盘

展示：

- 正在直播、等待开播、异常和停止监控的房间数量。
- 当前录制卡片：主播、标题、时长、文件大小、磁盘余量、重试次数。
- 最近 60 分钟互动趋势和当前弹幕速率。
- 最近场次及其报告状态。
- 需要处理的告警和依赖异常。

空状态提供“添加第一个直播间”；无 FFmpeg 时不阻止消息监听，但录制卡片显示明确修复入口。

### 6.3 直播间管理

- 列表支持搜索、状态筛选和按最近活动排序。
- 新增/编辑表单包含 Live ID、别名、自动监听、自动录制、质量、分片时长、Cookie 配置状态。
- Cookie 输入为密码框，不支持复制回读；保存成功后清空本地表单值。
- 删除操作先说明“仅删配置”与“同时删历史数据”的差异；后者必须输入房间别名确认。

### 6.4 实时监听页

- 页头：房间元数据、直播状态、连接状态、录制状态和主操作。
- 左侧：实时弹幕时间线，可筛选聊天、礼物、点赞、进场、关注和系统事件。
- 右侧：当前人数/互动摘要、分钟趋势、录制与磁盘、连接诊断。
- 事件行显示相对时间、用户脱敏显示名、类型和内容；不在默认视图显示原始 payload。
- “断线中”保留已收到事件并显示重试倒计时；“下播收尾中”禁止立即启动新场次。

P3-UI 已实现从直播间卡片“查看实时”进入的独立实时视图：可切换直播间，提供聊天、礼物、点赞、进房、关注、系统六类筛选；虚拟化时间线保留最近 2,000 条，右侧展示录制状态、时长、写入量、分片、速度、帧率、重启次数、重试倒计时和中断/恢复告警。未知事件归入系统筛选，所有昵称和内容均作为纯文本渲染；宽屏双栏在窄窗口下顺序折叠，键盘焦点、ARIA 标签和非颜色状态表达已覆盖组件测试与真实 GUI 验收。

### 6.5 历史场次与回放

- 场次列表按开始时间倒序，支持房间、日期、完整性和报告状态筛选。
- 详情包含概览、回放、互动、转写、分析、文件和诊断标签页。
- 播放器基于 `PlaybackManifest` 选择已封装 MP4；多分片以播放列表衔接。
- 拖动播放进度后按时间窗口查询事件，弹幕列表与图表同步定位。
- 媒体缺口以时间轴红色区间显示，不自动隐藏。

P4-PLY 已实现状态筛选和 keyset 分页场次列表、场次详情、事件/缺口/媒体分页装载、可点击及键盘定位的统一时间轴、当前位置前后 5 秒同步互动，以及只使用已审计 H.264 MP4 artifact 的播放器。播放器 URL 只含 opaque artifact ID，路径、root 和摘要不进入前端合同；不可直放与缺口位置保持明确占位和原因码。

### 6.6 分析报告

- 顶部展示总时长、弹幕、独立互动、点赞、关注、礼物和完整性分数。
- 趋势图叠加主播话术区间、互动峰值和媒体缺口。
- 高频问题、话题、沉默区间和高光候选按时间可跳转回放。
- 未配置 ASR 时隐藏依赖转写的模块，并解释如何配置；基础互动指标继续显示。
- 每个自动结论显示数据依据、时间窗口和算法版本，不输出无法追溯的建议。

P4-ANA 已实现终态场次选择、无报告生成、摘要卡、最多 240 点的十秒趋势、缺口/价值不可用等质量提示，以及高光、峰值和低谷候选列表。候选显示时间范围、得分、完整度和前三项贡献指标，并通过 AppShell 跳转到历史回放的精确场次偏移；底部展示输入指纹派生的分析版本、算法版本与完成时间。所有 Wails 载荷先经 strict Zod v1 白名单，私有字段、未知 warning 和计数不一致均 fail closed。

### 6.7 设置、日志与诊断

- 设置分为常规、存储、录制、分析、隐私和高级。
- 存储设置实时显示可写性、剩余空间和预估可录时长。
- 诊断页支持组件、级别、房间、场次和时间筛选。
- 诊断包导出前展示将包含和排除的内容，默认排除 Cookie、流 URL、签名和用户原始标识。

## 7. 视觉设计系统

### 7.1 原则

- 视觉优先级：录制安全与错误 > 当前直播状态 > 实时指标 > 装饰。
- 不仅依赖颜色表达状态，所有状态同时使用图标和文字。
- 信息密集但避免大面积渐变和无意义动画。
- 中文文本优先使用系统字体栈：`Segoe UI`, `Microsoft YaHei UI`, sans-serif。

### 7.2 Tokens

- 8 px 网格；常用间距 4/8/12/16/24/32。
- 圆角：控件 6 px、卡片 10 px、对话框 12 px。
- 正文 14 px，辅助 12 px，页面标题 24 px，数据重点 28–32 px。
- 状态语义：在线绿、等待蓝灰、收尾紫、警告琥珀、错误红；深浅主题均满足 WCAG AA 对比度。
- 动画时长 120–220 ms；尊重系统“减少动态效果”。

### 7.3 通用组件

- `RoomStatusBadge`、`RecordingIndicator`、`DiskSpaceMeter`。
- `EventTimeline`、`MetricCard`、`MetricTrendChart`。
- `SessionIntegrityBadge`、`GapTimeline`、`PlaybackEventPanel`。
- `AppErrorPanel`、`EmptyState`、`ConfirmDangerDialog`。
- `VirtualizedTable`、`FilterBar`、`TimeRangePicker`。

组件不得直接调用 Wails API；业务 feature 通过 hooks 提供数据和动作。

## 8. 状态与动作矩阵

| 房间状态 | 主要文案 | 允许动作 | 禁止动作 |
| --- | --- | --- | --- |
| `STOPPED` | 已停止监控 | 开始监控、编辑、删除 | 手动录制 |
| `WAITING` | 等待开播 | 停止监控、编辑非连接配置 | 手动录制 |
| `STARTING` | 正在建立连接 | 取消/停止 | 重复启动、删除 |
| `LIVE` | 直播中 | 启停录制、查看实时 | 修改 Live ID、删除 |
| `RECORDING` | 正在录制 | 停止录制、查看文件 | 重复录制、修改目录 |
| `RECONNECTING` | 连接中断，正在重试 | 停止监控 | 重复启动 |
| `FINALIZING` | 正在收尾 | 查看进度 | 新录制、删除场次 |
| `ERROR` | 需要处理 | 重试、查看诊断、停止 | 依错误类型限制 |

关闭主窗口时：若没有后台任务则正常退出；存在监听或录制时默认最小化到托盘。用户选择“退出程序”后显示活跃任务，确认后进入最多 10 秒的优雅关闭。

## 9. 前端数据边界

- 列表和详情均通过 query key 缓存；事件到达后只使相关查询失效，不复制数据库全量数据。
- 日期、大小、礼物价值和数量格式化集中在 `src/lib/format`。
- 所有 Wails 返回值先通过运行时 schema 验证；不合法载荷记录 `UI_CONTRACT_INVALID`。
- URL、HTML、昵称和弹幕内容按文本渲染，禁止未经处理的 `dangerouslySetInnerHTML`。
- 本地文件打开必须通过 Go 服务校验路径位于数据根目录或用户明确选择的导出目录。

## 10. 实施顺序

1. 建立 Wails 桌面入口、前端脚手架、主题和错误边界。
2. 生成 Go/TypeScript 绑定，完成设置与依赖检查页。
3. 实现房间 CRUD、状态事件、总览和房间页。
4. 实现实时事件缓冲、筛选、趋势和录制状态。
5. 实现场次列表、回放清单和同步时间线。
6. 实现分析报告、导出和诊断包。
7. 完成键盘导航、深浅主题、视觉回归和窗口生命周期测试。

## 11. 测试计划

### 11.1 单元与组件测试

- 每个 AppError code 的文案、动作和重试行为。
- 房间状态到按钮可用性的矩阵测试。
- 事件批次合并、去重、2,000 条淘汰和筛选。
- 时间、时区、文件大小、速率和空值格式化。
- 表单校验、Cookie 不回显和危险操作确认。

### 11.2 集成与 E2E

- 首次启动 → 选择目录 → 添加房间 → 等待开播。
- 模拟开播 → 实时事件 → 开始录制 → 下播收尾 → 历史场次。
- FFmpeg 缺失、磁盘不足、Cookie 失效、数据库忙和网络断开。
- 多显示器窗口恢复、最小尺寸、托盘退出和重启恢复。
- 深色/浅色、100%/150%/200% DPI 和 1024×700 最小窗口。

### 11.3 视觉验收

- 核心页面在固定 fixture 下截图比较，允许抗锯齿阈值但不允许布局漂移。
- 所有交互控件可用键盘到达，焦点可见。
- 状态不只依赖颜色，正文和控件对比度达到 WCAG AA。
- 实时页面持续 10 分钟无明显滚动卡顿或图表内存增长。

### 11.4 P3-UI 阶段验收（2026-07-19）

- Go 全量 test/vet/build、P2/P3 标签门禁和重点并发时序回归通过；前端 typecheck、6 个文件 20 项 Vitest 与生产构建通过。
- `p3uiacceptance` 只在测试标签暴露夹具；同一次无刷新冷启动通过严格结果 11/11：房间可见、状态/场次/操作 fencing、2,000 容量、筛选、进度、倒计时、缺口告警、隐私和布局全部为真。
- 严格 JSON SHA-256 为 `fa98aae2646d70cff878af9582e8699b7b8b8d4d5969f617651564ff8cdc4d55`；精确 HWND `PrintWindow(PW_RENDERFULLCONTENT)` 截图为 1044×788、50,575 字节，SHA-256 为 `a7d14795901fbdec920e8034582c0f9fd2b1afdd7a8fbeec2ddba9fc829c658d`，与同一冷启动结果一致。
- `WM_CLOSE` 54 ms 内自然退出，验收进程、计划任务和隔离数据根均为 0 残留；生产 EXE 为 48,097,792 字节，SHA-256 为 `4b0711dbef5778f19ea55975ce99323c4f03830caf19785545d2baaf88fdc4d9`。
- 独立终审 P0/P1/P2=0。当前 OpenSSH 环境 `CGO_ENABLED=0` 且无 GCC，故 `go test -race` 未运行；P3-ACC 的实际关闭证据见 11.5。

### 11.5 P3-ACC 阶段关闭（2026-07-21）

- 正式在线运行证明不少于 10 分钟的稳定资源窗口、FFmpeg 崩溃恢复、隔离 relay 网络故障恢复、新 attempt 与 gap；人工停止后的 UI finalizing 由用户观察，记为 `USER_OBSERVED`。
- 用户明确豁免继续等待自然下播和最终机器视觉 ACK；两项严格记为 `USER_WAIVED/NOT_RUN`，不把人工观察冒充为 `PrintWindow` 或 controller 成功。
- 外层控制器的严格终态、视觉 ACK、自然退出、SQLite 解锁和零残留合同没有被放宽；本次不声明 `P3-ACC-CONTROLLER/v1` 报告 `PASS` 或 `passed=true`。
- 当前关闭门禁包含前端 6 文件 20 项测试、typecheck/build、五组 Go test/vet/build、PowerShell 14/14 与 2/2、真实 Scheduled Task/MIC 正反门禁、两项 `cmd/main` loopback Ping/Pong 和四产物构建。
- 正式运行数据根作为诊断证据保留；未获得约 19 GB 不可恢复删除的明确授权，因此未删除，也不把它写成控制器零数据残留。
- 完整事实与产物哈希见[关闭记录](validation/2026-07-21-p3-acceptance-closeout.md)。

## 12. UI 完成标准

P3-UI-001 实时监控里程碑已满足本节中与实时事件、录制状态、内存上限、隐私、可访问性和真实 GUI 有关的标准；以下清单仍是整个桌面 UI 计划（历史、分析、导出与发布级 E2E）的最终完成标准，不能因 P3-UI 完成而整体标记为 `DONE`。

- 用户无需命令行即可完成首版核心旅程。
- 所有后台状态有明确、稳定且可恢复的 UI 表达。
- 前端没有完整流 URL、Cookie、签名或原始凭据。
- 历史长列表使用虚拟化/分页，实时事件有明确内存上限。
- 原有 `cmd/main` 行为未被桌面入口修改。
- 组件、绑定契约、E2E 与视觉验收全部通过。
