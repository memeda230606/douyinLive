# P3-CAP 场次生命周期与监督器编排验收记录

> 日期：2026-07-17
> 权威工作副本：Windows `D:\douyinLive`
> 范围：Schema v2、场次仓储、CaptureCoordinator、RoomSupervisor、manifest 恢复、应用收尾和前端状态契约

## 1. 结论

P3-CAP-001 已完成。每个房间至多存在一个 `starting/recording/finalizing` 活动场次；消息场次状态与录制状态分离，禁用录制或暂时无流不会错误结束事件场次。Open、Rebind、Finalize 均受 operation ID 与旧状态 CAS 保护，断线重连复用同一 session，只有可靠下播、用户停止或应用退出才进入最终收尾。

数据库是场次事实来源，`session.json` 是可修复镜像。数据库事务把 `manifest_dirty` 置为 1；镜像同目录原子替换并完成健康日志 Sync 后，再以完整版本 CAS 清零。任何文件、日志或清标失败都保留可恢复证据，启动修复按 128 条 keyset 分页处理脏记录和活动场次。

最终只读审计未发现可复现 P1/P2。

## 2. 自动化门禁

以下命令均在 Windows 权威工作副本执行并通过：

- `where go`：`C:\Program Files\Go\bin\go.exe`。
- `go test -count=1 ./...`。
- `go test -count=20 ./internal/capture ./internal/room ./internal/app ./internal/storage ./cmd/desktop`。
- `go test -count=1 -tags p2acceptance ./cmd/desktop`。
- `go test -v -count=1 -tags p3acceptance ./internal/app`：未提供直播间环境变量时以 `P3ACC_ENV_NOT_SET` 安全跳过。
- `go vet ./...`、`go build ./...`、`go build ./cmd/main`。
- 前端 `pnpm typecheck`、Vitest 3 个文件共 5 项测试、`pnpm build`。
- `wails build -clean -platform windows/amd64 -tags p2acceptance -skipbindings`。
- `wails build -clean -platform windows/amd64 -skipbindings`。
- `git diff --check`。

当前 Windows SSH 环境为 `CGO_ENABLED=0` 且没有 GCC，因此未启动 `go test -race`；这是已记录的工具链边界，不是源码测试失败。

## 3. 关键故障与并发回归

自动化测试覆盖：

- Schema v1→v2 备份、回填、约束和迁移失败全回滚。
- 同房间活动唯一索引、operation ID 幂等与旧状态 CAS。
- Recorder/EventSink 正常、禁用、无流、重绑、清理失败、panic 和 Finalize 重试。
- 两次可靠离线确认才收尾；STOP、自动下播和应用关闭并发时只有一个终态意图获胜。
- 首个关闭调用超时或已取消时，Monitor/Application 仍在后台共享排空；SQLite 与日志只在消费者完成后关闭。
- `STOPPING` 期间第二数据根初始化被拒绝，清理完成后无资源复活。
- manifest 健康日志 Sync 失败、清标中断、陈旧 CAS、旧 repair 与新 Transition 交错以及严格单调版本。
- 129 条脏终态记录跨两页全部修复；两个批量事件的 outstanding 从 2 正确递减到 0，每页仅一次 Sync。

## 4. 授权直播间边界

使用用户明确授权的测试直播间运行隔离验收时，平台当时明确返回离线，因此测试以 `P3ACC_OFFLINE` 安全跳过。跳过前后仍完成 StopMonitoring、Application Shutdown、数据库读写句柄关闭、Monitor 关闭和隔离数据库可重命名检查。

这项结果只证明离线分支与资源清理安全，不声明真实在线场次创建、重连或下播收尾已经通过；在线 10 分钟、断网和 FFmpeg 故障的真实 GUI 验收保留给 P3-ACC-001。

## 5. 隐私检查

- 验收代码只从 `P3ACC_LIVE_URL` 读取地址，未硬编码直播间。
- 对 171 个 tracked/untracked、非 ignored 文件执行固定字符串扫描，未发现授权直播间标识、查询参数或完整地址。
- 源码、文档、日志和前端 DTO 不包含 Cookie、签名或完整流 URL；manifest 健康事件只记录稳定错误码、session UUID 和计数。

## 6. 下一步

进入 P3-EVT-001：实现 Schema v3 事件 checkpoint、有界顺序队列、raw binpack/WAL 耐久顺序、allowlist 标准化、HMAC 用户身份、去重、礼物连击和 SQLite 批写。
