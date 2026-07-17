# P3-EVT 有界事件耐久链路验收记录

> 日期：2026-07-17
> 权威工作副本：Windows `D:\douyinLive`
> 范围：Schema v3、事件接纳队列、raw binpack/WAL、标准化与隐私、去重、礼物折叠、SQLite 批写、drop ledger 和 checkpoint 恢复

## 1. 结论

P3-EVT-001 已完成。直播回调只复制必要数据并非阻塞接纳到单一 FIFO；队列同时受条数和 payload 字节限制，紧急额度只是同一顺序中的预留容量。标准事件总线先于旧版同步处理器交付，并为每个订阅者创建独立 protobuf 快照，因此旧处理器阻塞或订阅者修改消息都不会拖住或污染耐久订阅。

每批先写 raw binpack，再写只含 envelope 元数据和 raw 引用的 WAL，并严格完成 raw Sync→WAL Sync 后才标准化和写 SQLite。source、aggregate、礼物折叠、缺口和 checkpoint 在同一事务中提交；同位置重放不能回填新的 source。数据库降级期间不建立无界 deferred map，恢复只从 checkpoint 尾部固定批次重放。

最终只读审计未发现剩余 P0/P1。`P3-RCV-001` 负责的跨进程场次终态编排仍按计划延后，不阻塞本节。

## 2. Schema v3 与事务边界

- `live_events` 增加 `ingest_sequence`、`event_role`、解析错误码和标准化器版本；source 序列唯一，aggregate 可共享来源序列。
- `event_ingest_checkpoints` 保存状态、最后提交序列、privacy key ID 及 WAL/raw 双游标。
- `gift_combo_states` 保存 open/closed 折叠状态；closed 行的 15 个业务字段由触发器保持不可变。
- `capture_gaps` 增加 `event_persistence` 类型和稳定 dedupe key。
- v2→v3 迁移、备份、历史行回填、失败回滚、外键和约束均有自动化覆盖。

Writer 使用旧序列 CAS；任何 source、aggregate、礼物状态、缺口或 checkpoint 失败都会回滚整个事务。相同 checkpoint 只允许迟到 aggregate/缺口等幂等派生更新，不允许插入新的 source。

## 3. Spool、故障与恢复

- raw 帧含版本、压缩标志、长度和 CRC32C；符合收益条件时使用受限 zstd。WAL 不复制 payload，只保存 raw 引用。
- raw 128 MiB、WAL 64 MiB 或跨 UTC 小时轮转；每场次 raw+WAL 默认总上限 4 GiB，恢复文件计入同一预算。
- `AppendBatch` 在写入前完成序列、编码、轮转和总容量预检；容量不足时文件集合、尺寸和 SHA-256 均不变。
- 跨分片真实 write/Sync 故障只返回已经 raw+WAL 双 Sync 的精确有序前缀；未确认尾回滚到上次双 Sync 游标，sink 只为未耐久后缀累计缺口。
- repair 先只读分析全部分片、连续索引、帧边界和 raw 引用，再验证 checkpoint；只有可证明的最终崩溃尾允许截断。
- checkpoint 前损坏、缺失中段、segment index 缺口、坏引用后又出现有效记录、privacy key 不匹配或 WAL/raw 双游标不一致均 fail closed，文件和数据库游标不变。

drop ledger 以临时文件、Sync 和原子替换保存累计/已确认计数。数据库 gap 提交成功后才确认 sidecar；旧确认不能覆盖后来新增丢失，spool fatal 进入等待时立即落账而不是等待首个 ticker。进程在内存增量合并到 sidecar 前被强制终止仍存在固有小窗口；正常本地 I/O 下限定在当前操作与一个批次周期内，系统调用永久阻塞时无法给出绝对上界。

## 4. 标准化、隐私、去重与礼物

- allowlist 标准化 chat/gift/like/member/follow/system/unknown；解析错误只保存稳定错误码。
- 用户身份使用独立 32 字节凭据的 HMAC-SHA256；checkpoint 固定非敏感 key ID。
- `SaveDisplayNames` 在启动时读取，并在设置成功更新后原子应用到已打开及未来场次；既有行不回写。
- Deduplicator 只有在 SQLite 事务成功后才缓存 key，并受 2 分钟 TTL 与 65,536 条容量限制；SQLite 唯一约束是最终去重事实来源。
- 礼物折叠只加载当前批触及的 open 状态；closed 状态留在数据库。缓存命中的连续重复礼物仍参与 idle 检查，跨空闲阈值会正确闭合 aggregate。

## 5. 自动化门禁

以下命令均在 Windows 权威工作副本执行并通过：

- `where go`：`C:\Program Files\Go\bin\go.exe`。
- `go test -count=1 ./...`。
- `go test -count=20 . ./internal/eventstore ./internal/capture ./internal/storage ./internal/app ./internal/room ./cmd/desktop`。
- 容量零写入、跨段 durable prefix、rollback terminal、checkpoint 修复零变更、有效 raw 游标不一致、满队列零复制和 cached gift idle 共 7 项高风险回归，各 100 轮通过。
- `go vet ./...`、`go build ./...`、`go build ./cmd/main`。
- `go test -count=1 -tags p2acceptance ./cmd/desktop`。
- `go test -v -count=1 -tags p3acceptance ./internal/app`：未提供环境变量时以 `P3ACC_ENV_NOT_SET` 安全跳过；授权直播间运行时平台返回离线，以 `P3ACC_OFFLINE` 安全跳过。
- 前端 `pnpm typecheck`、3 个 Vitest 文件共 5 项测试、`pnpm build`。
- `wails build -clean -platform windows/amd64 -tags p2acceptance -skipbindings`。
- `wails build -clean -platform windows/amd64 -skipbindings`。
- `git diff --check`。

当前 Windows SSH 环境为 `CGO_ENABLED=0` 且没有 GCC，因此未启动 `go test -race`；这是已记录的工具链边界，不是源码失败。

## 6. 授权直播间与隐私边界

用户授权直播间在本次隔离验收时被平台明确判定为离线，测试以 `P3ACC_OFFLINE` 安全跳过。跳过前后完成应用 Shutdown、数据库关闭和隔离根清理，不声明真实在线事件接纳已经通过；在线 10 分钟、断网、FFmpeg 崩溃和下播收尾仍由 `P3-ACC-001` 统一验收。

验收地址只通过 `P3ACC_LIVE_URL` 注入，未硬编码进源码或文档。对排除 `.git`、依赖、构建产物后的 209 个工作树文件扫描授权房间标识、查询参数和完整地址，匹配数为 0。源码、测试、文档、SQLite 字段和日志不保存 Cookie、签名或完整流 URL；raw payload 是场次内受限、带校验的恢复材料，数据库只保存相对引用。

## 7. 已知边界与下一步

`P3-RCV-001` 继续负责：

- terminal `interrupted/degraded` 场次的启动恢复编排；
- 活动孤儿场次完成恢复后的终态推进和唯一索引解锁。

当前 raw/WAL/drop ledger 都会保留，不会静默丢弃。下一步进入 P3-REC-001，实现 FFmpeg、流候选、Windows Job Object、进度解析和分级进程停止。
