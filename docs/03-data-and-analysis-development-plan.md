# 数据、时间轴与直播分析开发计划

> 上级计划：[总开发计划](00-master-development-plan.md)
> 相关计划：[桌面 UI](01-desktop-ui-development-plan.md) · [采集与录制](02-capture-and-recording-development-plan.md) · [工程与发布](04-engineering-testing-and-release-plan.md)
> 实施状态（2026-07-21）：P4-ANA-001 已完成版本化 10 秒指标桶、缺口完整度、稳健峰谷/高光候选、严格 React 报告与时间点回放；项目总进度 82%，下一任务为 P4-ASR-001。
> 最近验收：[P3-ACC 关闭记录](validation/2026-07-21-p3-acceptance-closeout.md)

## 1. 目标与原则

本计划定义直播场次的持久化、统一时间轴、指标计算、ASR 扩展、报告、导出和隐私策略。数据层必须满足：采集回调不被慢查询阻塞、应用崩溃后尽量恢复、媒体文件可以独立发现、所有自动结论可以追溯到事件或时间区间。

原则：

- SQLite 保存索引和标准化数据，文件系统保存媒体、大型原始载荷和可重建产物。
- 先保存、后分析；分析失败不得影响采集。
- 原始事实不可覆盖，校准和聚合以版本化派生数据表达。
- 时间、算法、数据完整性和隐私状态都必须显式记录。

## 2. 数据根目录

默认数据根使用 Windows 用户本地应用数据目录下的产品子目录；数据根承载数据库、配置、日志、事件 spool、报告与默认媒体。数据根迁移仍采用受控整库流程，不能用录制目录设置替代。

```text
<data-root>/
├── app.db
├── app.db-wal
├── config/
│   ├── settings.json
│   └── credentials.dat
├── rooms/<room-config-id>/sessions/...
├── logs/
├── cache/
├── exports/
└── backups/
```

P3-MEDIA 对 `RecordingDirectory` 采用独立媒体根语义：

- 默认内部媒体根是 `<data-root>/rooms`；`session_media.root_id` 为 NULL，媒体相对路径保留 `rooms/<room-config-id>/sessions/...`。
- 外部媒体根必须是已存在的绝对目录，先以 `.douyinlive-recording-root.json`、规范路径摘要、卷身份摘要和 SQLite `recording_roots` 行注册；外部场次媒体相对路径为 `<room-config-id>/sessions/...`。
- 外部根只保存 `media/`、`audio/` 和 `manifests/media.json`。对应数据库、配置、日志、`session.json`、事件 spool 与报告仍在数据根，二者通过稳定 root ID 和场次 ID 关联。
- 活动场次的媒体位置不可变；设置切换只影响后续场次。网络共享、可移动磁盘和云同步目录仍显示风险提示，身份或可写性漂移时录制 fail closed。

- 配置、数据库、内部/外部媒体根和日志分别设置权限；凭据文件不得进入诊断包，外部 root 的绝对路径也不得跨 DTO、通用格式化或结构化日志表面。
- 用户选择新目录时先执行可写、原子重命名、空闲空间和路径长度检查。
- 目录迁移采用“复制 → 校验 → 切换 → 保留旧目录提示”，不边运行边移动活跃场次。

## 3. SQLite 约定

### 3.1 连接和 PRAGMA

- 使用 `database/sql` 和无 CGO SQLite 驱动。
- 启动时单连接执行迁移；运行时一个写连接池和有限读连接池。
- `journal_mode=WAL`、`foreign_keys=ON`、`busy_timeout=5000`、`synchronous=NORMAL`。
- 每次启动执行快速完整性检查；异常时进入只读恢复模式，不自动覆盖数据库。
- schema 版本保存在 `schema_migrations`，迁移只前进；升级前生成数据库备份。
- 当前实现为 Schema v6。v3→v4 升级先保留可打开的 v3 备份，再在单一迁移事务中创建媒体对象；任一 DDL/约束失败都回滚表、列和版本。
- v4→v5 同样先创建可打开备份，再以单一迁移事务新增 `idx_live_sessions_recovery_page` 和 `idx_session_media_recovery_page` 两个恢复部分索引；注入任一 DDL 失败时索引与 schema version 一并回滚，v4 备份保持可恢复。
- v5→v6 先创建可打开的 v5 备份，再在单一迁移事务中重建 `metric_buckets`，把 `analysis_version` 纳入复合主键并逐列保留旧行与约束，同时增加 session/event/gap 三个稳定 keyset 索引；复制、DDL 或索引任一步失败时旧表、旧主键和 schema version 一并回滚。

### 3.2 标识、时间和枚举

- 主键为 UUIDv7 文本。
- 时间为 `INTEGER` UTC Unix 毫秒，持续时间和偏移也使用毫秒。
- 布尔值为 `INTEGER 0/1`。
- 枚举在 Go 中定义稳定字符串，并由 `CHECK` 约束或应用校验保护。
- JSON 字段只保存低频扩展数据，常用筛选字段必须有独立列。

## 4. 数据模型

