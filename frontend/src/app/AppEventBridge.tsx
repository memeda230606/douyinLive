import { useEffect } from 'react'

import { EventsOn, LogError } from '../generated/wailsjs/runtime/runtime'
import { roomStatusSchema } from '../lib/contracts'
import { useRoomStatusStore } from './roomStatus'

export function AppEventBridge() {
  const update = useRoomStatusStore((state) => state.update)

  useEffect(() => EventsOn('room:status', (payload: unknown) => {
    const parsed = roomStatusSchema.safeParse(payload)
    if (!parsed.success) {
      LogError('UI_CONTRACT_INVALID: room:status payload')
      return
    }
    update(parsed.data)
  }), [update])

  return null
}
