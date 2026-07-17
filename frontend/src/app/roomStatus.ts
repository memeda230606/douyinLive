import { create } from 'zustand'

import type { RoomRuntimeStatus } from '../lib/contracts'

type RoomStatusState = {
  byRoom: Record<string, RoomRuntimeStatus>
  update: (status: RoomRuntimeStatus) => void
  remove: (roomId: string) => void
}

export const useRoomStatusStore = create<RoomStatusState>((set) => ({
  byRoom: {},
  update: (status) => set((state) => ({ byRoom: { ...state.byRoom, [status.roomId]: status } })),
  remove: (roomId) => set((state) => {
    const byRoom = { ...state.byRoom }
    delete byRoom[roomId]
    return { byRoom }
  }),
}))
