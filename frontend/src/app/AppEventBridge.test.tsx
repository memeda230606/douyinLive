import { render } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { EventsOn, LogError } from '../generated/wailsjs/runtime/runtime'
import { AppEventBridge } from './AppEventBridge'
import { useRealtimeStore } from './realtimeStore'
import { useRoomStatusStore } from './roomStatus'

vi.mock('../generated/wailsjs/runtime/runtime', () => ({
  EventsOn: vi.fn(),
  LogError: vi.fn(),
}))

describe('AppEventBridge', () => {
  beforeEach(() => {
    useRoomStatusStore.setState({ byRoom: {} })
    useRealtimeStore.getState().reset()
    vi.mocked(EventsOn).mockReset()
    vi.mocked(LogError).mockReset()
  })

  it('registers the single bridge, validates all payloads and cleans up every listener', () => {
    const callbacks = new Map<string, (payload: unknown) => void>()
    const offs = new Map<string, ReturnType<typeof vi.fn>>()
    vi.mocked(EventsOn).mockImplementation((name, listener) => {
      callbacks.set(name, listener)
      const off = vi.fn()
      offs.set(name, off)
      return off
    })
    const view = render(<AppEventBridge />)
    expect(EventsOn).toHaveBeenCalledTimes(3)
    expect([...callbacks.keys()]).toEqual(['room:status', 'live:event', 'recording:progress'])

    callbacks.get('room:status')?.({
      roomId: '019bce70-0c00-7000-8000-000000000002', liveId: '123', alias: '房间',
      state: 'RECORDING', recordingStatus: 'recording',
      sessionId: '019bce70-0c00-7000-8000-000000000003',
      operationId: '019bce70-0c00-7000-8000-000000000004',
      revision: 2, changedAt: 2, message: '录制中',
    })
    expect(useRoomStatusStore.getState().byRoom['019bce70-0c00-7000-8000-000000000002']?.state).toBe('RECORDING')

    callbacks.get('live:event')?.({
      sessionId: '019bce70-0c00-7000-8000-000000000003', emittedAt: 3,
      events: [{
        id: '019bce70-0c00-7000-8000-000000000005', ingestSequence: 1,
        role: 'source', kind: 'chat', receivedAt: 3, sessionOffsetMs: 1,
        content: '消息', parseStatus: 'parsed',
      }],
    })
    expect(useRealtimeStore.getState().events).toHaveLength(1)

    callbacks.get('recording:progress')?.({
      roomId: '019bce70-0c00-7000-8000-000000000002',
      sessionId: '019bce70-0c00-7000-8000-000000000003',
      operationId: '019bce70-0c00-7000-8000-000000000004',
      state: 'recording', elapsedMs: 1_000, bytesWritten: 2_000,
      segmentCount: 1, frame: 25, restartCount: 0, fps: 25, speed: 1, updatedAt: 4,
    })
    expect(useRealtimeStore.getState().progressBySession['019bce70-0c00-7000-8000-000000000003']?.frame).toBe(25)

    callbacks.get('room:status')?.({ state: 'LIVE', cookie: 'secret-room' })
    callbacks.get('live:event')?.({ cookie: 'secret-event' })
    callbacks.get('recording:progress')?.({ streamUrl: 'secret-progress' })
    expect(LogError).toHaveBeenNthCalledWith(1, 'UI_CONTRACT_INVALID: room:status payload')
    expect(LogError).toHaveBeenNthCalledWith(2, 'UI_CONTRACT_INVALID: live:event payload')
    expect(LogError).toHaveBeenNthCalledWith(3, 'UI_CONTRACT_INVALID: recording:progress payload')
    expect(vi.mocked(LogError).mock.calls.flat().join(' ')).not.toContain('secret')

    view.unmount()
    expect([...offs.values()]).toHaveLength(3)
    offs.forEach((off) => expect(off).toHaveBeenCalledTimes(1))
  })
})
