# 数据、时间轴与直播分析开发计划

> 上级计划：[总开发计划](00-master-development-plan.md)
> 相关计划：[桌面 UI](01-desktop-ui-development-plan.md) · [采集与录制](02-capture-and-recording-development-plan.md) · [工程与发布](04-engineering-testing-and-release-plan.md)

## 1. 目标与原则

本计划定义直播场次的持久化、统一时间轴、指标计算、ASR 扩展、报告、导出和隐私策略。数据层必须满足：采集回调不被慢查询阻塞、应用崩溃后尽量恢复、媒体文件可以独立发现、所有自动结论可以追溯到事件或时间区间。

原则：

- SQLite 保存索引和标准化数据，文件系统保存媒体、大型原始载荷和可重建产物。
- 先保存、后分析；分析失败不得影响采集。
- 原始事实不可覆盖，校准和聚合以版本化派生数据表达。
- 时间、算法、数据完整性和隐私状态都必须显式记录。

## 2. 数据根目录

默认根目录使用 Windows 用户本地应用数据目录下的产品子目录；用户可改到其他本地固定磁盘。网络共享、可移动磁盘和云同步目录默认显示风险警告但不硬性禁止。

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

- 配置、数据库、媒体和日志分别设置权限；凭据文件不得进入诊断包。
- 用户选择新目录时先执行可写、原子重命名、空闲空间和路径长度检查。
- 目录迁移采用“复制 → 校验 → 切换 → 保留旧目录提示”，不边运行边移动活跃场次。

## 3. SQLite 约定

### 3.1 连接和 PRAGMA

- 使用 `database/sql` 和无 CGO SQLite 驱动。
- 启动时单连接执行迁移；运行时一个写连接池和有限读连接池。
- `journal_mode=WAL`、`foreign_keys=ON`、`busy_timeout=5000`、`synchronous=NORMAL`。
- 每次启动执行快速完整性检查；异常时进入只读恢复模式，不自动覆盖数据库。
- schema 版本保存在 `schema_migrations`，迁移只前进；升级前生成数据库备份。

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
| `started_at` | INTEGER | 场次墙钟起点 |
| `ended_at` | INTEGER NULL | 场次结束 |
| `media_epoch_at` | INTEGER NULL | 媒体基准墙钟 |
| `capture_offset_ms` | INTEGER | 校准偏移，默认 0 |
| `clock_source` | TEXT | media/received/calibrated |
| `integrity_score` | REAL | 0–1 完整性分数 |
| `data_path` | TEXT | 相对数据根路径 |
| `schema_version` | INTEGER | `session.json` 版本 |
| `created_at`/`updated_at` | INTEGER | 审计时间 |

索引：`(room_config_id, started_at DESC)`、`status`。

### 4.3 `live_events`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 事件 ID |
| `session_id` | TEXT FK | 所属场次 |
| `method` | TEXT | 平台 method |
| `kind` | TEXT | chat/gift/like/member/follow/system/unknown |
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

唯一约束：`(session_id, dedupe_key)`；索引：`(session_id, session_offset_ms)`、`(session_id, kind, session_offset_ms)`、`user_hash`。

### 4.4 `media_segments`

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `id` | TEXT PK | 分片 ID |
| `session_id` | TEXT FK | 所属场次 |
| `sequence` | INTEGER | 从 1 开始连续序号 |
| `relative_path` | TEXT | 工作/播放文件路径 |
| `container` | TEXT | mkv/mp4/ts 等 |
| `video_codec`/`audio_codec` | TEXT | 探测结果 |
| `started_at`/`ended_at` | INTEGER | 墙钟范围 |
| `pts_start_ms`/`pts_end_ms` | INTEGER NULL | 媒体时间戳 |
| `duration_ms` | INTEGER | 探测时长 |
| `size_bytes` | INTEGER | 文件大小 |
| `sha256` | TEXT NULL | 完成后计算 |
| `status` | TEXT | partial/complete/recovered/corrupt/missing |

唯一约束：`(session_id, sequence)`；路径在同场次内唯一。

