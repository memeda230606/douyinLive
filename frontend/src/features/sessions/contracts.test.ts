import { describe, expect, it } from 'vitest'

import { mediaPageSchema, sessionPageSchema } from './contracts'

const sessionId = '019aa000-0000-7000-8000-000000000001'
const roomId = '019aa000-0000-7000-8000-000000000002'

describe('playback contracts', () => {
  it('accepts the allowlisted session contract and rejects private fields', () => {
    const session = {
      id: sessionId, roomConfigId: roomId, roomAlias: '直播间', title: '场次',
      status: 'completed', recordingStatus: 'completed', startedAt: 1,
      endedAt: 2, captureOffsetMs: 0, clockSource: 'media', integrityScore: 1,
      sessionMediaState: 'completed',
    }
    expect(sessionPageSchema.safeParse({ version: 1, items: [session] }).success).toBe(true)
    expect(sessionPageSchema.safeParse({ version: 1, items: [{ ...session, relativePath: 'private/media' }] }).success).toBe(false)
  })

  it('rejects media paths and digests even when the visible media shape is valid', () => {
    const segment = {
      id: sessionId, sequence: 1, container: 'mkv', videoCodec: 'h264', audioCodec: 'aac',
      startedAt: 1, endedAt: 2, durationMs: 1, sizeBytes: 1, status: 'complete',
      timelineStartMs: 0, timelineEndMs: 1, artifacts: [],
    }
    expect(mediaPageSchema.safeParse({ version: 1, items: [segment] }).success).toBe(true)
    expect(mediaPageSchema.safeParse({ version: 1, items: [{ ...segment, sha256: 'a'.repeat(64) }] }).success).toBe(false)
  })
})
