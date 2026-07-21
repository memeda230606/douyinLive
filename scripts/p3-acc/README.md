# P3-ACC 验收控制器

`scripts/p3-acc-controller.ps1` 是 P3-ACC 的外层 Windows 控制器。它只输出固定结构、隐私安全的 JSON 报告；直播地址只通过受保护的一次性文件交给交互式应用，不出现在命令行、日志或报告中。

## 职责边界

一次完整验收按以下顺序执行：

启动业务流程前，控制器先创建唯一的 `Global\DouyinLive.P3ACC.App.<nonce>` Job。受保护 DACL 只允许当前用户与 SYSTEM，受保护 SACL 只设置 Medium mandatory label 的 `NO_WRITE_UP`，确保 OpenSSH 高完整性控制器创建的 Job 仍可由 `RunLevel Limited` 的交互式 launcher 取得 `QUERY`、`ASSIGN_PROCESS` 和 `TERMINATE` 权限，同时不向 Low/AppContainer 降低写入边界。限额只启用 `KILL_ON_JOB_CLOSE`，控制器从计划任务启动前一直持有最终句柄。

交互式 PowerShell 安全读取并删除一次性 secret 后只启动 `p3acclauncher.exe`。launcher 先加入外层 Job，再以 suspended 方式创建 app；app 必须继承同一 Job，通过成员校验后才恢复。正式握手发布后，launcher 清理环境、关闭自身 Job 句柄并退出，控制器复核其精确身份已经消失后才接受启动成功。

1. 应用内 JavaScript 等待不少于 10 分钟的稳定资源窗口，然后调用无参数 Crash hook。
2. Go hook 只崩溃当前 session fence 对应的 FFmpeg，证明新 attempt、恢复和 gap；JavaScript 随后置位 network fault armed。
3. 外层控制器只终止本次启动的独立 relay，证明传输中断，再在相同动态端口恢复 relay。控制器不得再次终止 FFmpeg。
4. 应用证明消息与录制恢复、直播离线、finalization、manifest/checkpoint/SQLite 一致性及资源和 UI 延迟门槛。
5. 同用户交互式 helper 用 `PrintWindow(PW_RENDERFULLCONTENT)` 生成右侧安全状态裁剪图；独立的 macOS 协调端确认图像后回 ACK，helper 才发送 `WM_CLOSE`。
6. 控制器要求外层 Job 的 `BasicAccounting.ActiveProcesses` 连续两次为 0 后才关闭最终句柄，随后以只读 SQLite `PRAGMA quick_check;` 和独占文件打开证明数据库正确且已解锁。历史 PID/PPID 图只可用于诊断，不能授权结束进程。

任何不确定性都 fail closed。强制结束进程只属于失败清理路径：控制器调用 `TerminateJobObject` 并有界等待 `ActiveProcesses=0` 后才关闭句柄；Job 外且仍匹配 pre-identity 的 launcher 只允许按 PID+UTC start ticks 精确清理。该路径不能作为成功证据。

交互式 JavaScript 探针随受控应用持续运行，不得设置短于外层控制器的固定总寿命；外层控制器的有界等待与 Job 生命周期才是总超时边界。真实下播从 `RecordingReconnecting` 干净收尾时，严格终态为 session `completed` 与 recording `incomplete`：最后一次连接已离线但所有组件清理成功。session `interrupted` 表示组件清理错误，必须拒绝，不能为了兼容旧夹具放宽最终门禁。

项目计划中的人工观察或用户豁免永远不构成控制器成功分支。只有本文定义的完整机器合同才能输出 `P3-ACC-CONTROLLER/v1` 的 `passed=true`；文档可把某次未执行项记为 `USER_WAIVED/NOT_RUN`，但不得据此修改 hook、模块或 runner 的成功判定。

## 前置条件

