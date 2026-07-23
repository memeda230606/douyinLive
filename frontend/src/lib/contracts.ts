import { z } from 'zod'

const nonnegativeSafeIntegerSchema = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER)
const finiteNonnegativeNumberSchema = z.number().finite().nonnegative()
const capabilityIDSchema = z.enum(['overview', 'rooms', 'realtime', 'sessions', 'analysis', 'diagnostics', 'settings'])

const qualitySchema = z.enum(['auto', 'original', 'ultra', 'high', 'standard'])

export const dataStatusSchema = z.object({
  ready: z.boolean(),
  schemaVersion: z.number().int().nonnegative(),
  mode: z.string(),
  loggingReady: z.boolean(),
}).strict()

export const buildInfoSchema = z.object({
  productVersion: z.string().min(1),
  gitCommit: z.union([z.literal('unknown'), z.string().regex(/^[0-9a-f]{40}$/)]),
  buildTime: z.union([z.literal('unknown'), z.string().datetime({ offset: true })]),
  buildSource: z.string().regex(/^[A-Za-z0-9._/-]{1,128}$/),
  goVersion: z.string().min(1).max(64),
  wailsVersion: z.string().min(1).max(32),
  nodeVersion: z.string().min(1).max(32),
  ffmpegVersion: z.string().min(1).max(128),
  ffmpegSHA256: z.string().regex(/^[0-9a-f]{64}$/),
  ffmpegLicense: z.literal('GPL-3.0-or-later'),
  databaseSchemaVersion: z.number().int().positive(),
  settingsSchemaVersion: z.number().int().positive(),
  analysisAlgorithmVersion: z.literal('basic-analysis/v1'),
  exportSchemaVersion: z.literal('analysis-export/v1'),
}).strict()

export const bootstrapSchema = z.object({
  apiVersion: z.literal('v1'),
  name: z.string().min(1),
  version: z.string().min(1),
  state: z.enum(['CREATED', 'RUNNING', 'STOPPING', 'STOPPED']),
  build: buildInfoSchema,
  data: dataStatusSchema,
  capabilities: z.array(z.object({
    id: capabilityIDSchema,
    label: z.string().min(1),
    available: z.boolean(),
  }).strict()),
}).strict()

const cookieStatusSchema = z.object({
  configured: z.boolean(),
  updatedAt: z.number().int().nonnegative().optional(),
}).strict()

// Read contracts retain the v1 range so existing room rows remain editable;
// all new writes use roomFormSchema's stricter 5..30 minute range.
export const recordingProfileSchema = z.object({
  quality: qualitySchema,
  segmentMinutes: z.number().int().min(1).max(60),
  saveDirectory: z.string().optional(),
}).strict()

export const roomSchema = z.object({
  id: z.string().uuid(),
  liveId: z.string().min(1),
  roomId: z.string().optional(),
  alias: z.string().min(1),
  anchorName: z.string().optional(),
  monitorEnabled: z.boolean(),
  recordEnabled: z.boolean(),
  recordingProfile: recordingProfileSchema,
  cookie: cookieStatusSchema,
  createdAt: z.number().int().nonnegative(),
  updatedAt: z.number().int().nonnegative(),
}).strict()

export const roomsSchema = z.array(roomSchema)

export const runtimeStates = ['STOPPED', 'WAITING', 'STARTING', 'LIVE', 'RECORDING', 'RECONNECTING', 'FINALIZING', 'ERROR'] as const

export const recordingStatuses = ['pending', 'disabled', 'starting', 'recording', 'unavailable', 'reconnecting', 'finalizing', 'completed', 'incomplete', 'failed'] as const

export const roomStatusSchema = z.object({
  roomId: z.string().uuid(),
  liveId: z.string().min(1),
  alias: z.string().min(1),
  state: z.enum(runtimeStates),
  revision: nonnegativeSafeIntegerSchema,
  operationId: z.string().uuid().optional(),
  sessionId: z.string().uuid().optional(),
  recordingStatus: z.enum(recordingStatuses).optional(),
  liveName: z.string().optional(),
  title: z.string().optional(),
  lastCheckedAt: z.number().int().nonnegative().optional(),
  changedAt: z.number().int().nonnegative(),
  retryAt: z.number().int().nonnegative().optional(),
  errorCode: z.string().optional(),
  message: z.string(),
}).strict()

export const liveEventSchema = z.object({
  id: z.string().uuid(),
  ingestSequence: nonnegativeSafeIntegerSchema,
  role: z.literal('source'),
  kind: z.enum(['chat', 'gift', 'like', 'member', 'follow', 'system', 'unknown']),
  receivedAt: nonnegativeSafeIntegerSchema,
  sessionOffsetMs: nonnegativeSafeIntegerSchema,
  displayName: z.string().max(256).optional(),
  content: z.string().max(4096).optional(),
  numericValue: z.number().finite().optional(),
  parseStatus: z.enum(['parsed', 'unknown', 'failed']),
}).strict()

