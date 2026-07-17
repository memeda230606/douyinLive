import { useQuery } from '@tanstack/react-query'
import { useEffect } from 'react'

import { useRoomStatusStore } from '../../app/roomStatus'
import { getRoomStatus, listRooms } from '../../lib/desktop'

export function useRoomsDashboard() {
  const roomsQuery = useQuery({ queryKey: ['rooms'], queryFn: listRooms, retry: false })
  const statuses = useRoomStatusStore((state) => state.byRoom)
  const update = useRoomStatusStore((state) => state.update)

  useEffect(() => {
    let active = true
    const rooms = roomsQuery.data ?? []
    void Promise.allSettled(rooms.map(async (room) => {
      const status = await getRoomStatus(room.id)
      if (active) update(status)
    }))
    return () => { active = false }
  }, [roomsQuery.data, update])

  return { roomsQuery, statuses }
}

export type RoomsDashboard = ReturnType<typeof useRoomsDashboard>