- 从 `D:\douyinLive` 构建的带 `p3accacceptance` tag 桌面应用、`cmd\p3accproxy` relay 和 `cmd\p3acclauncher` launcher。launcher 的受控输出位置为 `D:\douyinLive\cmd\desktop\build\bin\p3acclauncher.exe`：

  ```powershell
  where.exe go
  go build -tags p3accacceptance -o cmd\desktop\build\bin\p3acclauncher.exe .\cmd\p3acclauncher
  ```
- `sqlite3.exe` 可由当前 SSH 会话的 `PATH` 找到。
- Windows 当前用户存在可用的交互式桌面会话。
- 已核验 Clash/Mihomo 的实际回环监听地址；不要盲信固定端口，也不要持久修改全局 Git 或系统代理。
- acceptance root 是非重解析点的全新目录，目录内只能有精确 sentinel `.p3acc-controller-owned`，内容必须是 UTF-8（无 BOM）的 `P3ACC-CONTROLLER/v1` 加一个 LF。
- 直播地址文件必须位于 acceptance root 外，禁止重解析点，大小为 1–8192 字节，owner 为当前用户，ACL 仅允许当前用户与 SYSTEM；当前用户必须有读取和删除权限。helper 以 `DeleteOnClose` 打开并按句柄身份复核，读取后文件必须消失。

示例准备逻辑（直播地址由调用环境安全提供，示例不包含真实地址）：

```powershell
$runRoot = 'D:\p3acc-runs\run-<GUID>'
$secretPath = 'D:\p3acc-secrets\live-<GUID>.txt'
[void][IO.Directory]::CreateDirectory($runRoot)
[void][IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($secretPath))
$utf8 = [Text.UTF8Encoding]::new($false)
[IO.File]::WriteAllText((Join-Path $runRoot '.p3acc-controller-owned'), "P3ACC-CONTROLLER/v1`n", $utf8)
try {
    if ([string]::IsNullOrWhiteSpace($env:P3ACC_LIVE_URL)) { throw 'P3ACC_LIVE_URL is required' }
    [IO.File]::WriteAllText($secretPath, $env:P3ACC_LIVE_URL, $utf8)
} finally {
    Remove-Item Env:P3ACC_LIVE_URL -ErrorAction SilentlyContinue
}

$current = [Security.Principal.WindowsIdentity]::GetCurrent().User
$system = [Security.Principal.SecurityIdentifier]::new('S-1-5-18')
$acl = [Security.AccessControl.FileSecurity]::new()
$acl.SetOwner($current)
$acl.SetAccessRuleProtection($true, $false)
$allow = [Security.AccessControl.AccessControlType]::Allow
$acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($current, 'FullControl', $allow))
$acl.AddAccessRule([Security.AccessControl.FileSystemAccessRule]::new($system, 'FullControl', $allow))
Set-Acl -LiteralPath $secretPath -AclObject $acl
```

## 启动

runner 入口还会在启动 relay、应用或 helper 前主动删除并复核 ambient `P3ACC_LIVE_URL`，防止调用者遗留的完整地址被任意子进程继承。此环境变量不是控制器输入；唯一输入是 `-LiveUrlFile`。

```powershell
$null = Get-NetTCPConnection -State Listen -LocalAddress 127.0.0.1 -LocalPort 7890 -ErrorAction Stop
$null = Get-Command sqlite3.exe -CommandType Application -ErrorAction Stop

powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass `
  -File D:\douyinLive\scripts\p3-acc-controller.ps1 `
  -AppExecutable 'D:\douyinLive\cmd\desktop\build\bin\douyinLive-p3acc.exe' `
  -LauncherExecutable 'D:\douyinLive\cmd\desktop\build\bin\p3acclauncher.exe' `
  -RelayExecutable 'D:\douyinLive\cmd\desktop\build\bin\p3accproxy.exe' `
  -ControllerRoot 'D:\p3acc-runs\run-<GUID>' `
  -LiveUrlFile 'D:\p3acc-secrets\live-<GUID>.txt' `
  -ClashUpstream '127.0.0.1:7890' `
  -EvidenceAckTimeoutSeconds 300
