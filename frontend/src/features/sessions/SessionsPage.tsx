import { useInfiniteQuery, useMutation, useQuery } from '@tanstack/react-query'
import { AlertTriangle, CalendarDays, ChevronLeft, ChevronRight, Clock3, Film, MessageSquareText, Play, ShieldCheck } from 'lucide-react'
import { useMemo, useRef, useState } from 'react'

import { userFacingError } from '../../lib/desktop'
import {
  getPlaybackSession,
  listPlaybackEvents,
  listPlaybackGaps,
  listPlaybackMedia,
  listPlaybackSessions,
  locatePlaybackMedia,
  playbackMediaURL,
} from './api'
import type { MediaLocation, MediaSegment, PlaybackEvent } from './contracts'

const statusLabels: Record<string, string> = {
  completed: '已完成', interrupted: '已中断', failed: '失败',
  starting: '启动中', recording: '录制中', finalizing: '收尾中',
}
const kindLabels: Record<string, string> = {
  chat: '弹幕', gift: '礼物', like: '点赞', member: '进房', follow: '关注', system: '系统', unknown: '其他',
}

function formatDate(value: number) {
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value))
}

function formatDuration(value: number) {
  const seconds = Math.max(0, Math.floor(value / 1000))
  const hours = Math.floor(seconds / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  const remaining = seconds % 60
  return `${hours ? `${hours}:` : ''}${String(minutes).padStart(hours ? 2 : 1, '0')}:${String(remaining).padStart(2, '0')}`
}

function usePagedQuery<T extends { nextCursor?: string }>(
  key: readonly unknown[],
  enabled: boolean,
  query: (cursor: string) => Promise<T>,
) {
  return useInfiniteQuery({
    queryKey: key,
    enabled,
    initialPageParam: '',
    queryFn: ({ pageParam }) => query(pageParam),
    getNextPageParam: (last) => last.nextCursor || undefined,
  })
}

export function SessionsPage() {
  const [status, setStatus] = useState('all')
  const [selectedId, setSelectedId] = useState<string>()
  const [location, setLocation] = useState<MediaLocation>()
  const [currentOffset, setCurrentOffset] = useState(0)
  const playerRef = useRef<HTMLVideoElement>(null)
  const statuses = status === 'all' ? [] : [status]
  const sessionsQuery = usePagedQuery(['playback', 'sessions', status], true, (cursor) => listPlaybackSessions(statuses, cursor))
  const sessions = sessionsQuery.data?.pages.flatMap((page) => page.items) ?? []
  const activeId = selectedId ?? sessions[0]?.id
  const detailQuery = useQuery({
    queryKey: ['playback', 'session', activeId],
    queryFn: () => getPlaybackSession(activeId!),
    enabled: Boolean(activeId),
  })
  const eventsQuery = usePagedQuery(['playback', 'events', activeId], Boolean(activeId), (cursor) => listPlaybackEvents(activeId!, cursor))
  const gapsQuery = usePagedQuery(['playback', 'gaps', activeId], Boolean(activeId), (cursor) => listPlaybackGaps(activeId!, cursor))
  const mediaQuery = usePagedQuery(['playback', 'media', activeId], Boolean(activeId), (cursor) => listPlaybackMedia(activeId!, cursor))
  const events = eventsQuery.data?.pages.flatMap((page) => page.items) ?? []
  const gaps = gapsQuery.data?.pages.flatMap((page) => page.items) ?? []
  const media = mediaQuery.data?.pages.flatMap((page) => page.items) ?? []
  const session = detailQuery.data?.session
  const duration = Math.max(
    session?.endedAt ? session.endedAt - session.startedAt : 0,
    ...media.map((item) => item.timelineEndMs),
    ...gaps.map((item) => item.endOffsetMs ?? item.startOffsetMs),
    1,
  )
  const nearbyEvents = useMemo(() => events.filter((event) => Math.abs(event.sessionOffsetMs - currentOffset) <= 5_000), [events, currentOffset])
  const locateMutation = useMutation({
    mutationFn: (offsetMs: number) => locatePlaybackMedia(activeId!, offsetMs),
    onSuccess: (next) => {
      setLocation(next)
      setCurrentOffset(next.requestedOffsetMs)
    },
  })

  function selectSession(id: string) {
    setSelectedId(id)
    setLocation(undefined)
    setCurrentOffset(0)
  }

  function seekTo(offsetMs: number) {
    if (activeId) locateMutation.mutate(offsetMs)
  }

  function onPlayerTimeUpdate() {
    if (!location?.segment || location.segmentPlaybackMs === undefined || !playerRef.current) return
    const delta = playerRef.current.currentTime * 1000 - location.segmentPlaybackMs
    setCurrentOffset(Math.max(0, location.requestedOffsetMs + delta))
  }

  if (sessionsQuery.isLoading) return <div className="page sessions-page"><p className="eyebrow">历史场次</p><h1>正在读取本地场次…</h1></div>
  if (sessionsQuery.isError) return <div className="page sessions-page"><p className="eyebrow">历史场次</p><h1>历史场次不可用</h1><p className="page-subtitle">{userFacingError(sessionsQuery.error)}</p></div>

  return (
    <main className="page sessions-page">
      <header className="sessions-heading">
        <div><p className="eyebrow">本地复盘</p><h1>历史场次</h1><p className="page-subtitle">视频、弹幕和录制缺口使用同一条时间轴。</p></div>
        <label className="session-filter">场次状态
          <select value={status} onChange={(event) => setStatus(event.target.value)}>
            <option value="all">全部</option><option value="completed">已完成</option>
            <option value="interrupted">已中断</option><option value="failed">失败</option>
          </select>
        </label>
      </header>

      {sessions.length === 0 ? (
        <section className="sessions-empty"><CalendarDays aria-hidden="true" /><h2>还没有历史场次</h2><p>完成一次录制后，场次会出现在这里。</p></section>
      ) : (
        <div className="sessions-layout">
          <aside className="session-list" aria-label="历史场次列表">
            {sessions.map((item) => (
              <button className={item.id === activeId ? 'session-row is-active' : 'session-row'} key={item.id} onClick={() => selectSession(item.id)} type="button">
                <span className="session-row__title">{item.title || item.roomAlias || '未命名场次'}</span>
                <span>{item.roomAlias} · {formatDate(item.startedAt)}</span>
                <span className={`session-state session-state--${item.status}`}>{statusLabels[item.status] ?? item.status}</span>
              </button>
            ))}
            {sessionsQuery.hasNextPage && <button className="button button--secondary" type="button" onClick={() => sessionsQuery.fetchNextPage()}>加载更多场次</button>}
          </aside>

          <section className="session-detail" aria-live="polite">
            {!session ? <p>正在加载场次详情…</p> : <>
              <div className="session-detail__heading">
                <div><p className="eyebrow">{session.roomAlias}</p><h2>{session.title || '未命名场次'}</h2><p>{formatDate(session.startedAt)} · {formatDuration((session.endedAt ?? Date.now()) - session.startedAt)}</p></div>
                <div className="integrity-badge"><ShieldCheck aria-hidden="true" /><span>完整性</span><strong>{Math.round(session.integrityScore * 100)}%</strong></div>
              </div>

              <div className="playback-grid">
                <div className="player-card">
                  {location?.state === 'playback_mp4' && location.playbackArtifactId ? (
                    <video
                      aria-label="场次视频"
                      controls
                      key={location.playbackArtifactId}
                      onLoadedMetadata={(event) => { event.currentTarget.currentTime = (location.segmentPlaybackMs ?? 0) / 1000 }}
                      onTimeUpdate={onPlayerTimeUpdate}
                      ref={playerRef}
                      src={playbackMediaURL(location.playbackArtifactId)}
                    />
                  ) : (
                    <div className="player-placeholder"><Film aria-hidden="true" /><strong>{location?.state === 'gap' ? '此位置没有可播放画面' : '选择时间线中的事件开始回放'}</strong><span>{location?.reasonCode ?? '播放器只读取已审计的 H.264 MP4 代理文件'}</span></div>
                  )}
                  <div className="player-meta"><Clock3 aria-hidden="true" />当前位置 {formatDuration(currentOffset)} / {formatDuration(duration)}</div>
                </div>

                <div className="sync-events">
                  <div className="sync-events__heading"><MessageSquareText aria-hidden="true" /><div><strong>同步互动</strong><span>当前位置前后 5 秒</span></div></div>
                  {nearbyEvents.length === 0 ? <p className="sync-events__empty">当前位置附近没有互动事件。</p> : nearbyEvents.map((event) => <EventButton event={event} key={event.id} onSeek={seekTo} />)}
                </div>
              </div>

              <div className="unified-timeline" aria-label="统一时间轴">
                <div className="unified-timeline__heading"><strong>统一时间轴</strong><span>{events.length} 条互动 · {media.length} 个分片 · {gaps.length} 个缺口</span></div>
                <div className="timeline-track" onClick={(event) => {
                  const bounds = event.currentTarget.getBoundingClientRect()
                  seekTo(((event.clientX - bounds.left) / bounds.width) * duration)
                }}
                onKeyDown={(event) => {
                  if (event.key === 'Home') seekTo(0)
                  else if (event.key === 'End') seekTo(duration)
                  else if (event.key === 'ArrowLeft') seekTo(Math.max(0, currentOffset - 5_000))
                  else if (event.key === 'ArrowRight') seekTo(Math.min(duration, currentOffset + 5_000))
                  else return
                  event.preventDefault()
                }}
                role="button"
                tabIndex={0}
                >
                  {media.map((item) => <TimelineSegment duration={duration} item={item} key={item.id} />)}
                  {gaps.map((gap) => <span className="timeline-gap" key={gap.id} style={{ left: `${gap.startOffsetMs / duration * 100}%`, width: `${Math.max(0.4, ((gap.endOffsetMs ?? gap.startOffsetMs + 1_000) - gap.startOffsetMs) / duration * 100)}%` }} title={gap.reasonCode} />)}
                  {events.slice(0, 200).map((event) => <button aria-label={`跳转到${kindLabels[event.kind] ?? event.kind} ${formatDuration(event.sessionOffsetMs)}`} className={`timeline-event timeline-event--${event.kind}`} key={event.id} onClick={(click) => { click.stopPropagation(); seekTo(event.sessionOffsetMs) }} style={{ left: `${event.sessionOffsetMs / duration * 100}%` }} type="button" />)}
                  <span className="timeline-cursor" style={{ left: `${Math.min(100, currentOffset / duration * 100)}%` }} />
                </div>
                <div className="timeline-legend"><span><i className="legend-media" />可播放媒体</span><span><i className="legend-gap" />录制缺口</span><span><i className="legend-event" />互动事件</span></div>
                <div className="timeline-actions">
                  {(eventsQuery.hasNextPage || mediaQuery.hasNextPage || gapsQuery.hasNextPage) && <button className="button button--secondary" type="button" onClick={() => {
                    if (eventsQuery.hasNextPage) void eventsQuery.fetchNextPage()
                    if (mediaQuery.hasNextPage) void mediaQuery.fetchNextPage()
                    if (gapsQuery.hasNextPage) void gapsQuery.fetchNextPage()
                  }}>加载更多时间轴数据</button>}
                  {locateMutation.isError && <span className="timeline-error"><AlertTriangle aria-hidden="true" />{userFacingError(locateMutation.error)}</span>}
                </div>
              </div>
            </>}
          </section>
        </div>
      )}
    </main>
  )
}

function EventButton({ event, onSeek }: { event: PlaybackEvent; onSeek: (offset: number) => void }) {
  return <button className="sync-event" type="button" onClick={() => onSeek(event.sessionOffsetMs)}>
    <span>{formatDuration(event.sessionOffsetMs)}</span><strong>{event.displayName || kindLabels[event.kind] || '互动'}</strong><small>{event.content || (event.numericValue !== undefined ? String(event.numericValue) : kindLabels[event.kind])}</small><Play aria-hidden="true" />
  </button>
}

function TimelineSegment({ item, duration }: { item: MediaSegment; duration: number }) {
  const playable = Boolean(item.playbackArtifactId)
  return <span className={playable ? 'timeline-media is-playable' : 'timeline-media'} style={{ left: `${item.timelineStartMs / duration * 100}%`, width: `${Math.max(0.4, (item.timelineEndMs - item.timelineStartMs) / duration * 100)}%` }} title={`分片 ${item.sequence} · ${item.status}`} />
}
