import { describe, expect, it } from 'vitest'

import {
  bootstrapSchema, liveEventBatchSchema, recordingProfileSchema, recordingProgressSchema,
  recordingStatuses, roomFormSchema, roomSchema, roomStatusSchema, settingsFormSchema,
} from './contracts'

const room = {
  id: '019bce70-0c00-7000-8000-000000000001',
  liveId: '123456',
  alias: '测试房间',
  monitorEnabled: true,
  recordEnabled: true,
  recordingProfile: { quality: 'auto', segmentMinutes: 10 },
  cookie: { configured: true, updatedAt: 1 },
  createdAt: 1,
  updatedAt: 1,
}

describe('desktop runtime contracts', () => {
  it('accepts the shared asynchronous shutdown state', () => {
    expect(bootstrapSchema.safeParse({
      apiVersion: 'v1',
      name: 'test',
      version: 'test',
      state: 'STOPPING',
      data: { ready: false, schemaVersion: 0, mode: '', loggingReady: false },
      capabilities: [],
    }).success).toBe(true)
  })

  it('accepts sanitized room DTOs but rejects credential fields', () => {
    expect(roomSchema.safeParse(room).success).toBe(true)
    expect(roomSchema.safeParse({ ...room, credentialRef: 'secret-ref' }).success).toBe(false)
    expect(roomSchema.safeParse({ ...room, cookie: { configured: true, value: 'secret' } }).success).toBe(false)
  })

  it('accepts a sanitized status and rejects unknown states or stream URLs', () => {
    const status = {
      roomId: room.id, liveId: room.liveId, alias: room.alias,
      state: 'WAITING', revision: 1, changedAt: 1, message: '等待开播',
    }
    expect(roomStatusSchema.safeParse(status).success).toBe(true)
    expect(roomStatusSchema.safeParse({
      ...status,
      state: 'RECORDING',
      sessionId: '019bce70-0c00-7000-8000-000000000002',
      recordingStatus: 'recording',
    }).success).toBe(true)
    expect(roomStatusSchema.safeParse({
      ...status,
      state: 'FINALIZING',
      sessionId: '019bce70-0c00-7000-8000-000000000002',
      recordingStatus: 'finalizing',
    }).success).toBe(true)
    recordingStatuses.forEach((recordingStatus) => {
      expect(roomStatusSchema.safeParse({
        ...status,
        state: 'RECORDING',
        sessionId: '019bce70-0c00-7000-8000-000000000002',
        recordingStatus,
      }).success).toBe(true)
    })
    expect(roomStatusSchema.safeParse({ ...status, state: 'UNKNOWN' }).success).toBe(false)
    expect(roomStatusSchema.safeParse({ ...status, recordingStatus: 'unknown' }).success).toBe(false)
    expect(roomStatusSchema.safeParse({ ...status, sessionId: 'not-a-uuid' }).success).toBe(false)
    expect(roomStatusSchema.safeParse({ ...status, unexpected: true }).success).toBe(false)
    expect(roomStatusSchema.safeParse({ ...status, revision: undefined }).success).toBe(false)
    expect(roomStatusSchema.safeParse({ ...status, revision: -1 }).success).toBe(false)
    expect(roomStatusSchema.safeParse({ ...status, revision: 1.5 }).success).toBe(false)
    expect(roomStatusSchema.safeParse({ ...status, revision: Number.MAX_SAFE_INTEGER + 1 }).success).toBe(false)
    expect(roomStatusSchema.safeParse({ ...status, streamUrl: 'https://example.invalid/live.flv' }).success).toBe(false)
  })

  it('enforces the 5 to 30 minute recording segment contract', () => {
    for (const segmentMinutes of [5, 30]) {
      expect(recordingProfileSchema.safeParse({ quality: 'auto', segmentMinutes }).success).toBe(true)
      expect(roomFormSchema.safeParse({
        liveId: 'room', alias: '', monitorEnabled: true, recordEnabled: true,
        quality: 'auto', segmentMinutes, cookie: '',
      }).success).toBe(true)
      expect(settingsFormSchema.safeParse({
        recordingDirectory: 'D:\\recordings', defaultQuality: 'auto', defaultSegmentMinutes: segmentMinutes,
        maxConcurrentRecordings: 1, minimumFreeSpaceGiB: 10, saveDisplayNames: true,
      }).success).toBe(true)
    }
    for (const segmentMinutes of [4, 31]) {
      expect(recordingProfileSchema.safeParse({ quality: 'auto', segmentMinutes }).success).toBe(true)
      expect(roomFormSchema.safeParse({
        liveId: 'room', alias: '', monitorEnabled: true, recordEnabled: true,
        quality: 'auto', segmentMinutes, cookie: '',
      }).success).toBe(false)
      expect(settingsFormSchema.safeParse({
        recordingDirectory: 'D:\\recordings', defaultQuality: 'auto', defaultSegmentMinutes: segmentMinutes,
        maxConcurrentRecordings: 1, minimumFreeSpaceGiB: 10, saveDisplayNames: true,
      }).success).toBe(false)
    }
    for (const segmentMinutes of [0, 61]) {
      expect(recordingProfileSchema.safeParse({ quality: 'auto', segmentMinutes }).success).toBe(false)
    }
  })

  it('strictly bounds live event batches and rejects private fields', () => {
    const event = (index: number) => ({
      id: `019bce70-0c00-7000-8000-${index.toString(16).padStart(12, '0')}`,
      ingestSequence: index,
      role: 'source',
      kind: index === 1 ? 'unknown' : 'chat',
      receivedAt: index,
      sessionOffsetMs: index,
      displayName: '观众',
      content: '消息',
      numericValue: 1,
      parseStatus: index === 1 ? 'unknown' : 'parsed',
    })
    const batch = {
      sessionId: '019bce70-0c00-7000-8000-000000000100',
      emittedAt: 100,
      events: Array.from({ length: 100 }, (_, index) => event(index + 1)),
    }
    expect(liveEventBatchSchema.safeParse(batch).success).toBe(true)
    expect(liveEventBatchSchema.safeParse({ ...batch, events: [...batch.events, event(101)] }).success).toBe(false)
    expect(liveEventBatchSchema.safeParse({ ...batch, streamUrl: 'https://example.invalid/live.flv' }).success).toBe(false)
    expect(liveEventBatchSchema.safeParse({ ...batch, events: [{ ...event(1), raw: 'secret' }] }).success).toBe(false)
    expect(liveEventBatchSchema.safeParse({ ...batch, events: [{ ...event(1), content: 'x'.repeat(4097) }] }).success).toBe(false)
    expect(liveEventBatchSchema.safeParse({ ...batch, events: [] }).success).toBe(false)
  })

  it('validates finite nonnegative recording progress without extra fields', () => {
    const progress = {
      roomId: room.id,
      sessionId: '019bce70-0c00-7000-8000-000000000002',
      operationId: '019bce70-0c00-7000-8000-000000000003',
      state: 'recording',
      elapsedMs: 1_000,
      bytesWritten: 2_000,
      segmentCount: 1,
      frame: 25,
      restartCount: 0,
      fps: 25,
      speed: 1,
      updatedAt: 2,
    }
    expect(recordingProgressSchema.safeParse(progress).success).toBe(true)
    expect(recordingProgressSchema.safeParse({ ...progress, state: 'reconnecting' }).success).toBe(true)
    expect(recordingProgressSchema.safeParse({ ...progress, speed: Number.POSITIVE_INFINITY }).success).toBe(false)
    expect(recordingProgressSchema.safeParse({ ...progress, frame: -1 }).success).toBe(false)
    expect(recordingProgressSchema.safeParse({ ...progress, attemptId: 'private' }).success).toBe(false)
    expect(recordingProgressSchema.safeParse({ ...progress, state: 'finalizing' }).success).toBe(false)
    expect(recordingProgressSchema.safeParse({ ...progress, state: 'unavailable' }).success).toBe(false)
  })
})
