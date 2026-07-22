# P5-ACC-001 最终发布验收记录

- 日期：2026-07-22
- 版本：0.1.0 开发候选
- 权威工作区：`GJS-20250801EFK:D:\douyinLive`
- 范围修订基线 commit：`5c2b07140a8222721f2e100e6e64c0504aa1e572`
- 结论：`DONE`；用户明确目标是不发布到商店、只需直接/内部可运行。可复现构建、exact manifest、敏感扫描、安装矩阵、真实 GUI 和 60 分钟稳定性证据满足该范围；签名、双引擎扫描和 Windows 10 独立矩阵降为非阻塞增强，未冒充已完成。

## 本次交付

- 新增 `USER-GUIDE.md`、`PRIVACY.md`、`KNOWN-LIMITATIONS.md` 和 `RELEASE-CHECKLIST.md` 的版本化源文档。
- 发布构建器把四份用户文档加入 release manifest；NSIS 安装器把它们与许可证材料一同安装。
- 安装矩阵严格要求并复核新增文档，避免只存在于源码而缺失于发布包。
- `scripts/test-p5-release-acceptance.ps1` 升级为 `P5-ACC-001/v2`：`internal-runnable` 默认模式严格复核版本、commit、clean/reproducible、平台、敏感扫描、必需文件和 manifest 逐文件大小/SHA-256；签名、双引擎与 Windows 10 证据记录为 warning。显式 `public-signed` 模式仍将三项作为 blocker。
- 公开 GitHub Release 作业继续保留有效 Authenticode/时间戳与 Defender 严格门禁；当前直接/内部可运行交付不依赖该公开发布作业，也没有把未签名包冒充为公开签名发行。

## 当前主机与非阻塞增强事实

| 门禁 | 实测 | 结论 |
| --- | --- | --- |
| Windows | NT 10.0 build 22631，原生 x64（Windows 11 23H2 build） | 当前可运行与安装证据有效；Windows 10 为非阻塞兼容性扩展 |
| `signtool.exe` | OpenSSH 会话 `Get-Command` 数量 0 | 当前未签名，直接/内部运行允许 |
| 当前用户代码签名证书 | 可用、未过期且含私钥的证书数量 0 | 当前未签名，公开分发时建议补充 |
| 候选 EXE | 桌面、回滚工具、安装器均为 `NotSigned` | 可能触发 SmartScreen，必须核对 SHA-256 |
| Microsoft Defender | 服务、杀毒与实时防护均为 disabled；`MpCmdRun -DisableRemediation -ReturnHR` 返回 `0x80004005` | 扫描未运行成功，不宣称通过，但不阻塞当前范围 |
| Security Center | 同时登记腾讯电脑管家系统防护与 Windows Defender；仅取得产品状态，没有可绑定 manifest hash 的扫描原始报告 | 记录为非阻塞 warning |
| Windows 10 | 无独立 x64 主机或 VM 结果 | 记录为非阻塞 warning |

没有安装工具、启动 Defender、停用第三方杀毒、导入证书或更改系统/用户安全配置。

## 开发候选验证

在 dirty 工作树上显式使用 `-AllowDirty`，只用于验证新发布逻辑，不冒充正式 clean build：

```text
RELEASE_GATE_PASSED
commit=5a8867fd6b78add39b15a29d46d1947f9ec98f76
dirty=true
components=250
scannedFiles=406
artifactSHA256=e7446ab05dc09a4ba58cb0e6b9820104eebc912b880a48cdea8ddc13712fc95e
installerSHA256=7c9e8e63029bf4a9305fe05125e0c856b72167aa73fa6280dc83a5135030c4a3
rollbackToolSHA256=30d41617d24fe0fc2b673e1599a3b9d983b84b6a2c6660a9df104c2f1bfaca85
```

- 两次 production Wails 构建的桌面 EXE 一致；发布构建器成功生成 15 个文件并把四份用户文档登记到 manifest。
- NSIS `/INPUTCHARSET UTF8 /WX` 构建成功，安装矩阵 `fresh-install`、`in-place-upgrade`、`uninstall-preserves-data`、`purge-needs-second-confirmation`、`confirmed-purge`、`webview2-missing` 仍为 6/6。
- Wails 生成的 `models.ts` 只产生已知空白噪声，语义无变化并在构建后恢复。

初版面向公开正式发行的机器审计曾返回预期阻塞：

```text
P5_RELEASE_ACCEPTANCE_BLOCKED
blockers=BUILD_NOT_CLEAN_REPRODUCIBLE,CODE_SIGNING_MISSING,ANTIVIRUS_EVIDENCE_MISSING,WINDOWS10_EVIDENCE_MISSING
```

该结果证明 v1 没有把缺失证据冒充通过。范围修订后的 v2 在 `internal-runnable` 下仍严格阻塞 dirty/不可复现或 manifest/sensitive-scan 错误，同时把后三项输出为 warning；显式 `public-signed` 模式保持原失败关闭语义。

## 已继承的发布证据

- P5-ENG-001：可复现构建、完整身份、锁定 FFmpeg、250 组件 SBOM/许可证、敏感扫描和 CSP 已通过。
- P5-WIN-001：当前用户安装、升级、默认卸载保留、双确认清理、WebView2 缺失和数据库回滚矩阵已通过。
- P5-STB-001：真实 3600 秒、61 分钟样本、多房间、资源阈值及数据库忙、网络、磁盘满、强制退出恢复已通过。
- P4-ACC-001：同一次真实 Wails/WebView2 冷启动的回放、分析、ASR disabled、隐私导出与 PrintWindow 视觉 9/9 已通过。

这些证据直接覆盖“程序可安装、可启动、可运行并可恢复”的当前目标；不代表已获得商店、公开签名发行或 Windows 10 独立兼容认证。

## 后续非阻塞增强

1. 若未来公开分发，在受控签名环境签署桌面 EXE、回滚工具和安装器并验证可信时间戳。
2. 对最终目录执行双引擎扫描，保存绑定 `release-manifest.json` SHA-256 的机器报告。
3. 在 Windows 10 x64 补充安装、启动、升级、数据保留卸载和 WebView2 矩阵。
4. 对外发布前复核来源说明、SHA-256、SmartScreen 提示与用户沟通。

当前没有 P0/P1 源码缺陷证据，也没有当前“可直接/内部运行”范围内的阻塞。OpenSSH 仍为 `CGO_ENABLED=0` 且无 GCC，race 未启动，此项保持既有工具链限制记录。
