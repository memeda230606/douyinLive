# P4-ASR-001 ASR 插件接口与降级验证记录

日期：2026-07-21
权威工作副本：`GJS-20250801EFK:D:\douyinLive`
基线：`main@a161b8b2f614ddc0f36da3cd3fc22890401b347d`

## 交付范围

- 新增内部 `ASRProvider` 的 `ID/Validate/Transcribe` 适配器边界、可取消 context、音频输入、进度和转写段合同。
- 默认装配无 I/O 的 `disabled` 提供器；未配置返回稳定 `ASR_NOT_CONFIGURED`，调用方取消保持原 context 错误，且不调用进度回调。
- `InfrastructureOptions` 可替换注入后续本地或远程 provider；未引入云 SDK、网络前置、API Key 或固定模型。
- 新增 Wails `GetASRStatus` 与前端 strict v1 schema，仅允许 provider ID、能力状态和稳定错误码；原始错误、端点、模型、路径、摘要和凭据不出 Go 边界。
- 分析页在 disabled/未配置与 unavailable 时给出不同提示，同时继续显示已有基础报告；ready 时隐藏降级提示。

## 自动化证据

1. ASR 与基础分析专项：
   - `go test ./internal/analysis -count=20`：通过。
   - 覆盖 disabled、取消、进度不调用、ready/unavailable/未配置状态、危险/typed-nil provider、状态隐私和动态 ID 门禁。
   - 基础分析注入 Validate/Transcribe 调用即 panic 的 provider，报告生成、输入指纹复用和版本并存仍通过。
2. 应用装配专项：
   - `go test ./internal/app -run ASRProvider -count=10`：通过。
   - 默认 disabled 与替换 ready provider 均经真实基础设施装配验证。
3. 前端专项：
   - `pnpm vitest run src/features/analysis`：2 文件、8 测试通过。
   - 覆盖 strict 状态、未知私有字段、不一致状态、disabled 文案、状态失败仍保留基础报告和 ready 隐藏提示。
4. 全量门禁：
   - `go test ./...`：通过。
   - `go vet ./...`：通过。
   - `go build ./...`：通过。
   - `pnpm typecheck`：通过。
   - `pnpm vitest run`：10 文件、32 测试通过。
   - `pnpm build`：通过。
   - 从 `cmd/desktop` 执行 `wails build -clean -platform windows/amd64`：Wails 2.13.0 production 构建通过，产物为 `cmd/desktop/build/bin/douyin-live-desktop.exe`。
   - Wails 生成绑定的语义差异只新增 `ASRStatusDTO/GetASRStatus`；按既有门禁清理生成器空白噪声后 `git diff --check` 通过。

## 隐私与失败边界

- provider ID 仅接受 1–64 位小写字母、数字、点、下划线或连字符；初始化时拒绝危险/typed-nil provider，运行时 ID 漂移则映射为脱敏 unavailable。
- provider Validate 的任意未知错误只产生 `ASR_PROVIDER_UNAVAILABLE`，不回传错误文本。
- `AudioInput.AudioPath` 与 `SourceAudioSHA256` 标记为不可 JSON 序列化；前端合同拒绝未知字段。
- ASR 状态查询失败、未配置或 unavailable 都不阻塞 `AnalyzeSession`、已有报告读取或基础分析 UI。

## 未运行与后续

- `go test -race ./...` 未运行：当前 OpenSSH 环境 `CGO_ENABLED=0` 且 `where gcc` 无结果，竞态工具链不可用；未将其误记为源码失败。
- 本任务没有实现真实转写、缓存、重试或 transcript 持久化，也不需要云凭据；这些能力只有在后续选择具体 provider 后才能验证。
- 本轮未执行真实 GUI 视觉验收；disabled/unavailable/ready 可见行为由 React 组件测试锁定，完整回放、分析、ASR 降级与导出视觉验收保留给 P4-ACC-001。

## 结论

P4-ASR-001 的可替换接口、默认禁用、稳定降级、基础分析独立性和隐私边界均有自动化证据，可标记为 `DONE`。下一任务为 P4-EXP-001。
