# P4-EXP-001 分析报告导出验证记录

- 日期：2026-07-22
- 权威工作副本：`GJS-20250801EFK:D:\douyinLive`
- 基线：`main@3ed36e490306ee9c9854c0e2a20d2d1e1c7ea768`
- 任务：版本化 CSV/JSON 报告导出、稳定 manifest、安全写入与隐私门禁

## 交付合同

- `analysis-export/v1` JSON manifest 固定 version/schema、UTC 毫秒时间、场次毫秒偏移、脱敏场次信息、报告摘要/候选、隐私声明和 CSV 文件证据。
- 固定导出 `events.csv`、`metric-buckets.csv`、`transcripts.csv`、`media-segments.csv` 与 `manifest.json`；四个 CSV 均含 UTF-8 BOM，列序与行排序稳定。
- 每个文件返回固定名称、媒体类型、数据行数、字节数和小写 SHA-256；manifest 不包含随机 export ID 或目录名，同一数据库快照和生成时间可重复生成相同字节。
- Wails 返回值只含 version、UUIDv7 export ID、不透明目录名、UTC 生成时间、正文选择和五文件元数据；绝对路径不越过 Go 边界。

## 文件安全

- 导出根由 `storage.Layout.ExportsDir` 注入，客户端不能传任意路径。
- 导出前拒绝缺失、非目录或符号链接根；Go 1.26 `os.OpenRoot` 持有根句柄并限制所有后续相对访问不能逃逸。
- 每次导出创建不可预测 UUIDv7 临时目录，文件以 `O_CREATE|O_EXCL` 和 `0600` 创建，写完执行 `Sync`、关闭并复核 regular file、大小和摘要。
- 四个 CSV 与 manifest 全部完成后，临时目录才在同一根内原子重命名为最终唯一目录；取消、查询、写入或发布失败均删除未发布目录。
- 未知底层错误统一折叠为 `analysis export failed`，不会通过 Wails 泄露文件路径或注入错误详情。

## 隐私与内容选择

- 默认 `includeText=false`：弹幕正文、转写正文和 speaker 留空；昵称永不进入导出查询。
- user 标识只使用数据库已有的安装级 HMAC-SHA256；原始平台房间/消息 ID、operation、data/raw/media 路径、dedupe、normalized/raw payload、媒体 SHA-256、source audio SHA-256 不进入导出列或 manifest。
- Cookie、流 URL、签名不在导出查询图中；manifest 固定声明排除范围。
- 用户显式选择正文时保留弹幕、转写和 speaker；去除前导空白后以 `= + - @` 开头的 CSV 文本前置单引号，防止 Excel/表格软件执行公式。
- 回归 fixture 同时植入平台 ID、昵称、路径、normalized/raw 引用、媒体/音频摘要和公式载荷；默认包逐文件扫描均未发现这些值，显式正文包只出现带保护前缀的正文。

## UI 与合同

- 分析页在报告下方展示导出范围，正文复选框默认不选中；导出期间禁用按钮并显示忙状态。
- 成功只展示应用导出目录下的不透明目录名与文件数；失败使用现有用户错误边界。
- `analysisExportResultSchema` 为 strict v1：拒绝绝对路径、未知字段、未知文件、重复/重排文件、媒体类型漂移、非法 UTC、大小或 SHA-256。

## 验证证据

- `go test ./internal/analysis -run Export -count=20`：通过。
- `go test ./cmd/desktop -run Analysis -count=10`：通过。
- `go test ./internal/app -run Analysis -count=10`：通过。
- `go vet ./internal/analysis ./internal/app ./cmd/desktop`：通过。
- `go build ./internal/analysis ./internal/app ./cmd/desktop`：通过。
- `go test ./...`：通过。
- `go vet ./...`：通过。
- `go build ./...`：通过。
- `pnpm typecheck`：通过。
- `pnpm test`：10 个测试文件、35 项测试通过。
- `pnpm build`：通过。
- `wails build -clean -platform windows/amd64`：通过。
- `git diff --check`：通过；Wails 生成的 `models.ts` 仅保留新增导出模型语义，生成器空白噪声已清除。

## 未运行与后续

- 当前 OpenSSH 环境仍为 `CGO_ENABLED=0` 且 `where gcc` 无结果，`go test -race ./...` 会在源码测试前因 `-race requires cgo` 停止；未把它记为源码失败，也未安装或更改工具链。
- P4-EXP-001 完成后，PHASE-4 为 18/20 点（90%），项目总进度为 88%。
- 下一任务 P4-ACC-001：以真实/隔离 fixture 完成回放时间误差、报告重复、ASR disabled/unavailable 降级、导出包打开与 GUI 可见性的一体化验收。