### 4.5 `transcript_segments`

- `id`、`session_id`、可选 `media_segment_id`。
- `start_ms`、`end_ms`、`text`、`confidence`、可选 `speaker`。
- `provider`、`model`、`language`、`analysis_version`。
- `source_audio_sha256`，用于判断转写是否仍对应当前音频。

索引：`(session_id, start_ms)`；相同音频 hash、提供器和模型结果可复用。

### 4.6 `metric_buckets`

每个场次默认 10 秒一桶：

- `session_id`、`bucket_start_ms`、`bucket_size_ms` 联合唯一。
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

## 5. 写入链路与恢复

### 5.1 Spool 优先

事件先写场次 JSONL/WAL，再批量写 SQLite：

1. 回调将 `IngestEnvelope` 放入有界通道。
2. spool writer 按顺序追加记录，记录长度、CRC32C 和 schema 版本。
3. 每 250 条或 500 ms 提交一次 SQLite 事务。
4. 事务成功后更新 checkpoint；已落库块可延迟压缩归档。
5. 崩溃恢复从 checkpoint 后重放，依赖唯一约束保证幂等。

默认每秒 `fsync` 一次；正常收尾强制 flush + sync。用户可选择“更高可靠性”每批 sync，但 UI 必须说明性能影响。

### 5.2 原始载荷

- 原始 protobuf 使用 binpack：固定头、事件 ID、长度、CRC 和字节内容。
- 单文件达到 128 MiB 或场次跨小时后轮转。
- 数据库只保存相对文件、offset 和 length。
- 校验失败不阻止其他事件读取，并创建 `DATA_CORRUPTED` 诊断。

### 5.3 数据库忙与损坏

- `SQLITE_BUSY` 在 5 秒窗口内指数退避；spool 继续接收。
- 超过阈值进入 `degraded_persistence`，UI 告警但不立即断开直播。
- spool 超出配置上限时停止录制并保留消息链路最后诊断；禁止无提示丢弃。
- 完整性检查失败时复制数据库、进入只读模式并提供“从 session.json/spool 重建索引”工具。

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
- 分片重叠优先选择状态 `complete` 且校验通过者；分片缺失返回 Gap，不静默跳到错误画面。
- 首版验收目标：无人工校准时事件与音视频 P95 误差 ≤ 5 秒；完成校准标记后 P95 ≤ 1.5 秒。

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
- 场次媒体采用 manifest + SHA-256 校验，可选择只备份数据库和报告。
- 恢复先导入到临时目录并校验版本、路径穿越和 hash，再合并索引。
- 外部导入包不得覆盖现有 UUID；冲突时生成新 ID 并维护映射。

## 11. 保留与隐私

- 默认不自动删除；用户可设置按天数、总空间或每房间保留场次。
- 清理顺序：可重建缓存 → 代理文件 → 用户确认的旧场次；原始媒体和事件不会无提示删除。
- 正在录制、分析、导出或标记保留的场次不可清理。
- 用户平台 ID 使用安装级随机盐 HMAC-SHA256；盐保存在凭据存储。
- 昵称和弹幕属于个人数据，设置允许“不保存昵称”或导出时进一步匿名化。
- 诊断日志不记录完整弹幕正文；需要调试时仅记录长度、hash 和事件 ID。

## 12. 分析测试

### 12.1 数据与迁移

- 每个 schema 版本升级、失败回滚、旧备份恢复和空数据库初始化。
- WAL 崩溃重放、重复事件、CRC 错误、数据库忙和磁盘空间不足。
- 相对路径、Windows 长路径、非法字符和目录迁移。

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

- 崩溃后 spool 可幂等重放，场次和媒体可从 manifest 恢复索引。
- 所有指标和报告带版本、输入 fingerprint 与完整性信息。
- 时间轴原始值、估计值和校准值互不覆盖。
- 基础分析不依赖 ASR 或云服务。
- 导出、诊断和日志不包含凭据、完整流 URL 或原始平台用户 ID。
- 数据库迁移、备份、恢复、保留和损坏降级均有测试。