### 4.1 `rooms`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 房间配置 UUIDv7 |
| `live_id` | TEXT UNIQUE | 用户输入的直播间标识 |
| `room_id` | TEXT NULL | 最近解析到的平台长 ID |
| `alias` | TEXT | 用户别名 |
| `anchor_name` | TEXT NULL | 最近主播名，受隐私设置控制 |
| `monitor_enabled` | INTEGER | 是否自动监听 |
| `record_enabled` | INTEGER | 是否自动录制 |
| `recording_profile_json` | TEXT | 录制偏好，不含流 URL |
| `credential_ref` | TEXT NULL | 凭据引用，不存 Cookie |
| `created_at`/`updated_at` | INTEGER | UTC 毫秒 |

索引：`live_id` 唯一；`monitor_enabled` 普通索引。

### 4.2 `live_sessions`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 场次 ID |
| `room_config_id` | TEXT FK | 所属配置 |
| `platform_room_id` | TEXT NULL | 场次中的平台房间 ID |
| `title` | TEXT | 场次标题快照 |
| `status` | TEXT | starting/recording/finalizing/completed/interrupted/failed |
| `recording_status` | TEXT | disabled/pending/starting/recording/unavailable/reconnecting/finalizing/completed/incomplete/failed |
| `operation_id` | TEXT | Open/Rebind/Finalize 的非敏感 CAS 版本 |
| `manifest_dirty` | INTEGER | `session.json` 镜像尚未耐久确认 |
| `started_at` | INTEGER | 场次墙钟起点 |
| `ended_at` | INTEGER NULL | 场次结束 |
| `media_epoch_at` | INTEGER NULL | 媒体基准墙钟 |
| `capture_offset_ms` | INTEGER | 校准偏移，默认 0 |
| `clock_source` | TEXT | media/received/calibrated |
| `integrity_score` | REAL | 0–1 完整性分数 |
| `data_path` | TEXT | 相对数据根路径 |
| `schema_version` | INTEGER | `session.json` 版本 |
| `created_at`/`updated_at` | INTEGER | 审计时间 |

索引：`(room_config_id, started_at DESC)`、`status`；数据库以部分唯一索引保证同一房间至多一个 `starting/recording/finalizing` 场次。Schema v5 另以 `idx_live_sessions_recovery_page(id, created_at) WHERE status IN ('starting','recording','finalizing')` 支持固定截止 UUIDv7 keyset 恢复；Schema v6 以 `idx_live_sessions_playback_page(started_at DESC, id DESC)` 固定历史场次同起始时间的稳定顺序。状态变更使用旧场次状态、旧录制状态和旧 operation ID 联合 CAS；`manifest_dirty` 只有在文件原子替换及健康日志 Sync 后才能精确清除。

### 4.3 `live_events`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 事件 ID |
| `session_id` | TEXT FK | 所属场次 |
| `ingest_sequence` | INTEGER | 场次 source 接纳顺序；0 只保留给 v2 迁移行 |
| `method` | TEXT | 平台 method |
| `kind` | TEXT | chat/gift/like/member/follow/system/unknown |
| `event_role` | TEXT | source/aggregate |
| `platform_message_id` | TEXT NULL | 平台消息 ID |
| `dedupe_key` | TEXT | 场次内唯一去重键 |
| `message_create_at` | INTEGER NULL | 平台时间 |
| `received_at` | INTEGER | 接收时间 |
| `session_offset_ms` | INTEGER | 场次相对偏移 |
| `clock_confidence` | REAL | 0–1 时间可信度 |
| `user_hash` | TEXT NULL | HMAC 后用户标识 |
| `display_name` | TEXT NULL | 按隐私策略保存 |
| `content` | TEXT NULL | 弹幕/摘要文本 |
| `numeric_value` | REAL NULL | 礼物/计数等标准值 |
| `normalized_json` | TEXT | 扩展标准化字段 |
| `raw_file`/`raw_offset`/`raw_length` | TEXT/INTEGER | 原始载荷引用 |
| `parse_status` | TEXT | parsed/unknown/failed |
| `parse_error_code` | TEXT NULL | 稳定、脱敏的解析失败码 |
| `normalizer_version` | TEXT | 标准化器版本 |

唯一约束：`(session_id, dedupe_key)`；新 source 事件还受 `(session_id, ingest_sequence)` 条件唯一索引保护，aggregate 可与其来源共享序列。索引：`(session_id, ingest_sequence, event_role)`、`(session_id, session_offset_ms)`、`(session_id, kind, session_offset_ms)`、`user_hash`；Schema v6 增加 `(session_id, session_offset_ms, id)`，以 UUIDv7 ID 打破同偏移事件并列。

### 4.4 Schema v4 媒体索引

P3-MEDIA 将录制根、场次媒体快照、原始分片和派生产物拆成四个耐久契约。SQLite 是事实来源；`media.json` 只是不含流 URL、绝对路径和敏感查询参数的可修复投影。

