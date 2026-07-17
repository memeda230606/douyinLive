import { describe, expect, it } from 'vitest'

import { bootstrapSchema, recordingProfileSchema, recordingStatuses, roomFormSchema, roomSchema, roomStatusSchema, settingsFormSchema } from './contracts'

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
      state: 'WAITING', changedAt: 1, message: '等待开播',
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
})
