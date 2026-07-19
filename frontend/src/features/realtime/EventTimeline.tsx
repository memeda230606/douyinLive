import { ArrowDownToLine } from 'lucide-react'
import { useEffect, useMemo, useRef, useState, type UIEvent } from 'react'

import type { RealtimeEvent } from '../../lib/contracts'

const rowHeight = 68
const overscan = 5
const fallbackViewportHeight = 420

const eventLabels: Record<RealtimeEvent['kind'], string> = {
  chat: '聊天',
  gift: '礼物',
  like: '点赞',
  member: '进房',
  follow: '关注',
  system: '系统',
  unknown: '未知',
}

function eventDescription(event: RealtimeEvent) {
  if (event.content) return event.content
  if (event.numericValue !== undefined) {
    const prefix = event.kind === 'gift' ? '礼物数量' : event.kind === 'like' ? '点赞数量' : '数值'
    return `${prefix} ${event.numericValue}`
  }
  if (event.parseStatus === 'failed') return '事件解析失败，原始私密字段未传入界面。'
  if (event.parseStatus === 'unknown' || event.kind === 'unknown') return '收到暂不支持的事件。'
  return '收到事件。'
}

function receivedAtText(receivedAt: number) {
  const date = new Date(receivedAt)
  if (Number.isNaN(date.getTime())) return '接收时间不可用'
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  }).format(date)
}

function sessionOffsetText(sessionOffsetMs: number) {
  const hours = Math.floor(sessionOffsetMs / 3_600_000)
  const minutes = Math.floor((sessionOffsetMs % 3_600_000) / 60_000)
  const seconds = Math.floor((sessionOffsetMs % 60_000) / 1_000)
  const milliseconds = sessionOffsetMs % 1_000
  return `+${String(hours).padStart(2, '0')}:${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}.${String(milliseconds).padStart(3, '0')}`
}

function eventDateTime(receivedAt: number) {
  const date = new Date(receivedAt)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString()
}

export function EventTimeline({ events }: { events: RealtimeEvent[] }) {
  const viewportRef = useRef<HTMLDivElement>(null)
  const [scrollTop, setScrollTop] = useState(0)
  const [viewportHeight, setViewportHeight] = useState(fallbackViewportHeight)

  useEffect(() => {
    const measure = () => {
      const height = viewportRef.current?.clientHeight ?? 0
      if (height > 0) setViewportHeight(height)
    }
    measure()
    window.addEventListener('resize', measure)
    return () => window.removeEventListener('resize', measure)
  }, [])

  const windowed = useMemo(() => {
    const start = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan)
    const count = Math.ceil(viewportHeight / rowHeight) + overscan * 2
    return { start, events: events.slice(start, start + count) }
  }, [events, scrollTop, viewportHeight])

  function handleScroll(event: UIEvent<HTMLDivElement>) {
    setScrollTop(event.currentTarget.scrollTop)
  }

  function jumpToLatest() {
    const viewport = viewportRef.current
    if (!viewport) return
    const nextScrollTop = Math.max(0, events.length * rowHeight - viewport.clientHeight)
    viewport.scrollTop = nextScrollTop
    setScrollTop(nextScrollTop)
  }

  if (events.length === 0) {
    return (
      <div className="realtime-empty" role="status">
        <strong>暂时没有匹配的事件</strong>
        <span>事件到达后会自动显示；切换筛选可查看其他类型。</span>
      </div>
    )
  }

  return (
    <div className="event-timeline-shell">
      <button className="button event-timeline__jump" type="button" onClick={jumpToLatest}>
        <ArrowDownToLine aria-hidden="true" />跳到最新
      </button>
      <div
        aria-label="实时事件时间线"
        className="event-timeline"
        onScroll={handleScroll}
        ref={viewportRef}
        role="log"
        tabIndex={0}
      >
        <div className="event-timeline__canvas" style={{ height: events.length * rowHeight }}>
          {windowed.events.map((event, index) => (
            <article
              className={`event-row event-row--${event.kind}`}
              data-testid="event-row"
              key={event.id}
              style={{ height: rowHeight, transform: `translateY(${(windowed.start + index) * rowHeight}px)` }}
            >
              <time
                aria-label={`场次偏移 ${sessionOffsetText(event.sessionOffsetMs)}；接收时间 ${receivedAtText(event.receivedAt)}`}
                dateTime={eventDateTime(event.receivedAt)}
                title={`接收时间 ${receivedAtText(event.receivedAt)}`}
              >
                {sessionOffsetText(event.sessionOffsetMs)}
              </time>
              <span className="event-row__kind">{eventLabels[event.kind]}</span>
              <div className="event-row__content">
                <strong>{event.displayName || '匿名观众'}</strong>
                <span>{eventDescription(event)}</span>
              </div>
            </article>
          ))}
        </div>
      </div>
    </div>
  )
}
