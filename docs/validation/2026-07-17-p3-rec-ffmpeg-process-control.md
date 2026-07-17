# P3-REC FFmpeg 与进程控制验收记录

> 日期：2026-07-17
> 权威工作副本：Windows `D:\douyinLive`
> 范围：FFmpeg/ffprobe 发现、流候选选择、录制 attempt、进度解析、Windows Job Object、进程停止、异步退出协调与隐私边界

## 1. 结论

P3-REC-001 已完成。录制器能从显式配置、随包工具和 `PATH` 按顺序发现并校验成对的 FFmpeg/ffprobe，按稳定候选序列逐一启动，在签名地址变化时重新解析并避免重复尝试同一精确 URL。启动只有收到 `progress=continue` 且进程仍存活时才提交为活动 attempt，解决了“先报 ready、随即退出”导致的假录制竞态。

FFmpeg 生命周期独立于房间监督器调用 context。Windows 进程按 `CREATE_SUSPENDED`→Assign Job Object→`NtResumeProcess` 启动，任一步失败都 fail closed；停止顺序为 `q`→等待 5 秒→终止 Job 进程树→等待 3 秒→终止根进程/关闭句柄。进程退出事件通过 attempt 相关标识与仓储 CAS 协调，瞬时仓储错误会异步重试，旧 attempt 事件不能降级新录制。

真实 FFmpeg 8.1.2 本地 HTTP 流验收已生成 `.mkv.partial`，优雅停止后可由 ffprobe 识别为 Matroska。全量门禁及独立只读终审未发现 P0/P1。

## 2. 依赖发现、参数与隐私

- 发现顺序固定为显式绝对路径、可信随包目录、`PATH`；FFmpeg 与 ffprobe 必须成对存在并在 5 秒内通过 `-version`。
- 版本输出最多读取 64 KiB；持久化诊断只含版本、脱敏构建摘要和 SHA-256，不含本机路径或任意命令行参数。
- 本次工具为 FFmpeg 8.1.2；`ffmpeg.exe` SHA-256 为 `1326DDE4C84FF1F96FE6B8916C5BED29E163E9B5DCCF995F6F3DB069D143EC5E`，`ffprobe.exe` 为 `B49CCC7C6547B141AD5A2F6EC69CC04323D7133D7704D70B331B904C63EECB07`。
- FFmpeg 使用独立参数数组：隐藏 banner、warning 日志、`-nostats`、`-progress pipe:1`、15 秒读写超时、可选音视频 map、`-c copy`、Matroska segment、时间戳复位和 `.mkv.partial` 输出。
- 分片时长只接受 300–1800 秒，默认 600 秒；设置和房间写入契约统一为 5–30 分钟。
- 完整流 URL 只在 Go 内存和子进程参数中短暂存在；错误、fmt/GoString、JSON、slog、DTO 和构建摘要均有脱敏测试。

## 3. 候选、启动与 attempt 隔离

- 候选排序在副本上稳定执行：质量优先，兼容模式优先 H.264，同质量优先 FLV，再比较已知正码率；仅 H.265 时仍可降级录制。
- 每个解析快照按精确 URL SHA-256 去重；旧候选失败后重新解析，签名查询变化的新地址可再次尝试。
- 每次启动创建 UUIDv7 attempt 和独占 `.attempt-*` 目录；目录已存在即失败，不允许复用或覆盖已有分片。文件名包含分片序号、UTC 纳秒和 attempt 标识。
- `-progress` 由 16 KiB 有界解析器读取，支持 `out_time_us=N/A` 和带前导空格的 speed；超长、畸形或溢出输入返回稳定错误。
- 只有 `progress=continue` 可进入活动状态；`progress=end`、启动超时或早退均进入候选降级。stderr 只保留 64 KiB 已脱敏尾部。
- 403/404/410/EOF、临时网络和不支持输入可尝试后续候选；本地路径/权限和依赖错误立即停止，避免无意义重试。

## 4. 进程、Job Object 与停止

- 调用方 context 只限制启动和等待；已启动进程由私有生命周期 context 管理，调用方取消不会杀死 FFmpeg 或提前归还并发令牌。
- 每个进程只有一个 `Wait` owner；stdout/stderr 的 EOF 或非 EOF 终止错误都释放排空屏障，真实退出和输出排空后才释放容量。
- Windows Job 在进程启动前创建并设置 `KILL_ON_JOB_CLOSE`；根进程以 suspended 状态启动，成功 Assign 后才恢复，消除子进程先逃逸的窗口。
- Job 创建、配置、Assign 或 Resume 失败都终止已启动进程并关闭资源；错误只暴露稳定分类。
- `Stop` 幂等执行 `q`、5 秒等待、Job 进程树终止、3 秒等待、根进程终止/句柄关闭；超时只限制调用方等待，后台 watcher 仍负责最终回收。
- 真实 Windows 测试证明父进程与孙进程同属 Job 并可整树终止；suspended 测试证明 Assign/Resume 前子进程不会执行 marker 动作。

