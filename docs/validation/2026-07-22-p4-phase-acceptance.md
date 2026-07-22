# P4 回放与基础分析一体化验收记录

- 日期：2026-07-22
- 任务：`P4-ACC-001`
- 权威工作副本：`GJS-20250801EFK:D:\douyinLive`
- 基线分支：`main`
- 验收结论：`PASS`，PHASE-4 关闭

## 1. 验收范围

本次在独立数据根和真实交互式 Windows 会话中贯通以下链路：

1. 历史场次查询与安全媒体读取。
2. 两段真实 H.264/AAC MP4 解码、时间轴定位和跨段续播。
3. 基础分析生成与相同输入报告复用。
4. ASR 未配置时的明确降级与基础统计可用性。
5. `analysis-export/v1` 报告包结构、编码与隐私边界。
6. Wails/WebView2 可见窗口布局与隔离运行零残留。

## 2. 实现中发现并修正的问题

- `capture_offset_ms` 原先只参与媒体定位，没有从媒体 DTO 的可见时间轴扣除，导致校准场次混用墙钟偏移域和事件偏移域。现在列表与定位都统一到场次事件偏移域。
- 播放器原先没有在当前分片自然结束后进入下一分片。现在只对时间上相邻（容差 250 ms）且状态为已验证 MP4 的下一段自动续播；真实缺口会停在当前分片末尾，不会被静默跳过。
- 验收器最初把允许导出的伪匿名 `user_hash` 固定值当成隐私泄漏。最终门禁改为禁止原始展示名、业务操作/路径和凭据/URL 标记，同时继续允许合同规定的 `user_hash`。

## 3. 严格交互式验收

`p4accacceptance` 构建标签创建两个 4 秒、640×360、H.264/AAC 的本地媒体分片和固定事件集。交互式计划任务中的真实 Wails/WebView2 在同一次冷启动中返回 `P4-ACC-001/v1`：

- `historyVisible=true`
- `mediaDecoded=true`
- `crossSegmentAdvance=true`
- `timelineAligned=true`
- `analysisVisible=true`
- `asrDegraded=true`
- `exportVisible=true`
- `privacySafe=true`
- `layoutUsable=true`

20 个固定校准样本覆盖两段媒体，P95 定位误差为 0 ms，满足不高于 1.5 秒的门槛。相同输入连续分析的报告 ID、分析版本和完整 DTO 完全一致。

导出根只包含一个发布目录，目录内严格为 `manifest.json`、`events.csv`、`media-segments.csv`、`metric-buckets.csv` 和 `transcripts.csv`；四个 CSV 均带 UTF-8 BOM，manifest schema 为 `analysis-export/v1`，隐私扫描通过。

精确顶层窗口由 `PrintWindow(PW_RENDERFULLCONTENT)` 抓取：

- 尺寸：1440×900
- SHA-256：`c6d4d4dcc632e79a4365303608b3bc46a4b208de4f7e52c23afe33ba6326c05e`
- 结果哈希与文件复算一致
- 人工复核：分析标题、场次选择、ASR 降级说明、四项基础统计和趋势区均清晰可见、无遮挡或旧帧

验收结束后，精确计划任务、应用进程和隔离运行根均为 0。

## 4. 自动化与构建门禁

- `where go`：`C:\Program Files\Go\bin\go.exe`
- `go test ./...`：通过
- `go vet ./...`：通过
- `go build ./...`：通过
- `go test -tags p4accacceptance ./cmd/desktop -count=1`：通过
- `go vet -tags p4accacceptance ./cmd/desktop`：通过
- `pnpm test -- --run`：10 个文件、36 项测试通过
- `pnpm typecheck`：通过
- `pnpm build`：通过
- `wails build -platform windows/amd64 -tags p4accacceptance -o douyinLive-p4acc.exe`：通过
- `wails build -clean -platform windows/amd64`：通过
- production EXE：48,712,704 字节，SHA-256 `c509622776f43db78ddd2589bb91a1d5e3d74110477ce50343b0c018c45be2a8`

Wails 生产构建只产生 `models.ts` 文件末尾空白噪声，已审核语义 diff 并用反向补丁恢复。当前 OpenSSH 环境为 `CGO_ENABLED=0` 且 `where gcc` 无结果，因此 `go test -race` 未启动；这是已知工具链限制，不记为源码测试失败。

## 5. 阶段结论

P4 的回放、统一时间轴、基础分析、ASR 可插拔降级和隐私导出已形成可见、可重复、可验证的一体化闭环。PHASE-4 任务点 20/20 完成，项目按阶段权重从 88% 更新为 90%，下一任务为 `P5-ENG-001` 发布工程与供应链门禁。