#### 4.4.1 `recording_roots`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 外部录制根 UUIDv7 |
| `absolute_path` | TEXT | 仅后端使用的规范绝对路径，不进入 DTO/日志 |
| `canonical_key` | TEXT UNIQUE | 规范路径的 64 字符小写十六进制 SHA-256 |
| `volume_identity` | TEXT | 卷名/序列/文件系统组合的 SHA-256 |
| `status` | TEXT | 当前仅允许 ready |
| `created_at`/`updated_at`/`last_verified_at` | INTEGER | 注册和最近身份核验时间 |

目录内 marker 只保存版本、root ID 与卷身份。marker 先原子落盘再提交 SQLite，因此进程在两者之间崩溃可安全重试；marker、数据库行、规范路径或卷身份不一致时拒绝继续。被 `session_media` 引用的 root 受 `ON DELETE RESTRICT` 保护。

#### 4.4.2 `session_media`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `session_id` | TEXT PK/FK | 一场次一份媒体快照，级联引用 `live_sessions` |
| `root_id` | TEXT NULL FK | NULL 表示内部数据根；非 NULL 引用外部录制根 |
| `relative_path`/`relative_key` | TEXT | 受限相对路径及其大小写无关唯一键 |
| `state` | TEXT | open/finalizing/completed/incomplete |
| `manifest_revision` | INTEGER | 从 0 单调递增的媒体 CAS 版本 |
| `manifest_dirty` | INTEGER | `media.json` 尚未按本 revision 耐久确认 |
| `media_epoch_at` | INTEGER NULL | 首个有效媒体墙钟基准 |
| `attempts_json` | TEXT | 按 ordinal 排序、URL/路径均已剔除的 attempt journal |
| `created_at`/`updated_at` | INTEGER | 严格非倒退审计时间 |

内部路径全局唯一，外部路径按 `(root_id, relative_key)` 唯一；触发器禁止修改 `root_id` 或 `relative_path`。attempt 只保存 UUIDv7、ordinal、开始时间、分片秒数、committed/clean 及受限协议/质量/编码/码率，最多 128 条。Schema v5 的 `idx_session_media_recovery_page(state, session_id) WHERE state IN ('open','finalizing')` 为启动媒体恢复提供有界索引。

#### 4.4.3 `media_segments`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 分片 UUIDv7 |
| `session_id` | TEXT FK | 所属场次 |
| `sequence` | INTEGER | 场次内稳定序号 |
| `relative_path`/`source_relative_path` | TEXT | 最终 `.mkv` 与 attempt `.mkv.partial` 相对路径 |
| `container` | TEXT | 当前完成媒体为 mkv |
| `video_codec`/`audio_codec` | TEXT NULL | ffprobe 探测结果 |
| `started_at`/`ended_at` | INTEGER | 墙钟范围 |
| `pts_start_ms`/`pts_end_ms` | INTEGER NULL | 媒体时间戳范围 |
| `duration_ms`/`size_bytes` | INTEGER | 探测时长与已核验字节数 |
| `sha256` | TEXT NULL | 已核验内容摘要 |
| `status` | TEXT | partial/complete/recovered/corrupt/missing |
| `attempt_id`/`attempt_sequence` | TEXT/INTEGER | 来源 attempt 与其内分片序号 |
| `probe_version`/`error_code` | TEXT | 探测规则版本与稳定失败码 |

唯一约束覆盖 `(session_id, sequence)`、`(session_id, attempt_id, attempt_sequence)`，最终路径和源路径还使用大小写无关唯一索引。每场次最多 4,096 行；读取使用上限加一，upsert 后在同一事务中核对耐久并集，超限不推进 revision。

#### 4.4.4 `media_artifacts`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 派生产物 UUIDv7 |
| `session_id`/`media_segment_id` | TEXT FK | 同场次原始分片复合外键 |
| `kind` | TEXT | asr_wav/playback_mp4 |
| `relative_path` | TEXT | 产物相对媒体根路径 |
| `container`/`codec` | TEXT | wav/pcm_s16le 或 mp4/h264 |
| `duration_ms`/`size_bytes` | INTEGER | 预期时长与已核验字节数 |
| `sample_rate`/`channels` | INTEGER | ASR WAV 固定为 16000/1 |
| `sha256`/`source_sha256` | TEXT | 产物与来源分片摘要 |
| `status`/`error_code` | TEXT | pending/pending_transcode/complete/failed/missing/not_applicable 及稳定失败码 |
| `created_at`/`updated_at` | INTEGER | 创建与最后审计时间 |

每个原始分片和 kind 唯一，路径按场次大小写无关唯一，复合外键禁止跨场次绑定。上限为每分片两个、每场次 8,192 个产物；`pending_transcode` 明确表示保留原始 MKV、后续再转码，不冒充已生成 MP4。

### 4.5 `transcript_segments`

- `id`、`session_id`、可选 `media_segment_id`。
- `start_ms`、`end_ms`、`text`、`confidence`、可选 `speaker`。
- `provider`、`model`、`language`、`analysis_version`。
- `source_audio_sha256`，用于判断转写是否仍对应当前音频。

