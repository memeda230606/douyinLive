# P3-RCV-001 异常重试、缺口审计与启动恢复验收

- 日期：2026-07-19
- 权威工作副本：`GJS-20250801EFK:D:\douyinLive`
- 代码基线：`5a6602856f3b64a6fb02bf915d5f70e03ec52cce` 之后的 P3-RCV 工作树
- 状态：`DONE`
- 结论：恢复功能、自动化故障注入、Windows 跨进程对象验收、全量构建与隐私边界通过，独立终审 P0/P1/P2=0；在线 10 分钟和真实直播故障留给 P3-ACC，不在本记录中冒充完成。

## 1. 验收范围

本任务关闭以下 P3-RCV 范围：

- 运行期 FFmpeg 异常后的有界重试、代际隔离和 `recording_restart` 缺口。
- 应用重启时旧活动场次的进程、媒体、事件与数据库终态恢复。
- SQLite Schema v5 恢复索引、严格 keyset 分页和原子 `RecoverAndClose`。
- 同一数据根的全局应用实例租约，以及 Recorder Global Job Object 的重开和进程树处置。
- 启动 fail-closed、稳定错误分类、恢复报告和关联 ID/路径/URL 隐私边界。

本任务不包含 P3-UI 的实时界面，也不包含 P3-ACC 的在线 10 分钟、真实断网、真实 FFmpeg 崩溃和真实下播验收。

## 2. 完成实现

### 2.1 Schema v5 与扫描边界

- v4→v5 迁移新增活动场次 `(id, created_at)` 与 open/finalizing 媒体 `(state, session_id)` 两个部分索引；升级前备份，迁移失败整笔回滚。
- 每次启动只取一次 UTC `scan_cutoff`，最多 128 条一页，按 UUIDv7 ID 严格递增。页内 ID、全局顺序、页长和精确 `NextID` 在任何进程、文件或 SQLite 恢复副作用前统一校验。
- 终态事务以旧 session/recording 状态、旧 operation ID 和扫描截止为前置条件，幂等关闭旧 open gap、插入恢复 gap，并把旧活动场次推进 `interrupted`。

### 2.2 Windows 所有权与进程恢复

- 应用在布局完成后、日志/SQLite/凭据初始化前取得 `Global\DouyinLive.Infrastructure.<data-root-sha256>` 事件租约；同根第二实例拒绝启动，holder 正常退出或崩溃后内核释放句柄。
- Recorder attempt 使用 `Global\DouyinLive.Recorder.v1.<root-hash>.<uuidv7>` Job 名。名称不含绝对路径、房间或流信息；恢复只从耐久 attempt journal 重建。
- 新进程仍按 `CREATE_SUSPENDED → AssignProcessToJobObject → NtResumeProcess` fail closed；启动恢复可跨独立子进程重开 Global Job、拒绝重复 admission，并终止已证明属于旧 attempt 的 Job 树。
- Job 不存在按“无遗留进程”安全收敛；打开/查询/终止/关闭失败、歧义 native LastError、畸形 nil-error 结果或非 Windows 无可用检查能力时停止恢复，不触碰媒体或场次终态。`CreateJobObjectW` 返回非零句柄却携带异常 LastError 时由创建 helper 精确关闭一次；关闭也失败则保留稳定 isolation + cleanup 哨兵，不泄漏原生错误、不交给外层重复关闭。

### 2.3 媒体、事件与截止时间

- `open/finalizing` 媒体复用 P3-MEDIA finalizer；合法 partial/final 被登记为 `recovered`，损坏、重复、冲突和 orphan 原样保留并审计，不删除原始现场。open 恢复的结束时间首次确定后保持稳定，失败保持 finalizing 供下次重入。
- 事件从 checkpoint 后重放 durable tail，关闭 open gift fold，并推进 checkpoint `closing→closed`。缺失 checkpoint 时仍审计 event、gift fold 与 eventstore 自有的 `kind=event_persistence + reason_code=EVENT_DROPPED_LOCAL` gap；capture 启动恢复写入的 gap 不计入，避免恢复器制造自己的 durable evidence。没有证据才幂等成功，存在证据返回权威截止与永久错误。
- closed checkpoint 除拒绝残留 open fold 外，还拒绝 source 事件最大正 `ingest_sequence` 超过 `committed_sequence`；aggregate 不参与该 source 序列校验。
- durable prefix 已提交后才发现永久损坏时，重新查询 SQLite 中 `live_events.received_at` 与 `gift_combo_states.updated_at` 的权威最大时间；该读取失败则返回 deferred 并保留活动场次，禁止用旧媒体截止误终态化。
- 截止时间取场次开始、媒体与权威事件/礼物的非倒退最大值；未来时间或墙钟回退写入 `clock_uncertain`。

