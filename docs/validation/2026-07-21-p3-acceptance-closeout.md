# P3-ACC 稳定性、故障恢复与阶段关闭记录

- 日期：2026-07-21
- 权威工作副本：Windows `D:\douyinLive`
- 分支与关闭时 HEAD：`main` / `5b642f41660c7e4f6f19a9b0225ae2c0a8870665`
- 任务：`P3-ACC-001`
- 结论：按真实运行证据、后续修复与用户明确豁免关闭 PHASE-3；不声明严格控制器成功。

## 1. 结论口径

本记录把机器证明、用户观察、用户豁免和未运行项严格分开：

| 项目 | 结果 | 证据口径 |
| --- | --- | --- |
| 不少于 10 分钟稳定资源窗口 | `PROVEN` | 正式在线运行的稳定门禁通过 |
| FFmpeg 崩溃、新 attempt、缺口与恢复 | `PROVEN` | 正式运行的崩溃恢复阶段通过 |
| 隔离 relay 传输中断与恢复 | `PROVEN` | 正式运行的网络故障阶段通过 |
| 人工停止后的 UI finalizing | `USER_OBSERVED` | 用户批准停止并观察界面收尾 |
| 继续等待自然下播 | `USER_WAIVED/NOT_RUN` | 用户明确选择不再等待不可控的自然下播 |
| 最终机器视觉 ACK | `USER_WAIVED/NOT_RUN` | 用户明确豁免本次最终机器视觉确认 |
| `P3-ACC-CONTROLLER/v1` `PASS` / `passed=true` | `NOT_CLAIMED` | 严格最终合同未完整执行，不伪造成功报告 |

这是一项 2026-07-21 的项目级关闭例外。`scripts/p3-acc-controller.ps1`、Go hook 和 PowerShell 模块中的严格终态、独立视觉 ACK、自然退出、SQLite quick-check/unlock 与清理合同不增加豁免分支。正式发布候选仍可按原合同重新执行完整验收。

## 2. 正式运行与后续修复

正式在线运行已经跨过十分钟资源窗口，并证明当前 session/operation/attempt/restart fence 下的 FFmpeg 崩溃恢复和 relay 网络故障恢复。后续自然离线运行暴露并推动修复了以下问题：

- 历史 attempt 的媒体不完整不能污染当前干净 cleanup；质量退化与进程/媒体/sink 硬失败分开建模。
- `ErrCaptureCleanupPending` 由 worker-owned 后台重试持续拥有；调用方取消只解除等待，不丢失收尾观察者。
- 干净离线终态严格为 session `completed` 加 recording `incomplete`；`interrupted/incomplete` 仍表示 cleanup 错误并被拒绝。
- 交互式探针不再有短于外层控制器的固定一小时寿命。
- 媒体根、session/media manifest 与媒体文件读取绑定根代际、文件身份、大小和修改时间；Windows 用不共享删除的根目录 guard 排除 A→B→A。
- FFmpeg 8.1.2 的精确数字网络错误、`total_size=N/A`、有界资源采样、Job/MIC、计划任务清理和日志隐私均有正反回归。

## 3. 2026-07-21 关闭门禁

### 3.1 Go

在同一 Windows 权威工作副本中先执行 `where go`，结果为 `C:\Program Files\Go\bin\go.exe`。以下矩阵全部通过：

- `go test ./... -count=1`、`go vet ./...`、`go build ./...`
- `go test/vet/build -tags p2acceptance ./...`
- `go test/vet/build -tags p3acceptance ./...`
- `go test/vet/build -tags p3uiacceptance ./...`
- `go test/vet/build -tags p3accacceptance ./...`
- `go test ./internal/diagnostics -count=50`

显式交互式 Scheduled Task/MIC 门禁：

- 双标签但没有 `P3ACC_RUN_INTERACTIVE_TASK_TEST=1` 时立即 `FAIL`，且任务、相关进程、测试临时目录为 `0/0/0`。
- 精确设置环境变量后出现固定 `RUNNING`、`PASSED` 和 Go `PASS` 标记；结束后同三类残留仍为 `0/0/0`。

原 `cmd/main` 兼容门禁：

- `TestWsHandlerRepliesPongWithPingPayload` 通过。
- `TestRoomClientPingStillGetsPongAfterPreviousWriteDeadlineExpires` 通过。
- `go build ./cmd/main` 通过，生成的临时 `main.exe` 已删除并验证不存在。

### 3.2 前端与 PowerShell

- Node 24.18.0、npm 11.16.0、pnpm 9.12.3；NVM for Windows 路径优先。
- `pnpm --dir frontend test`：6 个文件、20 项测试全部通过。
- `pnpm --dir frontend typecheck` 与 `pnpm --dir frontend build` 通过。
- 当前 `package.json` 没有 lint 脚本，未把 lint 写成已运行或通过。
- `P3AccController.Offline.Tests.ps1`：`passed=14`。
- `P3AccController.CleanupRace.Tests.ps1`：`passed=2`。

### 3.3 Windows 构建产物

按固定顺序从 `cmd\desktop` 运行 production clean build，再运行不带 clean/skipbindings 的 P3ACC build；随后从仓库根以 `p3accacceptance` tag 构建 launcher 和 relay。Wails 生成的 `models.ts` 只有制表空白和末尾空行噪声，已用可审核反向补丁恢复，语义 diff 为零，`git diff --check` 通过。

| 产物 | 字节 | SHA-256 |
| --- | ---: | --- |
| `douyin-live-desktop.exe` | 48,323,072 | `9373088ce5e3eab609a4dad5d656cc7ed7bc1ebbc277220eb6a21109e12804c7` |
| `douyinLive-p3acc.exe` | 48,678,400 | `bb94aff219cee75fc9e70443a28959624e233786775267432e2876788ca0ed7b` |
| `p3acclauncher.exe` | 3,536,896 | `c3bc8bbe44d383d813a55f86430bd94a0c086b0da4425b90ed8e836cf050e404` |
| `p3accproxy.exe` | 3,667,968 | `30a0f052c482ce736dba2cc9928fba285a0848109c0f4a5fa111755d667d5337` |

## 4. 清理、隐私与保留数据

- 本轮门禁后的 `DouyinLive.P3ACC.*` 计划任务、P3ACC 相关进程和 `TestP3ACC*` 临时目录计数为 `0/0/0`。
- 正式运行的 private control root、控制器、应用、launcher、relay 和相关计划任务已确认无残留。
- 一份正式运行数据根作为诊断证据保留，约 19.57 GB；此前只读审计确认其为父目录唯一直接子项、整棵树无 reparse、主数据库可独占打开。
- 删除该数据不可恢复，且本次“继续开发”没有明确授予删除约 19 GB 正式数据的权限，因此未执行删除；不能把本次关闭描述为控制器零数据残留。
- 文档与报告没有写入直播间标识、Cookie、完整流 URL、响应头、文件内容、绝对运行根、PID、命令行或原始 stderr。

## 5. 未运行与后续边界

- `go env CGO_ENABLED` 为 `0`，`where gcc` 找不到编译器；`go test -race` 会在启动阶段失败，因此未运行。没有安装 GCC/MinGW，也没有更改 CGO/PATH。
- 自然下播等待与最终机器视觉 ACK 是用户豁免，不是测试通过。
- 未提交、未推送；提交和远端同步仍需用户明确授权。
- 下一任务是 `P4-PLY-001`：Schema v6、只读历史查询、稳定 keyset cursor、统一时间轴和隐私安全 playback DTO。
