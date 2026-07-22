# P5 稳定性与发布故障验证记录

## 1. 结论

`P5-STB-001` 已完成。Windows 权威工作副本中的严格 `p5stbacceptance` 门禁真实运行 3600 秒，固定开始/通过标记齐全，原子结果为 `P5-STB-001/v1`、`passed=true`，61 个分钟样本完整。四房间等待/模拟开播、资源趋势、数据库忙、网络中断、磁盘满分类和强制退出启动恢复均通过。

本任务关闭后 PHASE-5 完成 8/10 点，项目总完成度 98%；下一任务为 `P5-ACC-001` 最终发布验收与发布清单。

## 2. 交付

- `internal/app/p5_stability_acceptance_windows_test.go`
  - 仅在 `p5stbacceptance && windows` 编译；正式门禁缺少精确 `P5STB_RUN_60M=1` 时立即失败，不以 `Skip` 冒充通过。
  - 4 个合成房间：2 个持续等待，2 个周期模拟开播/下播；复用真实 `Application`、SQLite/WAL、事件 spool、CaptureCoordinator 和 Room Monitor，不访问真实直播间。
  - 每分钟采样工作集、私有内存、句柄、goroutine、CPU、进程 I/O、数据库/WAL 与事件队列；结果只含 allowlist 数值和状态计数。
  - 内置数据库 `BEGIN IMMEDIATE` 忙、合成网络断开/重连、退出码 93 子进程强制结束/同根重启恢复，并在全部断言后原子发布结果。
- `scripts/test-p5-stability.ps1`
  - 快速模式执行磁盘满/永久资源错误回归、短时稳定性夹具与 tagged vet；`-Run60Minutes` 才执行严格 3600 秒门禁。
  - 使用直接拥有的 `.NET Process` 分离 stdout/stderr、有界等待并检查真实退出码；不依赖 PowerShell 对原生 stderr 的错误包装。
- `internal/capture/recorder.go`
  - 增加 Windows/通用磁盘满文字的精确 allowlist，继续拒绝未知数字错误的宽泛分类。
- `internal/eventstore/queue_stats.go`
  - 将既有隐私安全、无会话 ID 的队列聚合采样复用于 P5 验收标签。

## 3. 60 分钟正式证据

命令：

```powershell
powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File scripts/test-p5-stability.ps1 `
  -Run60Minutes -ResultRoot <独立空目录>
```

本次直接门禁输出：

```text
P5STB_60M_RUNNING
P5STB_60M_PASSED
PASS
TestP5STB60MinuteStability 3601.37s
internal/app total 3603.801s
```

| 指标 | 结果 | 门禁 |
| --- | ---: | ---: |
| 样本/时长 | 61 / 3600 秒 | 必须严格相等 |
| 房间/场次/事件 | 4 / 13 / 18984 | 均大于 0 |
| 状态发布 P95 | 0 ms | < 1000 ms |
| 平均 CPU | 0.235% | < 10% |
| 工作集首/尾/峰值 | 36409344 / 62525440 / 67682304 B | 尾值 < 300 MiB，增长 < 64 MiB |
| 私有内存首/尾/峰值 | 72822784 / 96399360 / 101707776 B | 记录趋势 |
| 句柄首/尾/峰值 | 191 / 222 / 224 | 增长 < 64 |
| goroutine 首/尾/峰值 | 10 / 11 / 15 | 增长 < 64 |
| WAL 峰值 | 4255992 B | < 64 MiB |
| 数据库忙队列峰值 | 26 | > 0 且释放后恢复 |

结果 JSON SHA-256：`b0edc69d6f6b46d16afa7b1da24adaba344ccd02753ac7a4709dc0d4c0cbd5d9`。独立运行根仅含合成数据；完成复核后精确删除，不提交生成结果。

## 4. 故障门禁

- 数据库忙：独占 writer 连接执行 `BEGIN IMMEDIATE`，消息 callback 继续有界入队；瞬时队列峰值 26，回滚并释放连接后数据库恢复，最终 18984 条事件持久化。
- 网络：模拟连接在场次中途返回错误；状态流观察到 `RECONNECTING`，随后更高 revision 恢复 `LIVE`。
- 磁盘满：Windows FFmpeg 文字与通用文字均分类为 `RECORDER_LOCAL_RESOURCE`；永久本地资源错误不进入录制重试循环，消息采集状态语义保持。
- 强制退出：子测试进程建立真实活动场次后以 93 退出；同一数据根新进程执行启动恢复，活动场次计数归零，随后数据库句柄可释放。

## 5. 其他门禁与限制

- 快速控制器、磁盘满分类和永久资源错误均重复通过；`go test ./...`、`go vet ./...`、`go build ./...`、tagged vet、前端 10 文件 36 项测试/typecheck/build、production Wails 构建及 `git diff --check` 全部通过。
- 本次数据与网络均为本地合成，不使用真实 Cookie、直播 URL、主播信息或业务运行根。
- 当前 OpenSSH 会话仍为 `CGO_ENABLED=0` 且没有 GCC，`go test -race` 无法启动；这是环境限制，不是源码失败。
- 正式代码签名、病毒扫描、Windows 10 实机及最终发布清单属于 `P5-ACC-001`，本任务不冒充这些外部证据。