索引：`(session_id, start_ms)`；相同音频 hash、提供器和模型结果可复用。

### 4.6 `metric_buckets`

每个场次默认 10 秒一桶：

- `session_id`、`analysis_version`、`bucket_start_ms`、`bucket_size_ms` 构成复合主键；同一版本不得覆盖，同一时间桶可并存多个分析版本。
- `chat_count`、`unique_chatters`、`like_delta`、`gift_count`、`gift_value`。
- `follow_count`、`enter_count`、`active_users`、`message_total`。
- 可选 `speech_ms`、`silence_ms`、`words_per_minute`、`sentiment_score`。
- `analysis_version`、`completeness`。

### 4.7 `analysis_reports`

- `id`、`session_id`、`status`、`analysis_version`。
- `input_fingerprint`：事件范围、媒体 hash、转写版本和配置的组合 hash。
- `summary_json`、`highlights_json`、`topics_json`、`questions_json`。
- `started_at`、`completed_at`、`error_code`、`error_summary`。

同一 `input_fingerprint` 的已完成报告默认复用；用户强制重跑时创建新版本，不覆盖旧报告。

### 4.8 `capture_gaps`

- `id`、`session_id`、可选 `media_segment_id`。
- `kind`：message_disconnect/recording_restart/stream_unavailable/disk_full/process_crash/clock_uncertain。
- `started_at`、`ended_at`、`start_offset_ms`、`end_offset_ms`。
- `severity`、`recovered`、`reason_code`、脱敏 `details_json`。
- `kind` 另包含 `event_persistence`，用于本地队列、spool 或 SQLite 事件链路缺口。
- `dedupe_key` 在场次内唯一，使恢复和重试可以幂等物化同一缺口。
- P3-RCV 使用稳定 dedupe key 写入/关闭运行期 `recording_restart`，并在启动时写入 `process_crash` 或 `message_disconnect`、必要的 `event_persistence` 与 `clock_uncertain`；同一终态事务会关闭该场次遗留 open gap，恢复重跑不得制造重复缺口。
- Schema v6 增加 `(session_id, start_offset_ms, id)` 索引，以 UUIDv7 ID 打破同偏移缺口并列。

### 4.9 `event_ingest_checkpoints`

每个场次一行：

- `session_id` 主键并级联引用 `live_sessions`。
- `committed_sequence`：最后一个与全部派生状态共同提交的 source 序列。
- `state`：`open/closing/closed/degraded`；触发器限制正向转换，closed 后位置和时间不可变。
- `privacy_key_id`：HMAC 密钥的非敏感标识；场次建立后不可修改，恢复时不匹配即 fail closed。
- `spool_file/spool_offset` 与 `raw_file/raw_offset`：最后完整提交记录之后的相对文件和字节位置。
- `updated_at`：UTC 毫秒；序列为 0 时文件和位置必须同时为空/0，正序列时两组位置必须同时有效。

### 4.10 `gift_combo_states`

每个 `(session_id, combo_key)` 一行：

- `status`：`open/closed`；closed 行由触发器保持不可变。
- `user_hash`、`gift_id`、可选 `gift_name`、`total_count` 和可选 `total_value`。
- `first_ingest_sequence/last_ingest_sequence`、`started_at/updated_at`。
- closed 行必须同时具有 `closed_at` 和 `aggregate_event_id`，后者以 `(session_id, id)` 外键指向同场次 `live_events`。
- `normalizer_version` 固定派生规则版本；恢复只查询当前批触及状态，空闲和收尾扫描使用固定页大小。

## 5. 写入链路与恢复

### 5.1 Spool 优先

事件先写场次 raw binpack 和元数据 WAL，再批量写 SQLite：

1. 回调深拷贝 payload，将带场次序列的 `IngestEnvelope` 非阻塞放入条数与字节双有界的单 FIFO；紧急额度只是接纳预留，不建立第二条乱序通道。
2. 单 worker 先追加 raw 帧，再追加只含 envelope 元数据和 raw 引用的 WAL 帧；两者都包含版本、长度和 CRC32C。
3. 每批严格执行 raw flush/Sync，再执行 WAL flush/Sync；只有两者都耐久后才进入标准化与数据库提交。
4. 默认每 250 条或 500 ms 把 source/aggregate 事件、礼物折叠、缺口和 checkpoint 放入同一 SQLite 事务，并以旧 checkpoint 序列做 CAS。
5. 事务成功才更新内存 checkpoint；失败时 source 和派生状态均不确认，WAL 尾部保持可重放。
6. 崩溃或降级恢复只从 checkpoint 后按固定批次重放，重新执行去重和礼物折叠，依赖唯一约束及 closed 不可变约束保证幂等。

事件管理器每批建立 Sync 屏障，spool 的 1 秒定时 Sync 是未显式建屏障写入的最长默认窗口；正常收尾仍强制排空、flush、Sync，并把 checkpoint 从 closing 推进为 closed。

### 5.2 原始载荷