```

控制器为 relay 选择动态回环端口，并创建 acceptance root 的私有同级 control root。两者均绑定 canonical path 与卷/File ID；路径或目录身份变化会导致验收失败。

带 tag 的桌面应用把 WebView2 user-data 固定在本次全新 acceptance root 下的 `webview2` 目录；普通构建不改默认行为。root 的新鲜度、重解析点和逃逸检查失败时必须 fail closed，最终 root 清理会连同该目录一起验证零残留。

上述 `7890` 是当前主机已实测的 Mihomo 监听端口；每次正式运行仍须先执行监听检查。若端口变化，只替换检查和 `-ClashUpstream` 的端口，不能持久写入全局代理。

## 独立视觉证据握手

交互式 helper 只把下列文件写入 acceptance root：

- `p3-acc-safe-status.png`：宽度 300–420、高度 180 的安全裁剪，不是全窗口截图。
- `evidence.ready`：`{"schema":"P3ACC-EVIDENCE/v1","sha256":"<64 lowercase hex>","width":<n>,"height":180}`。

macOS 协调端必须独立完成以下步骤：

1. 通过 SSH 只读取 `evidence.ready`，严格校验字段、schema、SHA-256 格式和尺寸。
2. 通过 SCP 只下载 `p3-acc-safe-status.png` 到本地临时路径；不得下载 acceptance root、数据库、manifest 或日志。
3. 本地重新计算 SHA-256，读取实际 PNG 尺寸，并与 `evidence.ready` 完全比较；人工确认裁剪图只包含安全状态区域。
4. 构造 `{"schema":"P3ACC-EVIDENCE-ACK/v1","sha256":"<same hash>"}`，先写同目录临时文件，再用 Windows 同卷 `Move-Item` 原子改名为 `evidence.ack`。

协调端不能让控制器自签 ACK，也不能在验证前写 ACK。ACK schema、哈希、大小、重解析点或目录身份任一异常都会失败。ACK 后 helper 才会发送 `WM_CLOSE`。

下面是当前测试主机可直接改 `<GUID>` 后执行的跨机命令。它只下载安全裁剪；`evidence.ready` 通过 stdout 读取，不递归复制 Windows 目录。

执行前还必须把 `<SSH_KEY_ABSOLUTE_PATH>`、`<WINDOWS_SSH_USER>` 和 `<WINDOWS_HOST>` 替换为当前环境的实际值；示例不固化个人目录、账户或内网地址。

```bash
set -euo pipefail

SSH_KEY='<SSH_KEY_ABSOLUTE_PATH>'
WIN_HOST='<WINDOWS_SSH_USER>@<WINDOWS_HOST>'
RUN_ROOT='D:\p3acc-runs\run-<GUID>'
RUN_ROOT_SCP='/D:/p3acc-runs/run-<GUID>'
EVIDENCE_DIR=$(mktemp -d -t p3acc-evidence)
READY_LOCAL="$EVIDENCE_DIR/evidence.ready"
CROP_LOCAL="$EVIDENCE_DIR/p3-acc-safe-status.png"

ssh -i "$SSH_KEY" "$WIN_HOST" \
  "powershell -NoLogo -NoProfile -NonInteractive -Command \"Get-Content -LiteralPath '$RUN_ROOT\\evidence.ready' -Raw\"" \
  > "$READY_LOCAL"

jq -e '
  keys == ["height","schema","sha256","width"] and
  .schema == "P3ACC-EVIDENCE/v1" and
  (.sha256 | test("^[0-9a-f]{64}$")) and
  (.width >= 300 and .width <= 420) and .height == 180
' "$READY_LOCAL" >/dev/null

scp -q -i "$SSH_KEY" \
  "$WIN_HOST:$RUN_ROOT_SCP/p3-acc-safe-status.png" \
  "$CROP_LOCAL"

