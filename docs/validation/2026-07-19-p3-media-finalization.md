# P3-MEDIA 媒体收尾验收记录

> 日期：2026-07-19
> 权威工作副本：Windows `D:\douyinLive`
> 范围：Schema v4、录制根、URL-free attempt、分片探测与原子定稿、WAV/MP4 代理、`media.json`、文件一致性、生命周期归属和持久化上界

## 1. 结论

P3-MEDIA-001 已完成。SQLite Schema v4 已成为媒体恢复事实源：外部录制根有耐久身份，场次媒体有独立 revision/dirty CAS，录制 attempt 在进程启动前入账，原始分片和派生产物分别保存来源、状态与内容证据。`media.json` 是不含流 URL 和绝对路径的可修复投影，不与 SQLite 争夺事实来源。

真实 FFmpeg 8.1.2 链路已从生产 RecorderFactory 运行至 Stop、MKV 定稿、SQLite/`media.json`、16 kHz 单声道 WAV 与 H.264/AAC MP4，并覆盖内部数据根和外部注册根。metadata + 首个目标媒体包的两阶段 ffprobe 会拒绝只有流头、零目标包或零活动范围的空壳媒体。完成态文件后续缺失、替换或变为非 regular/reparse 对象时会精确降级且保留原证据。

全量 Go、P3 标签、前端和最终 Wails production 门禁通过；重点 finalizer、仓储上限和 post-commit 回归重复通过。独立终审未发现 P0/P1。Windows 主机在线连通性在最终门禁后已恢复；P3-MEDIA 没有把站点可达冒充为 10 分钟在线录制成功，该项仍按计划保留给 P3-ACC。

## 2. Schema v4 与录制根

Schema v4 新增或扩展以下事实表：

- `recording_roots`：保存 UUIDv7 root ID、后端绝对路径、规范路径 SHA-256、卷身份 SHA-256、ready 状态和验证时间。
- `session_media`：以场次为主键，保存不可变 `root_id + relative_path`、`open/finalizing/completed/incomplete`、manifest revision/dirty、媒体时钟和 attempt journal。
- `media_segments`：在既有字段上增加 attempt ID/序号、源 partial 相对路径、探测版本和错误码，并增加大小写无关路径唯一约束。
- `media_artifacts`：按原始分片记录 `asr_wav/playback_mp4`、来源/产物 SHA-256、profile、状态和稳定错误码。

v3→v4 升级在修改前生成可打开的 v3 备份。迁移失败测试证明新增表、列和 schema version 同事务回滚；约束测试证明外部 root 不能在被引用时删除，场次媒体位置不能修改，大小写路径别名和跨场次 artifact 绑定不能写入。

外部根注册采用 marker-before-DB：

1. 只接受已存在的绝对目录，先验证规范路径、regular directory、可写、文件 Sync 与同目录原子改名。
2. 计算大小写稳定的 canonical key 和卷身份；marker 只含版本、root ID 与卷身份，不含绝对路径。
3. marker 以独占、写穿方式发布，再幂等提交 SQLite；崩溃发生在两者之间时可安全重试。
4. finalizer 在扫描、探测返回、发布和数据库提交等敏感阶段重新核对 marker、SQLite 行、规范路径与卷身份，任一漂移都 fail closed。

Windows 路径逐组件拒绝 symlink、junction/reparse、保留设备名、尾随点/空格、百分号和大小写别名。非法链接组件不会先创建根外目录。内部根使用 `root_id = NULL` 和 `rooms/...` 相对路径；外部根只承载媒体、音频和媒体 manifest，数据库、日志、`session.json` 与事件 spool 仍留在数据根。媒体相对路径限定为 ASCII UUIDv7 与固定目录/文件名，使 Windows 路径身份规则和 SQLite 大小写唯一约束保持一致；外部根本身仍可使用 Unicode 绝对路径。

## 3. Attempt、进程与清理归属

