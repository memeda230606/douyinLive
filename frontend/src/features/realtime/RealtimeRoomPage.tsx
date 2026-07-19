import { ArrowLeft, Radio } from 'lucide-react'
import { useMemo, useState } from 'react'

import { useRealtimeStore } from '../../app/realtimeStore'
import type { RealtimeEvent, RoomConfig, RoomRuntimeStatus } from '../../lib/contracts'
import { RoomStatusBadge } from '../rooms/RoomStatusBadge'
import { EventTimeline } from './EventTimeline'
import { RecordingPanel } from './RecordingPanel'

const filters = [
  ['chat', '聊天'], ['gift', '礼物'], ['like', '点赞'],
  ['member', '进房'], ['follow', '关注'], ['system', '系统'],
] as const

type EventFilter = typeof filters[number][0]

function matchesFilter(event: RealtimeEvent, filter?: EventFilter) {
  if (!filter) return true
  if (filter === 'system') return event.kind === 'system' || event.kind === 'unknown'
  return event.kind === filter
}

function newestStatus(primary?: RoomRuntimeStatus, secondary?: RoomRuntimeStatus) {
  if (!primary) return secondary
  if (!secondary) return primary
  return primary.revision >= secondary.revision ? primary : secondary
}

export function RealtimeRoomPage({
  onBack, onRoomChange, roomId, rooms, statuses,
}: {
  onBack: () => void
  onRoomChange: (roomId: string) => void
  roomId?: string
  rooms: RoomConfig[]
  statuses: Record<string, RoomRuntimeStatus>
}) {
  const [filter, setFilter] = useState<EventFilter>()
  const events = useRealtimeStore((state) => state.events)
  const progressBySession = useRealtimeStore((state) => state.progressBySession)
  const bridgeStatuses = useRealtimeStore((state) => state.latestRoomStatus)
  const allAlerts = useRealtimeStore((state) => state.alerts)
  const selectedRoom = rooms.find((room) => room.id === roomId) ?? rooms[0]
  const status = selectedRoom ? newestStatus(statuses[selectedRoom.id], bridgeStatuses[selectedRoom.id]) : undefined
  const sessionId = status?.sessionId

  const visibleEvents = useMemo(() => events.filter((event) => (
    Boolean(sessionId) && event.sessionId === sessionId && matchesFilter(event, filter)
  )), [events, filter, sessionId])
  const alerts = useMemo(() => allAlerts.filter((alert) => (
    alert.roomId === selectedRoom?.id && (!sessionId || !alert.sessionId || alert.sessionId === sessionId)
  )), [allAlerts, selectedRoom?.id, sessionId])
  const progressCandidate = sessionId ? progressBySession[sessionId] : undefined
  const progress = progressCandidate && selectedRoom && status?.sessionId && status.operationId &&
    progressCandidate.roomId === selectedRoom.id &&
    progressCandidate.sessionId === status.sessionId &&
    progressCandidate.operationId === status.operationId
    ? progressCandidate
    : undefined

  if (!selectedRoom) {
    return (
      <main className="page realtime-page">
        <button className="button realtime-back" type="button" onClick={onBack}><ArrowLeft aria-hidden="true" />返回直播间</button>
        <section className="empty-panel"><div><h1>没有可查看的直播间</h1><p>先添加直播间，再从房间卡片进入实时视图。</p></div></section>
      </main>
    )
  }

  return (
    <main className="page realtime-page">
      <div className="realtime-page__toolbar">
        <button className="button realtime-back" type="button" onClick={onBack}><ArrowLeft aria-hidden="true" />返回直播间</button>
        <label className="room-picker"><span>选择直播间</span><select aria-label="选择直播间" value={selectedRoom.id} onChange={(event) => onRoomChange(event.target.value)}>{rooms.map((room) => <option key={room.id} value={room.id}>{room.alias}</option>)}</select></label>
      </div>
      <div className="realtime-page__heading">
        <div className="realtime-room-title"><span className="room-avatar"><Radio aria-hidden="true" /></span><div><p className="eyebrow">Live room</p><h1>{selectedRoom.alias}</h1><p>Live ID · {selectedRoom.liveId}</p></div></div>
        <RoomStatusBadge status={status} fallback={selectedRoom.monitorEnabled ? 'WAITING' : 'STOPPED'} />
      </div>

      <div className="realtime-layout">
        <section className="timeline-panel" aria-labelledby="timeline-title">
          <div className="timeline-panel__heading">
            <div><h2 id="timeline-title">实时事件</h2><p>{sessionId ? `当前场次已保留 ${visibleEvents.length} 条匹配事件` : '等待直播场次建立后接收事件'}</p></div>
            <div className="event-filters" role="group" aria-label="事件类型筛选">
              <button
                aria-pressed={filter === undefined}
                className={filter === undefined ? 'is-active' : ''}
                type="button"
                onClick={() => setFilter(undefined)}
              >全部</button>
              {filters.map(([value, label]) => (
                <button
                  aria-pressed={filter === value}
                  className={filter === value ? 'is-active' : ''}
                  data-event-filter={value}
                  key={value}
                  type="button"
                  onClick={() => setFilter(value)}
                >{label}</button>
              ))}
            </div>
          </div>
          <EventTimeline events={visibleEvents} />
        </section>
        <RecordingPanel alerts={alerts} progress={progress} status={status} />
      </div>
    </main>
  )
}
