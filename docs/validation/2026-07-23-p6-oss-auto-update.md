# P6-UPD-001 OSS 自动更新实现与验证记录

- 日期：2026-07-23
- 权威工作副本：`GJS-20250801EFK:D:\douyinLive`
- 任务状态：实现与基础设施、0.2.0 人工引导、0.2.2 canary/stable 发布及 0.2.0→0.2.2 真实自动升级已完成；剩余受控故障矩阵和 24 小时观察仍为 `IN_PROGRESS`
- 凭据边界：本文及仓库不记录 AccessKey、RAM 密钥、Ed25519 私钥或解密后的 DPAPI 数据

## 1. OSS 与发布身份

创建杭州标准存储私有 Bucket：

```text
douyinlive-updates-cn-hangzhou-1e8d9993065b
```

已验证配置：

- Bucket ACL 为 private；
- Bucket 级阻止公共访问只对该专用 Bucket 关闭，Bucket Policy 仅允许匿名 HTTPS `GetObject` 到 `channels/*` 与 `releases/*`；
- HTTP 明确 Deny；匿名 List/Put 和 `private/*` 均拒绝；
- Versioning 为 Enabled，默认 SSE 为 AES256；
- `channels/*` 非当前版本保留 90 天，未完成 multipart upload 7 天清理；
- CORS 未配置，静态网站未启用。

RAM 用户 `douyinlive-update-publisher` 仅附加自定义策略 `DouyinLiveOssUpdatePublisher`，允许指定 Bucket 的 `channels/*`、`releases/*` 执行 GetObject/GetObjectVersion/PutObject。未授予 DeleteObject、DeleteBucket、ListBuckets、Bucket Policy/ACL/Versioning/Lifecycle 等权限。

发布 RAM 凭据保存于 Windows 仓库外的 DPAPI LocalMachine 文件；签名私钥使用独立 DPAPI 文件。两者所在目录和文件均启用受保护 ACL，owner 为当前用户，允许主体严格为当前用户与 SYSTEM。

## 2. 更新协议与客户端

- `internal/update` 实现 Ed25519 单对象签名信封，签名覆盖 Base64 payload 的原始字节；验签后才严格解析 payload。
- payload 固定产品、通道、SemVer、UTC 发布时间、Git commit、数据库 schema、平台、纯文本说明、安装器与发布清单的 object key/size/SHA-256。
- 客户端固定公钥、`windows/amd64`、OSS HTTPS Origin 和对象前缀，持久化 `highestSeenVersion`；拒绝未知/重复字段、尾随 JSON、重定向、跨 Origin、超限响应、同版/降级和回放。
- manifest 上限 96 KiB、安装器上限 512 MiB；下载使用 `.part`、ETag 与 Range，完成后复算完整 SHA-256，不一致立即删除。
- 设置 schema v3 新增默认开启的 `automaticUpdates`。关闭后不启动周期请求，手动检查仍可用。
- Wails 暴露 `GetUpdateStatus`、`CheckForUpdate`、`PrepareUpdate`、`CancelUpdateDownload`、`InstallPreparedUpdate` 与 `update:status`；v1 DTO 不包含本地路径、OSS URL、签名或凭据。
- 启动成功 30 秒后首次检查，之后每 6 小时加随机抖动。STARTING/LIVE/RECORDING/RECONNECTING/FINALIZING 阻止下载和安装。

## 3. 独立更新助手与恢复

`douyin-live-updater.exe` 执行：

1. 严格读取签名安装作业并重新验证 Ed25519 信封、对象描述符和安装器 hash；
2. 有界等待父进程正常退出；
3. 把当前程序目录同卷改名为唯一备份，再运行 NSIS 静默安装；
4. 校验安装器退出码、卸载注册表版本与目标 EXE；
5. 启动新应用并等待匹配随机 nonce 的健康标记；标记仅在基础设施初始化成功后写入；
6. 失败时恢复旧程序目录；若数据库 schema 已变化，调用现有严格离线回滚恢复精确升级前备份；
7. 数据库回滚失败时拒绝启动旧版本，保留程序/数据库备份、结果文件和稳定错误码。