- 原始 protobuf 使用 binpack：固定头、版本、压缩标志、原始/存储长度、头与正文 CRC32C 和字节内容。
- 大于 256 字节且压缩后确实更小时使用单并发 zstd；解压设置最大内存与最大 payload 限制，禁止压缩炸弹。
- raw 单文件达到 128 MiB 或跨小时后轮转；WAL 单文件达到 64 MiB 或跨小时后轮转。
- 数据库只保存相对文件、offset 和 length。
- 校验失败不阻止其他事件读取，并创建 `DATA_CORRUPTED` 诊断。
- 每场次 raw+WAL 默认总上限 4 GiB；启动恢复发现的既有分片计入同一预算，零值配置不表示无限制。

### 5.3 数据库忙与损坏

- `SQLITE_BUSY` 在 5 秒窗口内指数退避；spool 继续接收。
- 超过阈值进入 `degraded_persistence`，UI 告警但不立即断开直播；不得 source-only 推进 checkpoint，也不得把 deferred 事件或全场礼物状态无限保留在内存。
- 降级期间继续写有界 spool；后续批次或关闭时从最后 checkpoint 尾部恢复。每个恢复事务完整提交 source、aggregate、礼物状态和 checkpoint，失败仍停留在原位置。
- 队列最终拒绝、payload 超限或 spool 致命失败必须累计为 `EVENT_DROPPED_LOCAL`，并以稳定 dedupe key 写入 `event_persistence` 缺口；缺口事务失败时保留累计状态，不能误报已审计。
- spool 超出 4 GiB 上限、发生中段损坏或无法 Sync 时停止该事件 sink 的接纳并保留严重诊断；不得绕过损坏继续推进 checkpoint，也不得无提示丢弃。
- 完整性检查失败时复制数据库、进入只读模式并提供“从 session.json/spool 重建索引”工具。

### 5.4 媒体快照与 manifest

- Recorder 在任何 FFmpeg 进程创建媒体前先用 `session_media.manifest_revision` CAS 追加 URL-free/path-free attempt；启动提交和优雅停止只能把 committed/clean 从 false 单调推进为 true。
- Finalizer 先审计已完成证据，再有界扫描 attempt/final 文件；metadata 与首个目标流 packet 共用一次 10 秒/1 MiB ffprobe 预算。探测前后文件身份、大小、SHA-256 或外部 root 身份变化均 fail closed。
- 第一笔 CAS 把分片和计划产物写为 finalizing，成功 materialize `media.json` 后生成 WAV/MP4，再次审计原始与产物，第二笔 CAS 才推进 completed/incomplete。首次有效媒体时钟与 `live_sessions.media_epoch_at/clock_source` 在同一数据库事务中推进。
- 每次媒体 CAS 都先把 `manifest_dirty` 置 1；`media.json` 用临时文件、Sync 和原子替换写入，随后只按同 session/revision 精确清脏。提交结果不明时用独立短 context 重读完整耐久快照，不能让调用方取消逆转已提交 attempt。
- 已验证文件后续缺失、变更为零字节/目录/符号链接/junction/reparse，或内容摘要变化时，保留原 size/SHA 证据并持久化 missing/changed。失败/缺失代理可重试，发布后提交前崩溃可收养同源合法产物，冲突或变更产物不静默覆盖。

### 5.5 启动恢复一致性

- 应用在打开 SQLite 前按规范数据根摘要取得 Windows 全局实例租约，防止第二实例同时扫描、终态化或重启录制。租约失败、基础设施清理失败和全部非空未知恢复错误只向外暴露稳定错误码。
- 每次启动固定 `scan_cutoff`，按 `id` 严格递增分页活动场次；仓储在返回前验证 UUIDv7、创建时间不晚于截止、页长和精确 cursor。任何畸形页均在进程/文件/数据库副作用前失败。
- 非 disabled 场次严格按进程证据→媒体快照→事件尾部→场次终态执行。Global Job 无法证明安全时不得探测或改写媒体；媒体恢复保留 partial、重复、冲突和 orphan 现场，不删除原始证据。
- 事件恢复从 checkpoint 后提交完整 durable prefix，关闭 open gift folds，并推进 `closing→closed`。无 checkpoint 时审计 event、gift fold 及 eventstore 自有的 `event_persistence/EVENT_DROPPED_LOCAL` gap；capture 启动恢复 gap 不计入，防止自生成证据。没有证据才幂等成功，存在证据返回权威截止与永久恢复错误。发生永久 spool 损坏时也从这组权威数据重读最大截止；若该读取失败则返回 deferred 并保留活动场次，不能用旧截止误终态化。
- 截止时间取场次开始、媒体与权威事件/礼物时间的非倒退最大值；未来时间写入 `clock_uncertain`。`RecoverAndClose` 在一个事务内校验旧状态/operation/扫描截止，幂等关闭旧 gap、插入恢复 gap，并把场次推进 `interrupted`，提交不明时按耐久结果判定成功或 stale。
- closed checkpoint 仍必须审计 open gift fold，并验证 source 事件最大正 `ingest_sequence <= committed_sequence`；aggregate 不参与该序列校验。任一不一致返回稳定恢复错误并保留证据，不能把损坏数据库误报为幂等完成。

