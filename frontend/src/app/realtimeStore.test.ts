import { beforeEach, describe, expect, it } from 'vitest'

import type { LiveEventBatch, RecordingProgress, RoomRuntimeStatus } from '../lib/contracts'
import { maximumRealtimeAlerts, maximumRealtimeEvents, useRealtimeStore } from './realtimeStore'

const sessionId = '019bce70-0c00-7000-8000-000000000001'
const roomId = '019bce70-0c00-7000-8000-000000000002'
const operationId = '019bce70-0c00-7000-8000-000000000003'

function event(index: number) {
  return {
    id: `019bce70-0c00-7000-8000-${index.toString(16).padStart(12, '0')}`,
    ingestSequence: index,
    role: 'source' as const,
    kind: 'chat' as const,
    receivedAt: index,
    sessionOffsetMs: index,
    content: `消息 ${index}`,
    parseStatus: 'parsed' as const,
  }
}

function batch(events: ReturnType<typeof event>[]): LiveEventBatch {
  return { sessionId, emittedAt: events.at(-1)?.receivedAt ?? 0, events }
}

function progress(overrides: Partial<RecordingProgress> = {}): RecordingProgress {
  return {
    roomId, sessionId, operationId, state: 'recording', elapsedMs: 1_000,
    bytesWritten: 2_000, bytesAvailable: true, segmentCount: 1, frame: 25, restartCount: 0,
    fps: 25, speed: 1, updatedAt: 2, ...overrides,
  }
}

function status(overrides: Partial<RoomRuntimeStatus> = {}): RoomRuntimeStatus {
  return {
    roomId, liveId: '123', alias: '测试房间', state: 'WAITING',
    revision: 1, changedAt: 1, message: '等待开播', ...overrides,
  }
}