安装前桌面应用重新核对所有直播间状态、创建 SQLite 一致性备份、检查系统盘与数据盘空间，并把助手复制到安装目录外。应用关闭时取消更新服务，旧助手副本按精确路径有界清理。

## 4. 自动化证据

在 Windows 权威工作副本执行并通过：

```text
where go
go test ./...
go vet ./...
go build ./...
pnpm --dir frontend test
pnpm --dir frontend typecheck
pnpm --dir frontend build
cd cmd\desktop && wails build -clean -platform windows/amd64
go run .\cmd\releasebuilder -allow-dirty -version 0.2.0 ...
powershell -File scripts/test-windows-installer.ps1 -ReleaseDirectory release-p6-validation/v0.2.0 -CurrentVersion 0.2.0
git diff --check
```

结果：

- Go 全量测试、vet、build 通过；
- 前端 10 个测试文件、37 项测试通过，typecheck 与 production build 通过；
- Wails production Windows/amd64 构建通过；
- 0.2.0 脏树候选发布门禁通过：251 个组件、408 个扫描文件，主程序、数据库回滚器、更新助手、安装器齐全；
- 提交 `54172c638bf43f33a481a8319cae81e66bf57218` 创建精确 tag `v0.2.0`，正式发布门禁为 `dirty=false`、251 个组件、436 个扫描文件；
- 正式主程序 SHA-256 `a55aaa53bae5dd4fb88d47db954688d891a843370468e7acad2991cb42647b22`，安装器 98,399,776 字节、SHA-256 `891b0afefd6ae597332be4568151c269d1fc3dbdd0c7e791e3134f931c37167b`；
- 正式 Ed25519 canary 信封已生成并完成本地自验，但 `published=false`，没有提前切换 OSS 通道；
- NSIS 隔离安装矩阵 7/7：fresh、原位升级、默认保留数据、删除二次确认、确认清理、WebView2 自动安装成功与失败回滚。

## 5. OSS 实网证据

使用一次性无敏感内容对象完成并清理：

- `channels/*` 和 `releases/*` 匿名 HTTPS GetObject 为 200，回读 SHA-256 与上传内容一致；
- HTTP GetObject、匿名 List、匿名 Put、`private/*` GetObject、CORS preflight 均为 403；
- 同一通道 key 连续上传 revision 1/2 后存在两个 VersionId；管理员按旧 VersionId 下载与 revision 1 字节一致；
- 验收结束后精确删除全部验收对象版本，三个验收前缀剩余 Version/DeleteMarker 总数为 0。

## 6. 0.2.0 人工引导与通道复核

- 用户提供的首次启动截图显示：左下角版本 `0.2.0`、桌面服务已连接、本地存储正常、SQLite Schema v6，P6-UPD-002 据此完成；
- 后续静态复核发现正式 0.2.0 的客户端固定读取 `channels/stable.json`，其已安装更新助手也固定按 stable 验证签名载荷，因此无法直接消费 canary 信封；
- 签名 payload 明确包含 channel，canary 信封不能逐字节提升为 stable。0.2.1 修复为：管理员机器策略选择 canary、安装作业携带并按签名 channel 验证、不可变安装器/发布清单跨通道复用、通道信封分别保存；
- 在真实 OSS 发布 canary 后，触及 stable 指针或替换用户当前正式安装前，必须记录一次性 0.2.0 引导决策；不得把仅下载 canary 冒充真实自动安装。

0.2.1 正式结果：

- 源提交/tag：`a176be66e217f48fd0aab7620d1ef0695bae36eb` / `v0.2.1`；
- Go 全量 `test/vet/build`、前端 10 文件 37 项测试/typecheck/build、production Wails 通过；
- 正式发布门禁：`dirty=false`、251 个组件、439 个扫描文件；
- 安装器矩阵：7/7；安装器大小 98,400,858 字节，SHA-256 `c9e43663cd086dc306971ed4c73008c9640a2adf4073b31be8db13207f367b8e`；
- canary 发布器通过进程级代理完成上传；不可变安装器、发布清单、`releases/v0.2.1/update-canary.json` 与 `channels/canary.json` 均经匿名 HTTPS 回读；
- `channels/canary.json` 与本地签名信封 SHA-256 均为 `e8bf381972355f478936b68c3610dbd32e1d8b25a49a7a1eb3d38784b0d1b684`，大小 1,424 字节；
- 实网复查：canary 200、版本化信封 200、安装器 Range 206/1 字节、HTTP 403、匿名 List 403，`channels/stable.json` 保持 404；
- 首次发布直连探测为 000，本机 Clash/Mihomo 代理为 404；仅为发布进程设置 `HTTP_PROXY`/`HTTPS_PROXY` 后成功，没有修改持久 Git、系统或应用代理。

