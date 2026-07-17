import { z } from 'zod'

const qualitySchema = z.enum(['auto', 'original', 'ultra', 'high', 'standard'])

export const dataStatusSchema = z.object({
  ready: z.boolean(),
  schemaVersion: z.number().int().nonnegative(),
  mode: z.string(),
  loggingReady: z.boolean(),
}).strict()

export const bootstrapSchema = z.object({
  apiVersion: z.literal('v1'),
  name: z.string().min(1),
  version: z.string().min(1),
  state: z.enum(['CREATED', 'RUNNING', 'STOPPING', 'STOPPED']),
  data: dataStatusSchema,
  capabilities: z.array(z.object({
    id: z.string().min(1),
    label: z.string().min(1),
    available: z.boolean(),
  }).strict()),
}).strict()

const cookieStatusSchema = z.object({
  configured: z.boolean(),
  updatedAt: z.number().int().nonnegative().optional(),
}).strict()

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

export const settingsSchema = z.object({
  version: z.number().int().positive(),
  storageRoot: z.string().min(1),
  recordingDirectory: z.string().min(1),
  defaultQuality: qualitySchema,
  defaultSegmentMinutes: z.number().int().min(1).max(60),
  maxConcurrentRecordings: z.number().int().min(1).max(4),
  minimumFreeSpaceGiB: z.number().int().min(1).max(1024),
  saveDisplayNames: z.boolean(),
}).strict()

export const roomFormSchema = z.object({
  liveId: z.string().trim().min(1, '请输入直播间标识').max(128, '直播间标识过长'),
  alias: z.string().trim().max(80, '别名不能超过 80 个字符'),
  monitorEnabled: z.boolean(),
  recordEnabled: z.boolean(),
  quality: qualitySchema,
  segmentMinutes: z.number().int().min(1, '最少 1 分钟').max(60, '最多 60 分钟'),
  cookie: z.string().max(16_384, 'Cookie 内容过长'),
})

export const settingsFormSchema = z.object({
  recordingDirectory: z.string().trim().min(1, '请输入录制目录'),
  defaultQuality: qualitySchema,
  defaultSegmentMinutes: z.number().int().min(1).max(60),
  maxConcurrentRecordings: z.number().int().min(1).max(4),
  minimumFreeSpaceGiB: z.number().int().min(1).max(1024),
  saveDisplayNames: z.boolean(),
})

export type BootstrapDTO = z.infer<typeof bootstrapSchema>
export type DataStatusDTO = z.infer<typeof dataStatusSchema>
export type RoomConfig = z.infer<typeof roomSchema>
export type RoomRuntimeStatus = z.infer<typeof roomStatusSchema>
export type AppSettings = z.infer<typeof settingsSchema>
export type RoomFormValues = z.infer<typeof roomFormSchema>
export type SettingsFormValues = z.infer<typeof settingsFormSchema>

export function contractError(contract: string, value: unknown): Error {
  const type = Array.isArray(value) ? 'array' : typeof value
  return new Error(`UI_CONTRACT_INVALID: ${contract} payload (${type})`)
}
