# P5-ENG-001 发布工程验证记录

- 日期：2026-07-22
- 权威工作副本：`GJS-20250801EFK:D:\douyinLive`
- 验证基线：`main` / `e293817fcd9cae9e4859a70b182a3c6914e920ac` 加本任务待提交变更
- 结论：`DONE`

## 1. 交付内容

- `internal/buildinfo` 建立不可变构建身份，覆盖产品版本、完整 commit、构建时间与来源、Go/Wails/Node、FFmpeg 版本/hash/许可证，以及数据库、设置、分析和导出 schema。
- `cmd/releasebuilder` 与 `scripts/build-release.ps1` 实现严格 SemVer 对齐、默认清洁工作树、锁定工具验证、双次 production Wails 构建、EXE 可复现断言、产物 manifest、SPDX 2.3 SBOM、许可证清单、第三方 notices 与敏感扫描。
- `build/ffmpeg-windows-amd64.lock.json` 固定 Gyan FFmpeg 8.1.2 Essentials 官方归档、归档 SHA-256、ffmpeg/ffprobe SHA-256 和 `GPL-3.0-or-later`。
- 桌面 WebView 增加 CSP、`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY` 与 `Referrer-Policy: no-referrer`；诊断页以严格前端 schema 展示完整构建身份。
- `.github/workflows/release.yaml` 增加 Windows tag 构建、锁定 FFmpeg 下载校验、证据归档和 GitHub Release 附件上传。
- 删除无许可证的直接 Tikhub Go 客户端依赖，以项目已有 HTTP 栈保留同一签名端点、Bearer 鉴权和查询参数，并补本地协议回归；错误不再包含远端响应正文。
- `doRequest.example.json` 改为脱敏 fixture 索引，历史响应中的私有展示数据与签名 URL 不再留在当前工作树。

## 2. 可复现构建与产物证据

在显式开发验证模式下执行：

```powershell
./scripts/build-release.ps1 -Version 0.1.0 -AllowDirty
```

发布构建器以同一 `SOURCE_DATE_EPOCH`、`-trimpath`、空 Go build ID 和同一版本注入连续执行两次 `wails build -clean -platform windows/amd64`。两次 EXE 的身份完全一致：

- 大小：`48,669,184` 字节
- SHA-256：`b3dcb7eb25c994b0cc6de28402fa5849db5670ae93529fa52f1ba0beedc4db16`
- 组件数：`250`
- 跟踪文本扫描数：`379`
- 敏感命中：`0`
- 产物文件：EXE、`release-manifest.json`、`sbom.spdx.json`、`licenses.json`、`THIRD-PARTY-NOTICES.txt`、FFmpeg lock、项目 LICENSE 与扫描报告

该次验证显式允许待提交工作树，因此 manifest 的 `dirty=true`，仅用于证明实现与可复现性；正式 tag/CI 不传 `-AllowDirty`，脏工作树会在构建前失败。

许可证库存中有 1 个间接模块的上游包未携带许可证声明，按 SPDX 记录为 `NOASSERTION`；门禁没有猜测许可证，也没有从 SBOM 中删除该组件。

## 3. 自动化门禁

以下命令在 Windows 权威工作副本通过：

```text
go test ./...
go vet ./...
go build ./...
pnpm --dir frontend typecheck
pnpm --dir frontend test
pnpm --dir frontend build
powershell -NoProfile -NonInteractive -File scripts/build-release.ps1 -Version 0.1.0 -AllowDirty -VerifyOnly
powershell -NoProfile -NonInteractive -File scripts/build-release.ps1 -Version 0.1.0 -AllowDirty
```

前端结果为 10 个测试文件、36 项测试全部通过。发布门禁验证 FFmpeg 8.1.2、归档/二进制 lock、250 个实际组件和零敏感命中，并完成两次一致的 production Wails 构建。

全部新增文件暂存后再次以独立 `release-verify` 输出根运行 `-VerifyOnly`，实际扫描 389 个跟踪文本文件、敏感命中仍为 0；该临时输出在检查后删除。

`go test -race ./...` 未启动：当前 OpenSSH 环境为 `CGO_ENABLED=0` 且没有 GCC，这是已知工具链限制，不是源码测试失败。

## 4. 后续边界

本任务交付的是可审计的便携 EXE 发布工程基线，不把未签名开发 EXE声明为正式安装包。Windows 安装、升级、卸载、WebView2 缺失、数据保留和数据库回滚矩阵由 `P5-WIN-001` 继续完成；60 分钟资源与故障稳定性门禁由 `P5-STB-001` 完成。