READY_SHA=$(jq -r .sha256 "$READY_LOCAL")
LOCAL_SHA=$(shasum -a 256 "$CROP_LOCAL" | awk '{print $1}')
CROP_WIDTH=$(sips -g pixelWidth "$CROP_LOCAL" | awk '/pixelWidth/{print $2}')
CROP_HEIGHT=$(sips -g pixelHeight "$CROP_LOCAL" | awk '/pixelHeight/{print $2}')
test "$LOCAL_SHA" = "$READY_SHA"
test "$CROP_WIDTH" = "$(jq -r .width "$READY_LOCAL")"
test "$CROP_HEIGHT" = "$(jq -r .height "$READY_LOCAL")"
open "$CROP_LOCAL"
```

人工确认安全裁剪后，再执行以下 ACK。PowerShell 源码以 POSIX 单引号格式串生成，避免 zsh 在 Base64 编码前展开 `$root`、`$hash` 等变量；ACK 先写同目录 GUID 临时文件，再原子改名。

```bash
ACK_PS=$(printf '$ErrorActionPreference="Stop";$root="%s";$hash="%s";$rootItem=Get-Item -LiteralPath $root -Force;if(-not $rootItem.PSIsContainer -or (($rootItem.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0)){throw "P3ACC_ACK_ROOT_INVALID"};$readyPath=Join-Path $root "evidence.ready";$ready=Get-Content -LiteralPath $readyPath -Raw|ConvertFrom-Json;if($ready.schema -cne "P3ACC-EVIDENCE/v1" -or $ready.sha256 -cne $hash){throw "P3ACC_ACK_READY_INVALID"};$ackPath=Join-Path $root "evidence.ack";if(Test-Path -LiteralPath $ackPath){throw "P3ACC_ACK_EXISTS"};$temporary=Join-Path $root ("evidence.ack."+[Guid]::NewGuid().ToString("N")+".tmp");$payload=[ordered]@{schema="P3ACC-EVIDENCE-ACK/v1";sha256=$hash}|ConvertTo-Json -Compress;[IO.File]::WriteAllText($temporary,$payload,[Text.UTF8Encoding]::new($false));Move-Item -LiteralPath $temporary -Destination $ackPath -ErrorAction Stop' "$RUN_ROOT" "$LOCAL_SHA")
ACK_B64=$(printf '%s' "$ACK_PS" | iconv -f UTF-8 -t UTF-16LE | base64 | tr -d '\n')
ssh -i "$SSH_KEY" "$WIN_HOST" \
  "powershell -NoLogo -NoProfile -NonInteractive -EncodedCommand $ACK_B64"
