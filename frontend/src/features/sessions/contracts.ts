import { z } from 'zod'

const safeInteger = z.number().int().min(Number.MIN_SAFE_INTEGER).max(Number.MAX_SAFE_INTEGER)
const nonnegativeInteger = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER)
const uuid = z.string().uuid()

export const sessionSchema = z.object({
  id: uuid,
  roomConfigId: uuid,
  roomAlias: z.string().max(256),
  title: z.string().max(4096),
  status: z.enum(['starting', 'recording', 'finalizing', 'completed', 'interrupted', 'failed']),
  recordingStatus: z.enum(['pending', 'disabled', 'starting', 'recording', 'unavailable', 'reconnecting', 'finalizing', 'completed', 'incomplete', 'failed']),
  startedAt: nonnegativeInteger,
  endedAt: nonnegativeInteger.optional(),
  mediaEpochAt: nonnegativeInteger.optional(),
  captureOffsetMs: safeInteger,
  clockSource: z.string().max(64),
  integrityScore: z.number().finite().min(0).max(1),
  sessionMediaState: z.enum(['open', 'finalizing', 'completed', 'incomplete']).optional(),
}).strict()

export const sessionResultSchema = z.object({ version: z.literal(1), session: sessionSchema }).strict()
export const sessionPageSchema = z.object({
  version: z.literal(1), items: z.array(sessionSchema).max(100), nextCursor: z.string().max(2048).optional(),
}).strict()

export const eventSchema = z.object({
  id: uuid,
  ingestSequence: nonnegativeInteger,
  role: z.enum(['source', 'aggregate']),
  kind: z.enum(['chat', 'gift', 'like', 'member', 'follow', 'system', 'unknown']),
  receivedAt: nonnegativeInteger,
  sessionOffsetMs: nonnegativeInteger,
  clockConfidence: z.number().finite().min(0).max(1),
  displayName: z.string().max(256).optional(),
  content: z.string().max(4096).optional(),
  numericValue: z.number().finite().optional(),
  parseStatus: z.enum(['parsed', 'unknown', 'failed']),
}).strict()

export const eventPageSchema = z.object({
  version: z.literal(1), items: z.array(eventSchema).max(100), nextCursor: z.string().max(2048).optional(),
}).strict()

export const gapSchema = z.object({
  id: uuid,
  kind: z.enum(['message_disconnect', 'recording_restart', 'stream_unavailable', 'disk_full', 'process_crash', 'clock_uncertain', 'event_persistence']),
  startedAt: nonnegativeInteger,
  endedAt: nonnegativeInteger.optional(),
  startOffsetMs: nonnegativeInteger,
  endOffsetMs: nonnegativeInteger.optional(),
  severity: z.string().min(1).max(64),
  recovered: z.boolean(),
  reasonCode: z.string().min(1).max(128),
}).strict()

export const gapPageSchema = z.object({
  version: z.literal(1), items: z.array(gapSchema).max(100), nextCursor: z.string().max(2048).optional(),
}).strict()

export const mediaArtifactSchema = z.object({
  id: uuid,
  mediaSegmentId: uuid,
  kind: z.enum(['asr_wav', 'playback_mp4']),
  container: z.string().max(32),
  codec: z.string().max(64),
  durationMs: nonnegativeInteger,
  sizeBytes: nonnegativeInteger,
  sampleRate: nonnegativeInteger,
  channels: nonnegativeInteger,
  status: z.enum(['pending', 'pending_transcode', 'complete', 'failed', 'missing', 'not_applicable']),
  errorCode: z.string().max(128).optional(),
  directPlayback: z.boolean(),
}).strict()

export const mediaSegmentSchema: z.ZodType<MediaSegment> = z.object({
  id: uuid,
  sequence: nonnegativeInteger,
  container: z.string().max(32),
  videoCodec: z.string().max(64).optional(),
  audioCodec: z.string().max(64).optional(),
  startedAt: nonnegativeInteger,
  endedAt: nonnegativeInteger,
  ptsStartMs: safeInteger.optional(),
  ptsEndMs: safeInteger.optional(),
  durationMs: nonnegativeInteger,
  sizeBytes: nonnegativeInteger,
  status: z.enum(['partial', 'complete', 'recovered', 'corrupt', 'missing']),
  errorCode: z.string().max(128).optional(),
  timelineStartMs: safeInteger,
  timelineEndMs: safeInteger,
  artifacts: z.array(mediaArtifactSchema).max(2),
  playbackArtifactId: uuid.optional(),
}).strict()

export const mediaPageSchema = z.object({
  version: z.literal(1), items: z.array(mediaSegmentSchema).max(100), nextCursor: z.string().max(2048).optional(),
}).strict()

export const mediaLocationSchema = z.object({
  version: z.literal(1),
  sessionId: uuid,
  requestedOffsetMs: nonnegativeInteger,
  adjustedOffsetMs: safeInteger,
  state: z.enum(['playback_mp4', 'source_mkv', 'gap']),
  reasonCode: z.string().max(128).optional(),
  segment: mediaSegmentSchema.optional(),
  segmentPlaybackMs: nonnegativeInteger.optional(),
  playbackArtifactId: uuid.optional(),
}).strict()

export type PlaybackSession = z.infer<typeof sessionSchema>
export type PlaybackEvent = z.infer<typeof eventSchema>
export type PlaybackGap = z.infer<typeof gapSchema>
export type MediaArtifact = z.infer<typeof mediaArtifactSchema>
export type MediaSegment = {
  id: string
  sequence: number
  container: string
  videoCodec?: string
  audioCodec?: string
  startedAt: number
  endedAt: number
  ptsStartMs?: number
  ptsEndMs?: number
  durationMs: number
  sizeBytes: number
  status: 'partial' | 'complete' | 'recovered' | 'corrupt' | 'missing'
  errorCode?: string
  timelineStartMs: number
  timelineEndMs: number
  artifacts: MediaArtifact[]
  playbackArtifactId?: string
}
export type MediaLocation = z.infer<typeof mediaLocationSchema>