## 5. 协调、应用接线与配置兼容

- 录制器以有界最新事件通道发布异步退出；协调器以 attempt 相关 ID 验证事件，仓储暂时失败时按 25 毫秒到 1 秒退避重试，确认 `RecordingUnavailable` 后才停止并释放录制器。
- Rebind/Finalize 可串行取代旧操作；排队的旧 attempt 事件不能覆盖新状态。
- 应用启动时只在全局录制目录等于规范化场次根时接入录制器；依赖缺失不阻止消息采集。外部录制根在 P3-MEDIA 注册机制完成前 fail closed，并在设置页明确提示。
- 设置文件升级到 v2，将旧全局分片值收敛到 5–30 分钟。旧房间行仍可读取和编辑，超出新范围时录制安全失败，等待用户保存合规值；后续数据迁移需保持这一兼容边界可审计。

## 6. 自动化门禁

以下命令均在 Windows 权威工作副本执行并通过：

- `where go`：`C:\Program Files\Go\bin\go.exe`。
- `go test -count=1 ./...`、`go vet ./...`、`go build ./...`、`go build ./cmd/main`。
- `go test -count=20 ./internal/capture`。
- 启动 ready→立即退出竞态、启动错误分类、构造失败延迟释放、分级停止、协调器退出重试、进程配置隐私等高风险路径各 100 轮通过。
- 真实 Windows Job 父子进程树终止 20 轮、suspended→Assign→Resume 10 轮通过。
- `go test -count=1 -tags p2acceptance ./cmd/desktop`。
- `go test -v -count=1 -tags p3acceptance ./internal/app ./internal/capture`：应用验收未注入地址时安全跳过；真实本地 HTTP MPEG-TS→FFmpeg→Matroska 分片与 ffprobe 可读性验收通过。
- 使用用户授权直播间运行 `TestP3CaptureIsolationAcceptance`：平台返回离线，以 `P3ACC_OFFLINE` 安全跳过。
- 前端 `pnpm typecheck`、3 个 Vitest 文件共 6 项测试、`pnpm build`。
- `wails build -clean -platform windows/amd64 -tags p2acceptance -skipbindings`。
- `wails build -clean -platform windows/amd64 -skipbindings`。
- 对全部 tracked/untracked 非忽略文件扫描授权房间标识、地址和查询参数，匹配数为 0；`git diff --check` 通过。

当前 Windows SSH Go 环境为 `CGO_ENABLED=0` 且没有 GCC，因此未启动 `go test -race`；这是已记录的工具链边界，不是源码失败。

## 7. 授权直播间与隐私边界

授权直播间只通过 `P3ACC_LIVE_URL` 在进程环境中注入，没有写入源码、测试、文档或持久化日志。本次平台判定房间离线，测试按协议安全跳过，不把离线跳过描述为在线录制成功；P3-ACC 仍需完成在线 10 分钟、断网、FFmpeg 崩溃和下播收尾验收。

流 URL 不进入 JSON DTO、数据库、manifest、前端或持久化结构化日志。候选、进程配置、录制 spec、工厂选项和 ResolvedStream 的 fmt、`%#v`、JSON、slog 均验证为脱敏输出。

## 8. 已知边界与下一步

P3-MEDIA-001 继续负责：

- 对每个 `.mkv.partial` 执行 ffprobe，成功后原子改名并登记 `media.json`；
- 生成音频代理、场次媒体摘要和自定义录制根注册；
- 保留不可读 partial 和 attempt 目录供恢复审计，不静默删除。

P3-RCV-001 负责 1/2/5/10 秒退避、重启上限、`capture_gaps` 和启动恢复；P3-UI-001 负责发布实时录制进度与缺口告警。

当前公共 `ResolveStreams` 没有 context 参数，录制器因此同步调用并依赖底层请求自身超时，避免放弃 goroutine；候选快照尚无额外硬数量上限，但每轮按精确 URL 去重且解析轮次有界。这两项作为非阻断 P2 继续跟踪，不影响本节已验证的进程安全与隐私边界。