export const liveEventBatchSchema = z.object({
  sessionId: z.string().uuid(),
  emittedAt: nonnegativeSafeIntegerSchema,
  events: z.array(liveEventSchema).min(1).max(100),
}).strict()

export const recordingProgressSchema = z.object({
  roomId: z.string().uuid(),
  sessionId: z.string().uuid(),
  operationId: z.string().uuid(),
  state: z.enum(['recording', 'reconnecting']),
  elapsedMs: nonnegativeSafeIntegerSchema,
  bytesWritten: nonnegativeSafeIntegerSchema,
  bytesAvailable: z.boolean(),
  segmentCount: nonnegativeSafeIntegerSchema,
  frame: nonnegativeSafeIntegerSchema,
  restartCount: nonnegativeSafeIntegerSchema,
  fps: finiteNonnegativeNumberSchema,
  speed: finiteNonnegativeNumberSchema,
  updatedAt: nonnegativeSafeIntegerSchema,
}).strict()

export type LiveEvent = z.infer<typeof liveEventSchema>
export type LiveEventBatch = z.infer<typeof liveEventBatchSchema>
export type RecordingProgress = z.infer<typeof recordingProgressSchema>
export type RealtimeEvent = LiveEvent & { sessionId: string }

export const settingsSchema = z.object({
  version: z.number().int().positive(),
  storageRoot: z.string().min(1),
  recordingDirectory: z.string().min(1),
  recordingDirectoryConfirmed: z.boolean(),
  defaultQuality: qualitySchema,
  defaultSegmentMinutes: z.number().int().min(5).max(30),
  maxConcurrentRecordings: z.number().int().min(1).max(4),
  minimumFreeSpaceGiB: z.number().int().min(1).max(1024),
  saveDisplayNames: z.boolean(),
  automaticUpdates: z.boolean(),
}).strict()

export const roomFormSchema = z.object({
  liveId: z.string().trim().min(1, '请输入直播间标识').max(128, '直播间标识过长'),
  alias: z.string().trim().max(80, '别名不能超过 80 个字符'),
  monitorEnabled: z.boolean(),
  recordEnabled: z.boolean(),
  quality: qualitySchema,
  segmentMinutes: z.number().int().min(5, '最少 5 分钟').max(30, '最多 30 分钟'),
  cookie: z.string().max(16_384, 'Cookie 内容过长'),
})

export const settingsFormSchema = z.object({
  recordingDirectory: z.string().trim().min(1, '请输入录制目录'),
  defaultQuality: qualitySchema,
  defaultSegmentMinutes: z.number().int().min(5).max(30),
  maxConcurrentRecordings: z.number().int().min(1).max(4),
  minimumFreeSpaceGiB: z.number().int().min(1).max(1024),
  saveDisplayNames: z.boolean(),
  automaticUpdates: z.boolean(),
})

export const updateStatusSchema = z.object({
  version: z.literal(1),
  state: z.enum(['disabled', 'idle', 'checking', 'available', 'downloading', 'ready', 'installing', 'failed']),
  currentVersion: z.string().min(1).max(64),
  availableVersion: z.string().max(64).optional(),
  publishedAt: z.string().max(64).optional(),
  releaseNotes: z.string().max(8192).optional(),
  downloadedBytes: nonnegativeSafeIntegerSchema.optional(),
  totalBytes: nonnegativeSafeIntegerSchema.optional(),
  checkedAt: nonnegativeSafeIntegerSchema.optional(),
  installBlocked: z.boolean(),
  blockReason: z.string().max(128).optional(),
  errorCode: z.string().regex(/^[A-Z0-9_]{1,64}$/).optional(),
}).strict()

export type BootstrapDTO = z.infer<typeof bootstrapSchema>
export type DataStatusDTO = z.infer<typeof dataStatusSchema>
export type RoomConfig = z.infer<typeof roomSchema>
export type RoomRuntimeStatus = z.infer<typeof roomStatusSchema>
export type AppSettings = z.infer<typeof settingsSchema>
export type RoomFormValues = z.infer<typeof roomFormSchema>
export type SettingsFormValues = z.infer<typeof settingsFormSchema>
export type UpdateStatus = z.infer<typeof updateStatusSchema>

export function contractError(contract: string, value: unknown): Error {
  const type = Array.isArray(value) ? 'array' : typeof value
  return new Error(`UI_CONTRACT_INVALID: ${contract} payload (${type})`)
}