- 每个 attempt 最多保存 UUIDv7、ordinal、开始时间、分片秒数、committed/clean、受限 variant/protocol/quality/codec/bitrate；完整 URL、SourcePath 和绝对路径不能进入 journal、manifest、fmt、JSON 或 slog。
- Recorder 在创建 attempt 目录或启动 FFmpeg 前先以 CAS 耐久追加 `committed=false, clean=false`；journal 追加失败时进程不会启动。
- 只有启动握手确认实际媒体活动后才能单调写入 `committed=true`；优雅停止后才能写入 `clean=true`。旧 recorder、重复 ordinal 或状态倒退均返回冲突。
- Finalizer 扫描前以耐久 journal 为基线合并调用方快照；Repository 又在同一个 SQLite CAS 事务内拒绝 attempt 删除、截断、乱序、身份变更、`true→false` 和 `clean=true && committed=false`，避免重启或第二个 finalizer 绕过单调性。
- 进程启动后若 commit journal 失败，Recorder 停止该进程并保留 open attempt，供 P3-RCV 审计；构造器后续失败也不擅自运行无 owner 的长耗时代理收尾。
- 调用方取消只停止等待，Recorder.Stop 的共享完成仍等待进程/输出排空和 media finalizer。录制并发额度在 FFmpeg 完全退出后释放，代理生成使用独立的单并发额度。
- CaptureCoordinator 在 Recorder 或 EventSink 的后台收尾尚未结束时保持 finalizing owner；Application 在活动清理归零前不关闭 SQLite，避免“调用方超时后代理仍访问已关闭数据库”。

解析快照先拒绝超过 4,096 个候选，再完成稳定排序，最后只尝试前 64 个；因此高优先级合法候选不会被排序前截断。每场次最多 128 个 attempts，达到上限后不再启动新进程。

## 4. 原始分片探测与定稿

分片探测只接受绝对、regular、非 symlink、非空的 `.mkv.partial` 或 `.mkv`：

1. metadata 阶段读取 Matroska 格式、音视频流、编码、time base、时长和首末时间；输出最多 1 MiB，stderr 只保留 64 KiB 脱敏尾部。
2. activity 阶段在同一个 10 秒 context 和剩余输出预算内读取选定音/视频流的首个 packet。
3. 必须存在匹配流 packet，并形成正 duration 或正时间范围；只有声明流、立即 EOF、零 duration 或畸形/溢出时间都不可读。

finalizer 在 probe 前后使用同一打开文件身份、size、mtime/mode 和 SHA-256 双重核对。可读 partial 先 Sync，再以不替换既有目标的原子操作发布为最终 `.mkv`；同内容重复目标可恢复收敛，异内容目标持久化 `MEDIA_TARGET_CONFLICT`，不会覆盖任一文件。

不可读 partial 保留现场并登记 corrupt。已验证 complete/recovered 分片发生暂时 ffprobe 失败而内容未变时保留既有证据；发生删除、内容替换、同 stat 替换、零字节、目录、symlink 或 junction/reparse 时分别持久化 missing/changed，并保留原 size/SHA。第一次有效分片同时把 `session_media.media_epoch_at` 与 `live_sessions.media_epoch_at/clock_source=media` 在同一事务中推进。

## 5. WAV/MP4 代理

- 有音频的原始分片生成 `pcm_s16le`、单声道、16 kHz WAV；无音频登记 `not_applicable`。
- H.264 + 可选 AAC 使用 stream copy 生成 faststart MP4；其他含视频组合登记 `pending_transcode` 并保留原始 MKV，无视频登记 `not_applicable`。
- 代理先写唯一 `.partial`，成功后 Sync 并原子发布，不覆盖既有目标；FFmpeg stderr 有界且不泄漏路径或输入地址。
- 产物再经两阶段 ffprobe：WAV 必须恰有一个 16 kHz/mono/pcm_s16le 音频流和目标音频 packet；MP4 必须恰有一个 H.264 视频流、至多一个 AAC 音频流，并存在目标视频 packet。
- FFmpeg 从 finalizer 已打开的 source snapshot 经 `pipe:0` 读取；读取器对 FFmpeg 实际消费的每个字节同步计数和 SHA-256，只有完整消费且与耐久 segment size/SHA 一致才允许发布。发布前后还会复核 source 路径身份与绑定录制根，发布后再核对产物身份与 SHA。只有流头的 WAV/MP4、声明音轨但零音频包、生成期间替换、录制根漂移或发布冲突都保持非 Complete。
- failed/missing 产物可重试；“文件已发布、数据库提交前崩溃”可在 profile、packet 和双哈希均通过时收养。已完成产物被替换或发生冲突后进入稳定 changed/conflict，不会在下一轮静默升级。

代理失败只产生显式 artifact 状态和 warning，不把已验证原始分片伪造为失败；场次媒体 completed 只要求全部原始分片为 complete/recovered。这样 ASR/播放代理可恢复，而原始录制证据保持独立。