0.2.1 stable 与引导修复：

- 用户授权一次性 stable 引导后，`releases/v0.2.1/update-stable.json` 与 `channels/stable.json` 签发成功，二者 SHA-256 均为 `0c51750abf903444124469273e8697608e3aad4297ef533898a96885f559669b`；stable/canary 的安装器和发布清单描述完全一致；
- 安装前只读检查发现实际卸载键是 `HKCU\...\Uninstall\DouyinLiveDesktop`，0.2.0 助手却硬编码 `DouyinLiveDouyinLiveDesktop`。直接安装会在新安装器成功后误报 `UPDATE_REGISTRY_NOT_CONVERGED` 并回滚，因此未触发客户端安装；
- 0.2.1 版本化对象保持不可变。0.2.2 安装器将额外写入仅含版本/安装位置的旧助手兼容键并在卸载时清理；0.2.2 助手改为校验正式键，后续版本不再依赖兼容键。

0.2.2 正式发布与真实升级：

- 源提交/tag：`4e51f521d19b74a2d715ff867ee9cf0382e9ea9a` / `v0.2.2`；正式发布门禁为 `dirty=false`、251 个组件、439 个扫描文件，正式 NSIS 安装矩阵 7/7；
- 安装器 98,400,939 字节，SHA-256 `a1d39cdde96a9dd164dfb44af6802a9ea228a1138a124244c6e129caae2a2d3e`；主 EXE SHA-256 `371a6cd050a04a66fec6f3168def848bdb43ecbfcbd2029c94ecb9f78b2d6b60`；
- canary 信封 SHA-256 `159f2bb16d1b4a6d06094da5d2012da0924c1d8fb81902e8a4a757d7caca0b96`，stable 信封 SHA-256 `4b00ec602cfa99bc7e8ecd67675a8370c1c1c90fb9476215e031e2a0a3b47720`；两者载荷通道不同但安装器与发布清单 object key/size/hash 完全一致；
- 匿名实网复验：安装器 Range 为 206/1 字节，HTTP 为 403，匿名 List 为 403；
- 当前主机的 Go 进程不自动继承 WinINET/Clash，仅给真实验收应用进程临时设置 `HTTP_PROXY`/`HTTPS_PROXY` 与本机 `NO_PROXY`，未修改持久环境；0.2.0 随后自动检查、下载并显示“版本 0.2.2 已准备好”；
- 第一次安装由验收夹具错误地把应用工作目录设为安装目录，助手安全返回 `UPDATE_PROGRAM_BACKUP_FAILED`，0.2.0 和数据库备份均保留。改用桌面工作目录后重试成功，安装结果为 `success=true`；
- 安装后正式键与兼容键的版本/位置均为 0.2.2/正式安装目录；已安装 EXE hash 与正式发布物一致；新应用健康启动，界面显示 0.2.2、本地存储正常、SQLite Schema v6；程序目录更新备份为 0，更新助手进程为 0。

## 7. 已知限制与剩余发布门

OSS 在 Bucket Versioning Enabled/Suspended 时忽略 `x-oss-forbid-overwrite`。当前发布工具对通道专属版本信封执行 HeadObject 并拒绝已存在 key；跨通道提升只复用匿名回读 hash 完全一致的安装器与发布清单，配合单一最小权限发布身份和版本历史实现可审计保护；这不是存储层不可绕过的 WORM。

以下证据不得提前宣称完成：

- 活动录制拒绝、断网续传、坏签名/坏安装包与安装失败自动恢复的受控验收；
- 权威 Windows 主机从 2026-07-23 14:42 +08:00 起连续 24 小时稳定观察。
