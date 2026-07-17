import { render } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { EventsOn, LogError } from '../generated/wailsjs/runtime/runtime'
import { AppEventBridge } from './AppEventBridge'
import { useRoomStatusStore } from './roomStatus'

vi.mock('../generated/wailsjs/runtime/runtime', () => ({
  EventsOn: vi.fn(),
  LogError: vi.fn(),
}))

describe('AppEventBridge', () => {
  beforeEach(() => {
    useRoomStatusStore.setState({ byRoom: {} })
    vi.mocked(EventsOn).mockReset()
    vi.mocked(LogError).mockReset()
  })

  it('registers once, validates events and unregisters on unmount', () => {
    let callback: ((payload: unknown) => void) | undefined
    const off = vi.fn()
    vi.mocked(EventsOn).mockImplementation((_name, listener) => { callback = listener; return off })
    const view = render(<AppEventBridge />)
    expect(EventsOn).toHaveBeenCalledTimes(1)

    callback?.({
      roomId: '019bce70-0c00-7000-8000-000000000002', liveId: '123', alias: '房间',
      state: 'LIVE', changedAt: 2, message: '直播中',
    })
    expect(useRoomStatusStore.getState().byRoom['019bce70-0c00-7000-8000-000000000002']?.state).toBe('LIVE')

    callback?.({ state: 'LIVE', cookie: 'secret' })
    expect(LogError).toHaveBeenCalledWith('UI_CONTRACT_INVALID: room:status payload')
    view.unmount()
    expect(off).toHaveBeenCalledTimes(1)
  })
})
