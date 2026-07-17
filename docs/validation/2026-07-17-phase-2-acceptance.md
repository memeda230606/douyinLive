# Phase 2 Wails 桌面与房间管理验收记录

> 日期：2026-07-17
> 任务：`P2-ACC-001`
> 验收基线：`main@161545b964c60fb7ad0f7dbfebbdfafc76f5d98c` 加本任务待提交变更
> 结论：通过

## 1. 范围与隐私边界

本次使用用户明确授权的测试直播间验证真实 Wails GUI，但不在源码、命令结果、验收 JSON 或本文记录直播间标识、完整 URL、Cookie、签名或流地址。测试运行在独立 `LOCALAPPDATA` 与隔离根内；开始监控前关闭自动录制，因此没有录制直播媒体。

验收只持久化严格白名单 DTO。测试直播间输入通过一次性文件注入，运行结束后连同隔离数据库、WebView 数据、设置、日志、结果和计划任务精确删除；清理后临时根不存在，匹配的桌面进程与计划任务均为 0。

## 2. 验收方式

- `p2acceptance` 构建标签启用真实 DOM 操作钩子；普通生产构建只编译空实现，不包含结果回报方法或嵌入脚本。
- 钩子在执行任何 CRUD 前 fail-closed 校验 `P2ACC_ROOT`、结果路径、录制目录及应用实际 `StorageRoot` 均位于同一隔离根；配置不满足时不注入脚本。
- 每轮由同一交互式 Windows 会话启动真实 Wails 窗口；关闭时按精确 PID 枚举可见窗口并投递 `WM_CLOSE`，再对原进程执行 `WaitForExit(10000)`。
- 验收成功状态拒绝“需要处理”，删除动作要求且只允许一次确认。

## 3. 三轮真实 GUI 结果

| 轮次 | 操作与检查 | 结果 |
| --- | --- | --- |
| 1 | 首次启动；新增房间、编辑别名与录制策略、关闭自动录制、启动监控；保存录制目录、默认质量、分片时长、并发上限、磁盘阈值和隐私设置 | `roomCreated`、`roomEdited`、`monitoringActive`、`settingsSaved` 全为 `true`；状态为“正在连接” |
| 2 | 重启；验证房间、编辑值、监控自动恢复和设置持久化；停止监控；经一次确认删除房间 | `roomPersisted`、`monitoringRestored`、`settingsPersisted`、`monitoringStopped`、`roomDeleted` 全为 `true`；`confirmationCount=1` |
| 3 | 再次重启；验证删除结果与设置仍持久化 | `deletionPersisted`、`settingsPersisted` 均为 `true` |

关闭证据：

| 轮次 | 关闭前状态 | `WM_CLOSE` | 自然退出 | 用时 | 退出码 |
| --- | --- | --- | --- | ---: | ---: |
| 1 | 监控活动 | 已投递 | 是 | 61 ms | 0 |
| 2 | 房间已删除 | 已投递 | 是 | 56 ms | 0 |
| 3 | 二次重启核验完成 | 已投递 | 是 | 56 ms | 0 |

三轮结束后匹配生产可执行路径的残留进程为 0，满足最长 10 秒退出要求。

## 4. 自动化与构建门禁

以下命令在 Windows 权威副本 `D:\douyinLive` 执行并通过：

- `where go`：`C:\Program Files\Go\bin\go.exe`；`go version go1.26.4 windows/amd64`。
- `go test -count=1 ./...`：全部包通过。
- `go test -count=1 -tags p2acceptance ./cmd/desktop`：通过；包含隔离路径、实际存储根越界和错误状态假通过测试。
- `go vet ./...`、`go build ./...`、`go build ./cmd/main`：通过。
- `pnpm typecheck`、`pnpm test`：3 个测试文件、4 项测试全部通过；`pnpm build`：通过。
- `wails build -clean -platform windows/amd64 -tags p2acceptance -skipbindings`：验收构建通过。
- `wails build -clean -platform windows/amd64 -skipbindings`：普通 Windows/amd64 生产构建通过。
- 普通生产构建真实窗口冒烟：可见窗口存在，`WM_CLOSE` 后 53 ms 自然退出，退出码 0，残留进程 0。

生产隔离核对：普通构建 Go 文件为 `acceptance_hook_disabled.go app.go main.go`；加入 `p2acceptance` 标签后才替换为 `acceptance_hook_p2.go app.go main.go`。

当前 SSH 工具链为 `CGO_ENABLED=0` 且无 GCC，故 `-race` 不可启动；这属于既知环境覆盖缺口，不是源码测试失败。本次状态机、隔离边界、真实 GUI 与重复重启证据均已覆盖 Phase 2 退出标准。

## 5. 边界说明

本次证明的是关闭窗口会触发 Wails 生命周期收尾并在 10 秒内退出，不把系统托盘、最小化驻留或独立“退出应用”入口声明为已实现。若后续产品计划引入托盘语义，应新增相应 UI 生命周期与真实交互验收，不能复用本记录替代。