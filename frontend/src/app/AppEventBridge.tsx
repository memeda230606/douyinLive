import { useEffect } from 'react'

import { EventsOn, LogError } from '../generated/wailsjs/runtime/runtime'
import { liveEventBatchSchema, recordingProgressSchema, roomStatusSchema, updateStatusSchema } from '../lib/contracts'
import { useRealtimeStore } from './realtimeStore'
import { useRoomStatusStore } from './roomStatus'
import { useUpdateStore } from './updateStore'

export function AppEventBridge() {
  useEffect(() => {
    const offRoomStatus = EventsOn('room:status', (payload: unknown) => {
      const parsed = roomStatusSchema.safeParse(payload)
      if (!parsed.success) {
        LogError('UI_CONTRACT_INVALID: room:status payload')
        return
      }
      useRoomStatusStore.getState().update(parsed.data)
      useRealtimeStore.getState().applyRoomStatus(parsed.data)
    })
    const offLiveEvent = EventsOn('live:event', (payload: unknown) => {
      const parsed = liveEventBatchSchema.safeParse(payload)
      if (!parsed.success) {
        LogError('UI_CONTRACT_INVALID: live:event payload')
        return
      }
      useRealtimeStore.getState().mergeLiveEventBatch(parsed.data)
    })
    const offRecordingProgress = EventsOn('recording:progress', (payload: unknown) => {
      const parsed = recordingProgressSchema.safeParse(payload)
      if (!parsed.success) {
        LogError('UI_CONTRACT_INVALID: recording:progress payload')
        return
      }
      useRealtimeStore.getState().applyRecordingProgress(parsed.data)
    })
    const offUpdateStatus = EventsOn('update:status', (payload: unknown) => {
      const parsed = updateStatusSchema.safeParse(payload)
      if (!parsed.success) {
        LogError('UI_CONTRACT_INVALID: update:status payload')
        return
      }
      useUpdateStore.getState().setStatus(parsed.data)
    })

    return () => {
      offRoomStatus()
      offLiveEvent()
      offRecordingProgress()
      offUpdateStatus()
    }
  }, [])

  return null
}