## 6. 统一时间轴

### 6.1 时间字段优先级

1. 原始保存平台时间与接收时间，不互相覆盖。
2. 媒体存在时以首个有效媒体 PTS 对应的墙钟作为 `media_epoch_at`。
3. `session_offset_ms = received_monotonic - session_monotonic_epoch`，避免系统校时造成倒退。
4. 后处理可使用平台时间和校准标记生成 `adjusted_offset_ms`，但保留原偏移。

### 6.2 平台时间可信度

- 自动识别秒、毫秒和微秒，但只有落在接收时间合理窗口内才接受。
- 平台时间缺失或明显异常时 `clock_confidence=0`。
- 同一消息类型出现稳定偏移时可以提高可信度，但不能把估计写回原字段。

### 6.3 媒体映射

- 每个分片维护墙钟范围、PTS 范围和场次偏移范围。
- 播放器定位事件时先找包含 `adjusted_offset_ms` 的分片，再换算为分片内时间。
- 分片重叠优先选择状态为 `complete/recovered`、SHA-256 与当前常规文件均通过审计者；`missing/corrupt` 返回 Gap，不静默跳到错误画面。播放 MP4 只有 H.264 + 可选 AAC 且通过目标视频包探测时才可直接使用，`pending_transcode` 必须显式降级到原始 MKV 或后续转码。
- 首版验收目标：无人工校准时事件与音视频 P95 误差 ≤ 5 秒；完成校准标记后 P95 ≤ 1.5 秒。

### 6.4 只读 playback 查询合同

- 内部 repository 只持有 SQLite reader，不提供写方法；场次按 `(started_at DESC, id DESC)`，事件按 `(session_offset_ms, id)`，缺口按 `(start_offset_ms, id)` 做 limit+1 keyset 分页。
- cursor 使用 URL-safe 无填充编码，内部负载固定版本、查询类型、过滤摘要、排序值和 UUIDv7；解码拒绝未知字段、尾随 JSON、错误版本、跨查询复用、非 UUIDv7 和过滤摘要不一致。
- 过滤集合先校验白名单、拒绝重复并排序后再摘要，因此顺序等价但语义变化的过滤条件不能续用旧 cursor；页大小不进入摘要，可在 1–100 内调整。
- 场次 DTO 排除 live ID、平台 room ID、operation ID、数据/媒体路径和 root 身份；事件 DTO 排除 method、平台消息 ID、dedupe、user hash、normalized JSON 与 raw 引用；缺口 DTO 排除 details JSON 与 dedupe key。
- 首个切片只覆盖 session/event/gap 查询，不进入 Wails 绑定，也不提供动态媒体 Range；segment/artifact、媒体定位、应用服务和前端回放在 P4-PLY-001 后续切片完成。
- 第二个切片新增按 (sequence, id) 的媒体 keyset 分页；segment/artifact DTO 排除相对路径、root/attempt 身份与内容摘要，只返回后续 Range 服务可解析的 opaque ID。时间轴优先用 media_epoch_at + PTS，否则回退分片墙钟；定位应用 capture_offset_ms，重叠时优先具备耐久摘要的 complete/recovered，MP4 直放还要求 H.264、产物摘要和来源摘要一致。
- PlaybackService 已作为只读应用服务装配并通过 Wails 门面暴露 session/event/gap/media/locate 查询；动态 Range 必须在打开文件时重新验证 root、路径 containment、常规文件身份、大小与摘要，不能仅信 DTO。
- 第三个切片完成动态 `/playback/media/<opaque-artifact-id>`：重新联查 artifact/segment/session root 绑定与来源摘要；外部 root 在禁止重命名的目录句柄覆盖下复核 marker、规范路径摘要与卷身份；目标文件以禁止共享写/删的 Windows 句柄固定身份，SHA-256、大小、同路径重开和最终句柄路径全部通过后才交给标准 HTTP Range。React 只消费严格 v1 allowlist，不接收路径或摘要。

## 7. 基础指标

### 7.1 10 秒桶

- 弹幕速率：`chat_count / bucket_seconds`。
- 独立互动用户：桶内非空 `user_hash` 去重。
- 点赞增量：优先使用消息 delta；只有累计值时取非负差，重置时开新基线。
- 礼物价值：只在有可靠平台价值映射时计算；否则展示数量，不猜测金额。
- 进入/关注：分别统计事件条数和独立用户。
- 完整性：桶覆盖时间减去消息/媒体缺口后的比例。

### 7.2 峰值与低谷

- 使用 30 秒滚动窗口平滑 10 秒桶。
- 候选峰值需高于本场中位数至少 2 个 MAD，且持续两个桶；相邻 60 秒候选合并。
- 低谷只在场次超过 10 分钟且完整性 ≥ 0.8 时计算。
- 每个候选保存基线窗口、结果窗口、贡献指标和算法版本。

### 7.3 环节效果

