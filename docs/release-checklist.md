# Windows 可运行交付清单

本清单用于 `0.1.x` Windows x64 直接/内部交付。目标是安装包和便携程序在受支持的 Windows x64 环境中可运行，不要求应用商店上架、商业代码签名或公开发行资质。任一“必需”项缺少机器证据时，状态必须为 `BLOCKED`；标为“非阻塞增强”的项目不影响 `P5-ACC-001` 完成。

当前验收已由 P4/P5 的真实 GUI、60 分钟稳定性、可复现构建与安装矩阵覆盖；未签名包可能触发 Windows SmartScreen 提示，使用者必须从可信渠道取得文件并核对 SHA-256。

## 1. 版本与源代码

- [x] 待交付 commit 已固定，构建时工作树为 clean；只有启用公开发行时才额外要求受保护的 `v<semver>` tag。
- [x] `frontend/package.json`、`cmd/desktop/wails.json` 和构建参数版本完全一致。
- [x] Go 全量 test/vet/build、前端 typecheck/Vitest/build、Wails production build 和安装矩阵通过。
- [x] 60 分钟稳定性与网络、FFmpeg、数据库忙、磁盘满、强制退出恢复证据仍绑定兼容代码基线。

## 2. 产物与供应链

- [x] 两次 clean production 构建的未签名 EXE hash/size 一致。
- [x] 发布目录包含桌面 EXE、安装器、数据库回滚工具、`release-manifest.json`、SPDX SBOM、许可证清单、第三方 notices、FFmpeg lock、敏感扫描和全部用户文档。
- [x] release manifest 的每个路径、大小和 SHA-256 与最终文件复算一致；无额外未登记文件。
- [x] FFmpeg/ffprobe 与锁文件版本和 SHA-256 一致；敏感扫描为零命中。

## 3. 非阻塞增强：签名与病毒扫描

- [ ] （非阻塞增强）使用受控 Windows 代码签名证书签署桌面 EXE、回滚工具和安装器，并记录 RFC 3161 时间戳及签名前后 SHA-256。
- [ ] （非阻塞增强）对最终目录执行可复核病毒扫描，报告绑定 manifest SHA-256。
- [x] 未签名直接交付必须保留 exact manifest、逐文件 SHA-256、敏感信息零命中扫描和来源说明。

## 4. Windows 与安装矩阵

- [x] Windows 11 x64：fresh install、覆盖升级、默认卸载保留数据、双确认删除、WebView2 缺失和启动冒烟通过。
- [ ] （非阻塞兼容性扩展）Windows 10 x64：同一安装包完成 fresh install、启动、升级保留数据、卸载保留数据和 WebView2 场景。
- [x] 数据库 v5→v6→v5 备份/迁移/回滚通过；外部路径、活动 sidecar、损坏和较新 schema 反例失败关闭。
- [x] 安装后主程序、回滚工具、FFmpeg/ffprobe 和用户/许可证文档齐全，卸载后程序/快捷方式/注册表零残留。

## 5. 人工验收与文档

- [x] 已引用绑定兼容代码基线的真实 GUI 9/9 证据，覆盖添加房间、监控、实时状态、回放、分析、ASR disabled、隐私导出、诊断和正常退出。
- [x] 真实 Wails/WebView2 PrintWindow 视觉证据、运行 PID/版本和截图 hash 已记录；额外 DPI 组合按界面变更时复验。
- [x] `USER-GUIDE.md`、`PRIVACY.md`、`KNOWN-LIMITATIONS.md`、`INSTALLATION.md` 与交付版本一致。
- [x] 已知限制有明确说明；无未关闭 P0/P1。

## 6. 发布与回滚

- [x] 直接/内部交付前再次验证文件清单和 SHA-256；公开上传仍使用独立严格门禁，不覆盖上一稳定版。
- [x] 交付说明列出版本、commit、安装/升级步骤、隐私边界、已知限制、校验和和回滚入口。
- [x] 数据库兼容说明、升级前备份和离线回滚路径齐全。

发布负责人在所有直接/内部可运行交付的必需项通过后即可把 `P5-ACC-001` 和项目状态改为 `DONE/100%`；非阻塞增强项以后可按需要补充。
