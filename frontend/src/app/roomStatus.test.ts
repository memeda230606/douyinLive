import { beforeEach, describe, expect, it } from 'vitest'

import type { RoomRuntimeStatus } from '../lib/contracts'
import { useRoomStatusStore } from './roomStatus'

const roomId = '019bce70-0c00-7000-8000-000000000001'

function status(overrides: Partial<RoomRuntimeStatus> = {}): RoomRuntimeStatus {
  return {
    roomId,
    liveId: '100000000001',
    alias: '测试直播间',
    state: 'WAITING',
    revision: 1,
    changedAt: 100,
    message: '等待开播',
    ...overrides,
  }
}

describe('room status store', () => {
  beforeEach(() => useRoomStatusStore.setState({ byRoom: {} }))

  it('uses revision as the sole ordering fence when timestamps collide or regress', () => {
    useRoomStatusStore.getState().update(status({ state: 'RECORDING', revision: 2 }))
    useRoomStatusStore.getState().update(status({ state: 'ERROR', revision: 1 }))
    expect(useRoomStatusStore.getState().byRoom[roomId]).toMatchObject({
      state: 'RECORDING', revision: 2, changedAt: 100,
    })

    useRoomStatusStore.getState().update(status({ state: 'ERROR', revision: 2, message: '重复修订' }))
    expect(useRoomStatusStore.getState().byRoom[roomId]?.state).toBe('RECORDING')

    useRoomStatusStore.getState().update(status({ state: 'LIVE', revision: 3, changedAt: 50 }))
    expect(useRoomStatusStore.getState().byRoom[roomId]).toMatchObject({
      state: 'LIVE', revision: 3, changedAt: 50,
    })
  })
})
