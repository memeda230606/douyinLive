# Windows 正式发布清单

本清单用于 `0.1.x` Windows x64 正式发布。任一必需项缺少机器证据时，状态必须为 `BLOCKED`，不得把未运行、人工推测或开发候选结果写成通过。

## 1. 版本与源代码

- [ ] 受保护的 `v<semver>` tag 精确指向待发布 commit，工作树为 clean。
- [ ] `frontend/package.json`、`cmd/desktop/wails.json`、tag 和构建参数版本完全一致。
- [ ] Go 全量 test/vet/build、前端 typecheck/Vitest/build、Wails production build 和安装矩阵通过。
- [ ] 60 分钟稳定性与网络、FFmpeg、数据库忙、磁盘满、强制退出恢复证据仍绑定兼容代码基线。

## 2. 产物与供应链

- [ ] 两次 clean production 构建的未签名 EXE hash/size 一致。
- [ ] 发布目录包含桌面 EXE、安装器、数据库回滚工具、`release-manifest.json`、SPDX SBOM、许可证清单、第三方 notices、FFmpeg lock、敏感扫描和全部用户文档。
- [ ] release manifest 的每个路径、大小和 SHA-256 与最终文件复算一致；无额外未登记文件。
- [ ] FFmpeg/ffprobe 与锁文件版本和 SHA-256 一致；敏感扫描为零命中。

## 3. 签名与病毒扫描

- [ ] 使用受控 Windows 代码签名证书签署桌面 EXE、回滚工具和安装器；安装器内嵌的两个 EXE 也必须是已签名版本。
- [ ] `Get-AuthenticodeSignature` 和 `signtool verify /pa` 均为有效，RFC 3161 时间戳存在；记录签名前后 SHA-256。
- [ ] 对最终签名后的三个 EXE 和完整发布目录执行受控病毒扫描；报告记录产品/引擎/特征库版本、扫描 UTC、目标 manifest SHA-256、发现数和结果。
- [ ] 至少 Windows Defender 与发布负责人指定的第二引擎通过；扫描器停用、退出码不可信或没有原始报告均为阻塞。

## 4. Windows 与安装矩阵

- [ ] Windows 11 x64：fresh install、覆盖升级、默认卸载保留数据、双确认删除、WebView2 缺失和启动冒烟通过。
- [ ] Windows 10 x64：同一最终签名安装包完成 fresh install、启动、升级保留数据、卸载保留数据和 WebView2 场景。
- [ ] 数据库 v5→v6→v5 备份/迁移/回滚通过；外部路径、活动 sidecar、损坏和较新 schema 反例失败关闭。
- [ ] 安装后主程序、回滚工具、FFmpeg/ffprobe 和用户/许可证文档齐全，卸载后程序/快捷方式/注册表零残留。

## 5. 人工验收与文档

- [ ] 在最终签名包上人工完成添加房间、启停监控、实时页面、历史回放、基础分析、ASR disabled 降级、隐私导出、诊断和正常退出。
- [ ] 视觉复核覆盖 100%/150% DPI、最小窗口与主要空/错/加载状态；记录截图 hash 和运行 PID/版本。
- [ ] `USER-GUIDE.md`、`PRIVACY.md`、`KNOWN-LIMITATIONS.md`、`INSTALLATION.md` 与发布说明版本一致。
- [ ] 已知 P2 缺陷有明确规避方式；无未关闭 P0/P1。

## 6. 发布与回滚

- [ ] 上传附件前再次验证签名、文件清单和 SHA-256；GitHub Release 只接收最终目录，不覆盖上一稳定版。
- [ ] 发布说明列出版本、commit、安装/升级步骤、隐私边界、已知限制、校验和和回滚入口。
- [ ] 保留上一稳定安装包、对应数据库兼容说明和升级前备份恢复路径。

发布负责人在所有必需项通过后才能把 `P5-ACC-001` 和项目状态改为 `DONE/100%`。