首版计算标准化分数，不把不同量纲直接相加：

```text
effect_score =
  0.30 * z(chat_rate) +
  0.20 * z(unique_interactors) +
  0.20 * z(like_delta) +
  0.15 * z(follow_count) +
  0.15 * z(gift_value_or_count)
```

当某指标不可用时按剩余权重归一化，并在报告中标明缺失项。分数只用于同一场次内排序；跨场次比较必须先按场次规模归一化。

### 7.4 P4-ANA-001 实施结果（2026-07-21）

- `internal/analysis` 以终态场次、source 事件和缺口为只读输入，在单事务中写入不可覆盖的 `metric_buckets` 与 completed `analysis_reports`；`analysis_version=basic-analysis/v1+<input_fingerprint 前 16 位>`，相同输入复用，输入或算法变化时旧版并存。
- 固定 10 秒桶覆盖弹幕、独立用户、点赞 delta/累计重置、礼物折叠与可靠价值、关注、进房、活跃用户、消息总量和缺口区间并集完整度；缺失礼物价值不猜金额，数值与数量保持分离。
- 30 秒平滑后以本场中位数与 MAD 生成候选，要求至少 2 MAD、连续两个桶并合并 60 秒内相邻区间；低谷仅对超过 10 分钟且完整度不少于 0.8 的场次开放，高光只从包含聊天证据的峰值派生。
- 报告 DTO 只包含聚合桶、候选贡献、证据桶、质量 warning 和版本，不包含 user hash、原始内容、raw 引用、媒体路径或本地根；React 再以 strict Zod v1 allowlist 拒绝未知字段。
- 固定事件集 golden 覆盖点赞累计重置、礼物回退、缺口和未知解析提示；服务集成覆盖版本并存、同指纹复用、终态门禁、畸形持久化和隐私字段白名单。

## 8. ASR 与文本分析

### 8.1 插件接口

```go
type ASRProvider interface {
    ID() string
    Validate(ctx context.Context) error
    Transcribe(ctx context.Context, input AudioInput, emit ProgressFunc) ([]TranscriptSegment, error)
}
```

首版内置 `disabled` 提供器和至少一个后续可实现的本地/远程适配点，但不把任何云服务作为基础统计的前置条件。API Key 使用凭据存储，只传给选中的提供器。

### 8.2 任务行为

- 音频按媒体分片或 15–30 分钟块处理，任务可取消和恢复。
- 对同一音频 SHA-256、提供器、模型和语言缓存结果。
- 转写段时间必须是场次偏移，保留提供器原始置信度。
- 单块失败最多重试三次；部分成功生成 `partial` 报告并显示缺失区间。

### 8.3 文本派生

- 高频问题：只从聊天文本中识别疑问句/问号和重复语义，输出样本数与时间。
- 话题：按固定时间窗从弹幕和转写生成关键词/聚类，保留来源片段引用。
- 主播语速：转写字数除以有效说话分钟；无可靠 VAD 时不输出沉默比例。
- 高光候选：互动峰值与转写/弹幕时间窗相交，候选不自动宣称因果关系。

## 9. 报告结构

`AnalysisReport` 包含：

- 场次元数据、分析版本、输入 fingerprint、完整性和警告。
- 总量指标与分钟/10 秒趋势。
- 峰值、低谷、高光候选及其时间范围和证据。
- 高频问题、话题、主播话术和 ASR 覆盖率。
- 连接、消息、媒体和时钟缺口。
- 可跳转回放的 `start_ms`/`end_ms`。

报告任何结论都必须能链接到至少一个指标桶、事件集合或转写区间。报告重算不会修改原始事件或旧报告。

## 10. 导入、导出与备份

### 10.1 导出

- CSV：事件、指标桶、转写和分片清单分别导出 UTF-8 BOM 文件。
- JSON：使用版本化 manifest，时间为 ISO 8601 UTC + 毫秒偏移。
- 报告可导出 HTML/JSON；首版不要求 PDF。
- 默认导出哈希用户 ID，不导出原始平台 ID、Cookie、流 URL 或签名。

### 10.2 备份

- SQLite 使用在线 backup API/一致性快照，不直接复制活跃 WAL 组合。
- 场次媒体备份以一致 SQLite 快照中的 root ID/revision/相对路径为索引，以同 revision `media.json` 与 SHA-256 校验文件；不能单独信任 manifest 中的路径重新绑定外部根。
- 外部 root 的 marker 与数据库 `recording_roots` 行必须成对备份和恢复，并在导入前重新验证规范路径、卷身份、regular-file/reparse 边界；不得自动覆盖目标机器已有 root ID。
- 恢复先导入到临时目录并校验版本、路径穿越和 hash，再合并索引。
- 外部导入包不得覆盖现有 UUID；冲突时生成新 ID 并维护映射。

## 11. 保留与隐私