```

ACK 写入后保留本地安全裁剪与 `evidence.ready` 作为人工验收附件；不要保存或输出 secret 文件。

## 成功报告

stdout 恰好是一份 `P3-ACC-CONTROLLER/v1` JSON。报告仅保留：

- 阶段布尔结论与固定错误码；
- 拓扑样本计数和四项布尔证明；
  - 故障前样本仅在十分钟稳态、崩溃恢复和当前 attempt 强 fence 全部成立后开始，恢复后另建独立 epoch；
  - 两个阶段各要求同一 app/relay/FFmpeg 身份下连续 3 个 `Established` 样本，客户端连接必须与 relay 反向端口行对应；
  - 采样窗口内 FFmpeg 正常退出或必要连接短暂缺失只会重置连续窗口，明确直连、异常 relay 远端、PID 复用或客户端 UDP 仍立即失败；
- 安全裁剪哈希、尺寸和 ACK/关闭结论；
- SQLite quick-check/unlock 结论；
- 资源窗口、owned process tree CPU，以及各资源的 baseline/peak/latest/delta/latter-half delta/trend：
  - 只有 process count、working set、private bytes、threads、handles、goroutines、heap alloc、heap in-use 和 system 这 9 项 leak 指标要求 latter-half trend 为 `STABLE` 或 `FALLING`；
  - 每项 metric 在样本数少于 2 时必须为 `INSUFFICIENT`，否则必须由 latter-half delta 与该项固定阈值精确推导为 `STABLE`、`RISING` 或 `FALLING`；累计 process/data-root 指标有活动时允许且应当为 `RISING`；
  - database WAL size 只证明已采样，允许为 0 或随 checkpoint 回落，不参与 leak 趋势门禁；
  - `processReadBytes`/`processWriteBytes` 及其速率来自外层 Windows Job 的 `JobObjectBasicAndIoAccountingInformation` 累计 I/O transfer counters，覆盖该 Job 内活动、已退出及嵌套 Job 子进程；它们只是进程 I/O 辅助证据，不代表磁盘写入，也不参与正式磁盘 I/O 门禁；
  - `dataRootPhysicalBytes` 以受控 data-root 当前 regular-file footprint 为原始样本，累加相邻样本的正向净增长；shrink 只更新 raw baseline、不扣减累计值，因此公开 metric 单调并作为磁盘写入下界；average/latter-half disk write rate 只由该下界换算，是正式磁盘 I/O 门禁的唯一来源，但不代表设备完整吞吐；
  - 事件队列报告 queue count、items、bytes 与同一窗口内采样到的真实 item/byte capacity，使用量必须落在对应容量内；
  - `eventQueueObserved` 必须且只能由 `eventQueueCount.peak > 0` 得出；样本少于 2 时所有 latter-half delta 必须为 0，`cpuTrend` 必须为 `INSUFFICIENT`，且 `stableWindowProven`、`cpuWithinTarget`、`diskIoObserved` 必须为 false；
  - `stableWindowProven` 必须严格等于完整样本、至少 30 个样本、窗口至少 600000 ms、WAL/磁盘写入/事件队列均已观察且队列容量有效的合取；`cpuWithinTarget` 必须严格等于该稳定窗口成立且 average CPU 小于 10%；`PreFaultReady` 还会独立重验至少 30 个样本和 600000 ms 窗口，不能由伪造派生布尔提前放行；
- UI 延迟与 attempt/segment/artifact/gap 数量；
- 清理结论。

报告不得增加 URL、房间号、Cookie、响应头、文件内容、绝对路径、PID、命令行或原始 stderr。成功退出码为 0；任何失败（包括清理不确定）退出码为 1。

## 离线回归

离线测试不会访问真实直播间：

```powershell
powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass `
  -File D:\douyinLive\scripts\p3-acc\tests\P3AccController.Offline.Tests.ps1
powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass `
  -File D:\douyinLive\scripts\p3-acc\tests\P3AccController.CleanupRace.Tests.ps1
```

## 显式交互式计划任务门禁

普通 `p3accacceptance` 标签故意不编译真实交互式计划任务测试，避免无交互桌面的常规包门禁被宿主环境误伤。真实 High→Medium Job Object/MIC 门禁必须同时启用专用标签与进程级环境变量：

```cmd
cd /d D:\douyinLive
set P3ACC_RUN_INTERACTIVE_TASK_TEST=1&& go test -v -tags p3accacceptance,p3accinteractive .\cmd\p3acclauncher -run TestP3ACCLauncherRealInteractiveScheduledTaskNestedJob -count=1 -timeout=120s
```

有效证据必须同时包含：

- `P3ACC_INTERACTIVE_SCHEDULED_TASK_GATE=RUNNING`；
- `P3ACC_INTERACTIVE_SCHEDULED_TASK_GATE=PASSED`；
- 测试本身为 `PASS`，而不是 `SKIP`；
- 结束后 `DouyinLive.P3ACC.*` 计划任务、相关进程和测试临时目录均为 0。

若只启用双标签但没有把 `P3ACC_RUN_INTERACTIVE_TASK_TEST` 精确设为 `1`，测试必须立即失败且不得创建计划任务。该门禁使用 `.invalid` 隔离 fixture，不访问真实直播间。

测试与静态门禁通过后，才允许在独立 root/secret/relay 上执行一次真实验收。真实验收结束后，应确认计划任务、应用树、relay、secret、acceptance root 和 private control root 均为零残留。