### 2.4 运行期恢复与应用门禁

- FFmpeg 异常使用 1/2/5/10 秒退避，10 秒封顶；单个恢复代最多 10 次或 5 分钟。成功关闭同一 `recording_restart` gap，耗尽后稳定落为 `RECORDING_RETRY_EXHAUSTED`，消息监听继续。
- operation/generation/attempt 三层 fencing 阻止旧 timer、旧 ResolveStreams 或旧进程退出覆盖新状态；恢复持久化失败不冒充成功。
- 只有真正取消/超时的父 context 立即中断全局扫描；组件自己的 context 错误按该组件失败继续审计其余场次。
- process recovery、event deferred、纯 incomplete 和未知恢复错误都在 Application 边界 fail closed，Monitor 不启动、Ready 不发布。Reporter 只输出合法 UUIDv7，损坏关联值统一为 `invalid`。

## 3. 验证证据

| 门禁 | 结果 |
| --- | --- |
| `where go` | `C:\Program Files\Go\bin\go.exe` 可用 |
| `go test ./... -count=1` | 全部 Go 包通过 |
| `go vet ./...` | 通过 |
| `go build ./...` | 通过 |
| event source-tail/local-drop-gap、启动恢复、Reporter、Global Job、应用实例租约、事件 cutoff/deferred 等重点回归 `-count=20` | 通过；包含真实独立 Windows 子进程 holder/reopen/crash-release |
| Schema v4→v5、备份/回滚、严格分页、gap/场次原子性 | 通过 |
| Node/pnpm | Node 24.18.0、pnpm 9.12.0 |
| `pnpm test` | 3 个测试文件、6 项测试通过 |
| `pnpm build` | TypeScript 检查和 Vite 生产构建通过 |
| Wails v2.13.0 `windows/amd64` 生产构建 | `cmd/desktop/build/bin/douyin-live-desktop.exe`，48,013,824 字节 |
| 生产 EXE SHA-256 | `A73F3FD44E780292F13D675E6CE438F395C1DF167F201DEB5218CCD229FE3D60` |
| `git diff --check` | 通过；Wails 生成绑定仅空白噪声已按语义 diff 审核并恢复 |

重点故障注入覆盖：畸形/无序/回退分页，process inspection 失败，Global Job 歧义 LastError、句柄单次清理和 cleanup 稳定分类，媒体缺失/损坏/orphan/重入，无 checkpoint 但有耐久事件/礼物，事件 durable prefix 后永久损坏与 cutoff 刷新失败，closed checkpoint 残留 open fold，event source-tail/local-drop-gap，父/组件 context 差异，SQLite 提交不明、gap 冲突、实例重复启动和 holder 异常退出。独立终审确认 P0=0、P1=0、残余 P2=0。

## 4. 隐私与安全结论

- Job/lease 名只含固定前缀、规范根 SHA-256 与 UUIDv7 attempt；日志、Reporter、错误和 details JSON 不含绝对路径、直播间标识、Cookie、流 URL、查询参数或 native 错误正文。
- 非法关联 ID 统一输出 `invalid`；错误对外只使用稳定托管错误码。
- 媒体恢复从不删除未知或冲突现场；缺少可靠进程所有权证据时 fail closed。
- 本验收未使用或写入任何真实直播间地址、完整流地址或用户凭据。

## 5. 已知限制与后续验收

- 当前 Windows OpenSSH 环境为 `CGO_ENABLED=0` 且没有 GCC，`go test -race` 会在启动阶段报 `-race requires cgo`；这表示竞态工具链不可用，不是源码测试失败。本任务未安装 GCC、未更改 CGO。
- Global 对象已通过同一 Windows 登录会话内的真实独立子进程创建、重开、重复拒绝和崩溃释放；没有在第二个不同登录会话（例如另一 console/RDP 会话）执行人工验收。代码使用 Windows Global namespace，该跨登录会话行为留作 P3-ACC/发布矩阵补验。
- P3-RCV 的正确性由本地确定性故障注入、真实 Windows 内核对象和 SQLite/文件系统恢复覆盖，不需要直播间在线即可验收，因此本阶段没有发起直播测试。
- P3-ACC 仍需使用授权房间完成 10 分钟连续录制、真实断网、真实 FFmpeg 崩溃、恢复后新分片/缺口、真实下播收尾与 GUI 状态一致性；这些项目未计入 P3-RCV 完成证据。

## 6. 结论

P3-RCV-001 的 4 个任务点完成，PHASE-3 进度由 22/30（73%）更新为 26/30（87%），总体进度由 62% 更新为 66%。当前任务转为 P3-UI-001，下一任务为 P3-ACC-001；发布级在线与跨登录会话补验不阻塞实时 UI 开发。