- 默认不自动删除；用户可设置按天数、总空间或每房间保留场次。
- 清理顺序：可重建缓存 → 代理文件 → 用户确认的旧场次；原始媒体和事件不会无提示删除。
- 正在录制、分析、导出或标记保留的场次不可清理。
- 用户平台 ID 使用安装级随机盐 HMAC-SHA256；盐保存在凭据存储。
- 事件 HMAC 使用独立 32 字节凭据；数据库只保存 `privacy_key_id`，同一场次 checkpoint 建立后不可更换密钥，恢复时不匹配即拒绝处理。
- 昵称和弹幕属于个人数据，设置允许“不保存昵称”或导出时进一步匿名化。
- 诊断日志不记录完整弹幕正文；需要调试时仅记录长度、hash 和事件 ID。

## 12. 分析测试

### 12.1 数据与迁移

- 每个 schema 版本升级、失败回滚、旧备份恢复和空数据库初始化；Schema v2→v3 必须保留旧事件和旧缺口，并在失败时完整恢复表名、列和版本。
- Schema v3→v4 必须先生成可打开的 v3 备份，创建 `recording_roots/session_media/media_artifacts` 和扩展 `media_segments`；注入失败时表、列和 schema version 全部回滚。
- Schema v4→v5 必须先生成可打开的 v4 备份，再创建两个恢复部分索引；迁移成功、任一索引 DDL 注入失败、备份可打开和完整 rollback 都有回归。
- Schema v5→v6 必须保留可打开的 v5 备份和全部 metric bucket 字段，验证四列版本化复合主键、三个 keyset 索引及任一步失败后的表名、旧三列主键、索引和版本完整 rollback。
- 固定截止 keyset 分页覆盖 0/1/128/多页、非法 UUID、无序、cursor 回退/遗漏/多出和截止后新增行；所有错误必须在恢复副作用前 fail closed。
- raw/WAL 崩溃重放、重复事件、CRC/截断错误、Sync 顺序、数据库忙/满/损坏和 spool 总空间边界。
- checkpoint CAS、事务中途失败、closed checkpoint/combo 不可变、privacy key 不匹配和 source/aggregate 唯一约束。
- 高基数礼物、长时间数据库降级、连续本地拒绝和重启迟到消息必须验证内存、恢复批次及辅助耐久状态有界。
- 相对路径、Windows 长路径、非法字符和目录迁移。
- 外部 root marker-before-DB 重试、并发幂等、规范路径/卷身份漂移、复制 marker、symlink/junction/reparse、保留设备名、大小写与尾随点/空格别名。
- attempt/segment/artifact 精确上限、最大宽度 `media.json`、读取上限加一、upsert 后耐久并集超限整笔回滚，以及 post-commit 结果重读不受调用方取消影响。
- 真实 FFmpeg/ffprobe 覆盖 Matroska 与 WAV/MP4 的 metadata + 首个目标媒体包；空壳、零目标包、错误 profile、超时、输出超限和调用方取消都保持非 Complete。
- finalizer 重复运行、发布冲突、发布后数据库提交前崩溃、代理失败/缺失重试、完成文件删除/替换/非 regular 对象和探测/生成 TOCTOU，均验证状态、证据与文件现场不被静默改写。
- 无 checkpoint + durable event/fold 必须返回权威 cutoff 与永久错误；事件 durable prefix 后永久损坏也必须返回刷新后的权威 cutoff。cutoff 读取失败必须 deferred，closed checkpoint + open fold 必须拒绝完成，隐私/路径损坏不得泄露原值。
- 进程/媒体/事件组件的局部 context 错误、父 context 真取消、未知错误、提交不明、gap 冲突及重复恢复分别验证继续审计、立即中断或应用启动 fail closed 的边界。

### 12.2 时间轴

- 平台秒/毫秒/微秒、缺失时间、系统时钟跳变和 DST 切换。
- 分片重叠、缺失、PTS 非零起点和应用重启后的场次。
- 固定校准样本验证 P95 误差目标。

### 12.3 指标与报告

- 固定事件集的 10 秒桶 golden test。
- 点赞累计重置、礼物无价值、用户 ID 缺失和缺口低完整性。
- 峰值合并、权重缺失归一化和跨版本可重复性。
- ASR 全成功、部分失败、取消、缓存命中和未配置提供器。

## 13. 完成标准

- 崩溃后只从 checkpoint 尾部有界、幂等重放 spool；source、aggregate、礼物折叠和缺口不会出现部分提交。
- 媒体以 SQLite revision/dirty/CAS 为事实、以同 revision `media.json` 为可修复投影；原始分片只在 packet、身份、内容与根绑定均通过时完成，代理失败显式记录且不伪造原始媒体失败。
- 所有指标和报告带版本、输入 fingerprint 与完整性信息。
- 时间轴原始值、估计值和校准值互不覆盖。
- 基础分析不依赖 ASR 或云服务。
- 导出、诊断和日志不包含凭据、完整流 URL 或原始平台用户 ID。
- 数据库迁移、备份、恢复、保留和损坏降级均有测试。
- 启动恢复完成后不存在旧活动场次、open checkpoint/fold 或未审计 open gap；deferred/未知结果则应用不发布 Ready，保留全部耐久证据等待下次恢复。
