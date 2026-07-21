# P4-PLY-001 历史同步回放基础验证记录

日期：2026-07-21

权威工作副本：`GJS-20250801EFK:D:\douyinLive`

任务：`P4-PLY-001`

## 完成范围

- Schema v6 为历史场次、事件、缺口和版本化指标桶提供稳定索引与主键。
- `internal/playback` 提供只读 session/event/gap/media keyset 查询、严格 cursor、隐私 allowlist DTO 和统一时间轴定位。
- Wails 门面暴露查询与定位，生成 TypeScript 绑定包含完整 Playback v1 合同。
- React 历史场次页面支持状态筛选、分页列表、详情、媒体/事件/缺口分页、同步互动和可点击/键盘定位的统一时间轴。
- 动态媒体端点支持 `GET`、`HEAD` 与标准 Range 语义；端点只接受 opaque artifact UUIDv7，不接受路径或查询参数。

## 动态媒体安全门

每次请求都重新执行以下检查，不信任前端 DTO：

1. SQLite 联查 artifact、segment、session media 与 recording root，要求 complete/recovered、H.264 MP4、来源摘要与 segment 摘要一致。
2. 内部 root 使用应用 data root；外部 root 还要求 durable `ready`，并在禁止重命名的目录句柄持有期间复核 marker root ID、规范路径摘要与卷身份。
3. 相对路径仅接受受限 ASCII 组件，拒绝绝对路径、反斜杠、盘符、控制字符、空组件和 `.`/`..`。
4. 目标必须是 containment 内、非 symlink/reparse 的常规文件；打开前后身份一致，句柄最终路径等于期望路径。
5. 当前大小和 SHA-256 必须等于 durable evidence；哈希前后身份、大小和修改时间不变，同路径重开仍是同一文件。
6. Windows 文件句柄禁止 `FILE_SHARE_WRITE` 与 `FILE_SHARE_DELETE`，通过验证后直到 Range 响应结束都不能并发修改或替换。
7. HTTP 错误只返回 404/405/409 通用状态，不回显路径、root、摘要或底层错误。

## 验证结果

- `go test -count=20 ./internal/playback`：通过，包括篡改、路径穿越、非播放 artifact、并发写/替换冻结。
- `go test -run TestSQLiteRepositoryRegisterRecordingRootPersistsMarkerAndRow -count=20 ./internal/capture`：通过，包括只读外部 root 正反身份复核。
- `go test -count=10 ./internal/playback ./cmd/desktop`：通过，包括 HTTP Range 206、Content-Range、关闭句柄和错误脱敏。
- `go test ./...`：通过。
- `go vet ./...`：通过。
- `go build ./...`：通过。
- `pnpm typecheck`：通过。
- `pnpm test`：8 个文件、23 项测试通过。
- `pnpm build`：通过，Vite production bundle 成功。
- `wails build -platform windows/amd64`（从 `cmd/desktop`）：production build 通过。
- `git diff --check`：通过；Wails 生成的 Playback 类型被保留，纯制表空白噪声已机械清理。

当前 OpenSSH Go 环境仍为 `CGO_ENABLED=0` 且无 GCC，因此未运行 `go test -race`；这不是源码测试失败。真实跨分片播放、时间轴 P95 误差和完整 GUI 视觉验收保留给 `P4-ACC-001`，本任务不冒充这些尚未执行的终验。

## 结论

`P4-PLY-001` 的查询、同步交互和安全媒体读取基础已完成，可以进入 `P4-ANA-001`。按阶段任务点计算，PHASE-4 完成 6/20 点（30%），总体完成度为 76%。
