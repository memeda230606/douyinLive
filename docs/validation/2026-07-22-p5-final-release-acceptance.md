# P5-ACC-001 最终发布验收记录

- 日期：2026-07-22
- 版本：0.1.0 开发候选
- 权威工作区：`GJS-20250801EFK:D:\douyinLive`
- 基线 commit：`5a8867fd6b78add39b15a29d46d1947f9ec98f76`
- 结论：`BLOCKED`；可在当前主机完成的发布文档、候选构建、安装矩阵和失败关闭审计已完成，但没有外部代码签名证书、可信双引擎病毒扫描报告和 Windows 10 x64 实机证据，不能把 P5-ACC-001 或项目标记为完成。

## 本次交付

- 新增 `USER-GUIDE.md`、`PRIVACY.md`、`KNOWN-LIMITATIONS.md` 和 `RELEASE-CHECKLIST.md` 的版本化源文档。
- 发布构建器把四份用户文档加入 release manifest；NSIS 安装器把它们与许可证材料一同安装。
- 安装矩阵严格要求并复核新增文档，避免只存在于源码而缺失于发布包。
- 新增 `scripts/test-p5-release-acceptance.ps1`：复核版本、commit、clean/reproducible 状态、平台、敏感扫描、必需文件、manifest 逐文件大小/SHA-256、三个 EXE 的 Authenticode 与时间戳、双引擎病毒扫描证据和 Windows 10 证据，并原子生成 `P5-ACC-001/v1` 报告。
- GitHub Windows 发布作业在上传任何桌面 evidence 或 Release 附件前，强制三个 EXE 的有效签名/时间戳和启用状态下的 Microsoft Defender 扫描；当前尚未配置受控签名阶段，因此正式标签流程按预期失败关闭。

## 当前主机与外部门禁事实

| 门禁 | 实测 | 结论 |
| --- | --- | --- |
| Windows | NT 10.0 build 22631，原生 x64（Windows 11 23H2 build） | Windows 11 证据可用；不是 Windows 10 实机 |
| `signtool.exe` | OpenSSH 会话 `Get-Command` 数量 0 | 不可签名 |
| 当前用户代码签名证书 | 可用、未过期且含私钥的证书数量 0 | 不可签名 |
| 候选 EXE | 桌面、回滚工具、安装器均为 `NotSigned` | 不是正式发布候选 |
| Microsoft Defender | 服务、杀毒与实时防护均为 disabled；`MpCmdRun -DisableRemediation -ReturnHR` 返回 `0x80004005` | 扫描未运行成功，不得写成通过 |
| Security Center | 同时登记腾讯电脑管家系统防护与 Windows Defender；仅取得产品状态，没有可绑定 manifest hash 的扫描原始报告 | 不构成病毒扫描证据 |
| Windows 10 | 无独立 x64 主机或 VM 结果 | 缺失，阻塞 |

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

机器审计返回预期阻塞：

```text
P5_RELEASE_ACCEPTANCE_BLOCKED
blockers=BUILD_NOT_CLEAN_REPRODUCIBLE,CODE_SIGNING_MISSING,ANTIVIRUS_EVIDENCE_MISSING,WINDOWS10_EVIDENCE_MISSING
```

该结果证明脚本没有把 dirty、未签名、未扫描或缺少 Windows 10 的包误判为通过。提交后可重新生成 clean unsigned 候选消除第一项，但后三项需要外部资源。

## 已继承的发布证据

- P5-ENG-001：可复现构建、完整身份、锁定 FFmpeg、250 组件 SBOM/许可证、敏感扫描和 CSP 已通过。
- P5-WIN-001：当前用户安装、升级、默认卸载保留、双确认清理、WebView2 缺失和数据库回滚矩阵已通过。
- P5-STB-001：真实 3600 秒、61 分钟样本、多房间、资源阈值及数据库忙、网络、磁盘满、强制退出恢复已通过。
- P4-ACC-001：同一次真实 Wails/WebView2 冷启动的回放、分析、ASR disabled、隐私导出与 PrintWindow 视觉 9/9 已通过。

这些证据不替代最终签名包上的 Windows 10、病毒扫描和人工发布复核。

## 解阻条件

1. 在受控签名环境提供有效 Windows 代码签名证书和 `signtool`，先签桌面 EXE/回滚工具，再重建并签安装器，验证 RFC 3161 时间戳和安装器内嵌签名。
2. 对最终签名目录执行 Microsoft Defender 与第二引擎扫描，保存绑定最终 `release-manifest.json` SHA-256 的机器报告。
3. 用同一最终签名安装包在 Windows 10 x64 完成 fresh install、启动、覆盖升级、数据保留卸载和 WebView2 场景，生成 `douyinlive-windows10-evidence/v1`。
4. 在最终签名包上执行发布清单中的人工功能/视觉验收；全部通过后才能把阶段和项目更新为 100%。

当前没有 P0/P1 源码缺陷证据；阻塞属于未提供的外部发布资质与环境。OpenSSH 仍为 `CGO_ENABLED=0` 且无 GCC，race 未启动，此项保持既有工具链限制记录。