describe('realtime store', () => {
  beforeEach(() => useRealtimeStore.getState().reset())

  it('deduplicates globally, orders late batches and evicts IDs with their events', () => {
    useRealtimeStore.getState().mergeLiveEventBatch(batch([event(2), event(3)]))
    useRealtimeStore.getState().mergeLiveEventBatch(batch([event(1), event(2)]))
    expect(useRealtimeStore.getState().events.map((item) => item.ingestSequence)).toEqual([1, 2, 3])

    for (let start = 4; start <= 2_104; start += 100) {
      useRealtimeStore.getState().mergeLiveEventBatch(batch(
        Array.from({ length: Math.min(100, 2_104 - start + 1) }, (_, offset) => event(start + offset)),
      ))
    }
    const state = useRealtimeStore.getState()
    expect(state.events).toHaveLength(maximumRealtimeEvents)
    expect(Object.keys(state.eventIds)).toHaveLength(maximumRealtimeEvents)
    expect(state.events[0]?.ingestSequence).toBe(105)
    expect(state.eventIds[event(1).id]).toBeUndefined()
    expect(state.eventIds[event(2_104).id]).toBe(true)
  })

  it('fences progress by room, session and operation before applying updatedAt', () => {
    useRealtimeStore.getState().applyRecordingProgress(progress({ updatedAt: 10 }))
    useRealtimeStore.getState().applyRecordingProgress(progress({ updatedAt: 9, bytesWritten: 9_999 }))
    useRealtimeStore.getState().applyRecordingProgress(progress({
      operationId: '019bce70-0c00-7000-8000-000000000004', updatedAt: 99, bytesWritten: 8_888,
    }))
    expect(useRealtimeStore.getState().progressBySession[sessionId]?.bytesWritten).toBe(2_000)

    const nextOperationId = '019bce70-0c00-7000-8000-000000000004'
    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'RECORDING', recordingStatus: 'recording', sessionId,
      operationId: nextOperationId, revision: 2, changedAt: 3,
    }))
    expect(useRealtimeStore.getState().progressBySession[sessionId]).toBeUndefined()
    useRealtimeStore.getState().applyRecordingProgress(progress({
      operationId: nextOperationId, updatedAt: 1, bytesWritten: 3_000,
    }))
    expect(useRealtimeStore.getState().progressBySession[sessionId]?.bytesWritten).toBe(3_000)

    useRealtimeStore.getState().applyRecordingProgress(progress({ updatedAt: 100, bytesWritten: 100_000 }))
    expect(useRealtimeStore.getState().progressBySession[sessionId]).toMatchObject({
      operationId: nextOperationId, updatedAt: 1, bytesWritten: 3_000,
    })
  })

  it('purges mismatched progress while retaining matching reconnect and exhausted metrics', () => {
    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'RECORDING', recordingStatus: 'recording', sessionId, operationId, revision: 1,
    }))
    useRealtimeStore.getState().applyRecordingProgress(progress())
    expect(useRealtimeStore.getState().progressBySession[sessionId]).toBeDefined()

    const nextOperationId = '019bce70-0c00-7000-8000-000000000004'
    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'RECORDING', recordingStatus: 'recording', sessionId,
      operationId: nextOperationId, revision: 2,
    }))
    expect(useRealtimeStore.getState().progressBySession[sessionId]).toBeUndefined()

    useRealtimeStore.getState().applyRecordingProgress(progress({ operationId: nextOperationId }))
    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'RECONNECTING', recordingStatus: 'reconnecting', sessionId,
      operationId: nextOperationId, revision: 3,
    }))
    expect(useRealtimeStore.getState().progressBySession[sessionId]).toBeDefined()
    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'ERROR', recordingStatus: 'unavailable', sessionId,
      operationId: nextOperationId, revision: 4,
    }))
    expect(useRealtimeStore.getState().progressBySession[sessionId]).toBeDefined()

    useRealtimeStore.getState().applyRoomStatus(status({ state: 'WAITING', revision: 5 }))
    expect(useRealtimeStore.getState().progressBySession[sessionId]).toBeUndefined()
  })

  it('closes a reconnect gap when LIVE resumes unless recording is disabled or unavailable', () => {
    useRealtimeStore.getState().applyRoomStatus(status({ state: 'RECONNECTING', revision: 2, changedAt: 2 }))
    useRealtimeStore.getState().applyRoomStatus(status({ state: 'LIVE', recordingStatus: 'disabled', revision: 3, changedAt: 3 }))
    expect(useRealtimeStore.getState().alerts.map((alert) => alert.kind)).toEqual(['gap_open'])
    useRealtimeStore.getState().applyRoomStatus(status({ state: 'RECONNECTING', revision: 4, changedAt: 4 }))
    useRealtimeStore.getState().applyRoomStatus(status({ state: 'LIVE', revision: 5, changedAt: 5 }))
    expect(useRealtimeStore.getState().alerts.at(-1)?.kind).toBe('gap_recovered')
  })

  it('derives bounded, sanitized gap and error alerts without accepting stale status', () => {
    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'RECONNECTING', sessionId, operationId, revision: 2, changedAt: 2,
      errorCode: 'RECORDER_NETWORK_FAILURE', message: 'private backend detail',
    }))
    useRealtimeStore.getState().applyRoomStatus(status({ revision: 1, changedAt: 2, state: 'ERROR', message: 'stale' }))
    expect(useRealtimeStore.getState().latestRoomStatus[roomId]?.state).toBe('RECONNECTING')
    expect(useRealtimeStore.getState().alerts[0]).toMatchObject({
      kind: 'gap_open', severity: 'warning', errorCode: 'RECORDER_NETWORK_FAILURE',
    })
    expect(useRealtimeStore.getState().alerts[0]?.message).not.toContain('private')

    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'RECORDING', recordingStatus: 'recording', sessionId, operationId, revision: 3, changedAt: 3,
    }))
    expect(useRealtimeStore.getState().alerts.at(-1)?.kind).toBe('gap_recovered')

    for (let index = 0; index < 60; index += 1) {
      useRealtimeStore.getState().applyRoomStatus(status({
        state: 'RECONNECTING', sessionId, operationId,
        revision: 4 + index * 2, changedAt: 4 + index * 2,
      }))
      useRealtimeStore.getState().applyRoomStatus(status({
        state: 'RECORDING', recordingStatus: 'recording', sessionId, operationId,
        revision: 5 + index * 2, changedAt: 5 + index * 2,
      }))
    }
    expect(useRealtimeStore.getState().alerts).toHaveLength(maximumRealtimeAlerts)
  })

  it('does not mislabel a room-only ERROR as a recording failure', () => {
    useRealtimeStore.getState().applyRoomStatus(status({ state: 'ERROR', revision: 1 }))
    expect(useRealtimeStore.getState().alerts).toEqual([])
    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'ERROR', recordingStatus: 'disabled', sessionId, operationId, revision: 2,
    }))
    expect(useRealtimeStore.getState().alerts).toEqual([])
    useRealtimeStore.getState().applyRoomStatus(status({
      state: 'ERROR', recordingStatus: 'unavailable', sessionId, operationId, revision: 3,
    }))
    expect(useRealtimeStore.getState().alerts.at(-1)?.kind).toBe('recording_error')
  })
})