## 6. SQLite、manifest 与有界性

- 每次 PersistMediaSnapshot 先校验输入，再在同一事务中 upsert segment/artifact、统计 upsert 后耐久并集、CAS revision、置 dirty；超过 4,096 segments 或 8,192 artifacts 时整笔回滚，revision 不变。
- 读取 segment/artifact 使用 `LIMIT maximum+1`，每行重新执行完整契约校验；数据库被绕过 API 写入非法行或超限行时 fail closed，不把截断结果伪装成完整快照。
- commit 返回不明时，用独立后台短 context 重读并比较完整预期快照；只有 durable state 完全一致才视为成功。commit 已成功但调用方取消或 post-commit reload 失败时，返回已构造的 committed snapshot，不能逆转已提交 attempt。
- `media.json` schema version 1 按 attempt ordinal、segment sequence 和 artifact 绑定稳定排序，使用临时文件、Sync 和原子替换写入；随后仅以相同 session/revision CAS 清除 `session_media.manifest_dirty`。
- manifest 最大 128 MiB，相对路径最大 2,048 字节，safe token 最大 128 字节；最大合法 128 attempts、4,096 segments、8,192 artifacts 和最大宽度路径已经过编码测试，超限不落盘。

## 7. 自动化与真实工具证据

以下门禁均在 Windows 权威工作副本执行并通过：

- `where go`：`C:\Program Files\Go\bin\go.exe`。
- `gofmt -l cmd\desktop internal\app internal\capture internal\room internal\storage`：无输出。
- `go test ./... -count=1 -timeout=300s`、`go vet ./...`、`go build ./...`。
- `go test -tags p3acceptance ./... -count=1 -timeout=300s`、`go vet -tags p3acceptance ./...`。
- `go test -tags p2acceptance ./cmd/desktop -count=1`。
- 最终修复后工作树执行 `go test ./internal/capture -count=20 -timeout=900s`，20 轮全包回归通过，耗时 302.218 秒。
- 整组 `TestSQLiteSessionMediaFinalizer` 20 轮、post-commit journal 回归 20 轮、完成态对象/分片/产物高风险回归 20 轮通过。
- source 实际消费字节、录制根漂移、durable journal 空/后缀合并及 Repository journal 删除/篡改/倒退拒绝等新增高风险回归各 20 轮通过。
- 4,096/8,192 精确边界、重复超限回滚、maximum+1 污染读取的重型 cardinality 测试外层 20 轮通过。
- 真实 FFmpeg 分片、空 Matroska、活跃/空壳 WAV/MP4、声明音轨但目标音频零包、生产工厂内部/外部根 E2E 通过。
- 前端 `pnpm typecheck`、3 个 Vitest 文件共 6 项测试、`pnpm build`。
- 从 `D:\douyinLive\cmd\desktop` 执行 `wails build -clean -platform windows/amd64`，最终 production 构建通过；EXE 为 47,647,744 字节，SHA-256 `7e7d668ef42b4e61b1edfed43921ef2fc61dec2a12291612673ffeef3d474e91`；生成绑定仅产生的空白噪声经语义审计后恢复。
- `git diff --check` 通过，独立代码审计结论为 P0=0、P1=0。

当前 SSH Go 环境为 `CGO_ENABLED=0` 且没有 GCC，因此 `go test -race` 在启动阶段不可用；这是一项已记录的工具链边界，不是源码测试失败，也未擅自安装编译器或更改系统配置。

## 8. 在线直播间、隐私与剩余范围

授权直播间只应通过进程环境注入，不写入源码、测试、文档或持久化日志。最终门禁后对站点根执行只读 HEAD 已获得 HTTP 响应，证明 Windows 主机的 DNS、TLS 和 HTTP 连通性恢复；本节仍不声明 P3-ACC 在线录制成功，授权房间的 10 分钟、断网和真实下播验收统一留在 P3-ACC。工作树隐私扫描未发现授权房间标识、完整地址、查询参数、Cookie 或有效流 URL。

P3-RCV-001 继续负责 1/2/5/10 秒退避、最多 10 次录制重启、`capture_gaps` 开闭、应用启动时 orphan/open attempt/partial 扫描和旧场次终态推进。P3-UI-001 负责实时录制进度与缺口告警；在线 10 分钟、断网、FFmpeg 崩溃和真实下播收尾由 P3-ACC-001 统一验收。
