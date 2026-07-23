# 录制目录首次引导与运行时切换验证

## 目标

- 全新安装首次启动时强制用户确认录制目录。
- 设置页提供 Windows 系统目录选择器，并继续允许手工输入绝对路径。
- 保存时创建目录并验证可写性；失败时不覆盖旧设置。
- 目录变化只影响之后启动的录制，不移动或中断活动场次。
- 已有 0.2.x 用户升级时沿用既有目录，不重复弹出首次引导。

## 实现

- 设置 schema 升至 v4，新增 `recordingDirectoryConfirmed`。
- 新建设置默认未确认；v1/v2/v3 迁移统一标记为已确认，保持升级兼容。
- 首次引导不可关闭，保存成功前持续阻挡主界面；可选择建议目录或其他本地目录。
- Wails 门面新增 `SelectRecordingDirectory`，只返回用户选择的本地路径，不返回凭据或 OSS 信息。
- 录制工厂在每次新建录制时读取最新设置；未确认或非法目录 fail closed，活动录制继续使用创建时的目录。

## 自动验证

Windows 权威工作副本 `D:\douyinLive`：

- `go test ./internal/settings ./internal/capture ./internal/app ./cmd/desktop`：通过。
- `go test ./...`：通过。
- `go vet ./...`：通过。
- `go build ./...`：通过。
- `pnpm test`：11 个测试文件、39 项测试全部通过。
- `pnpm typecheck`：通过。
- `pnpm build`：通过。
- `wails build -platform windows/amd64`：通过。
- `wails build -clean -platform windows/amd64`：通过。
- `git diff --check`：通过；生成绑定仅保留新方法与新字段的语义差异。
- `releasebuilder -verify-only -allow-dirty`：`RELEASE_GATE_PASSED`，251 个组件、439 个文件完成敏感信息与供应链门禁。

专项反例覆盖：

- 新设置保持未确认，首次保存后才变为已确认。
- v1 与 v3 设置迁移后保持已确认，且不改变自动更新原值。
- 未确认的新安装录制根提供器拒绝启动录制。
- 保存新目录后，录制根提供器无需重启即可解析到新路径。
- 目录选择器取消返回空结果；相对路径和系统调用失败返回稳定错误码。
- 首次引导测试证明系统选择结果进入表单、完整设置保存成功后对话框才消失。

## 剩余发布动作

本变更进入下一版安装包后，需要在隔离的全新用户数据根完成一次真实 Windows 首次启动截图，并在已有 v3 设置上完成一次升级不弹窗复验；这两项属于候选发布验收，不影响本次源码实现与构建完成结论。
