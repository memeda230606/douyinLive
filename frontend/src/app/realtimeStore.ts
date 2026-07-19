import { create } from 'zustand'

import type { LiveEventBatch, RealtimeEvent, RecordingProgress, RoomRuntimeStatus } from '../lib/contracts'

export const maximumRealtimeEvents = 2_000
export const maximumRealtimeAlerts = 100
const maximumProgressSessions = 16

export type RealtimeAlertSeverity = 'info' | 'warning' | 'error'
export type RealtimeAlert = {
  id: string
  roomId: string
  sessionId?: string
  kind: 'gap_open' | 'gap_recovered' | 'recording_error'
  severity: RealtimeAlertSeverity
  message: string
  errorCode?: string
  occurredAt: number
}

type RealtimeState = {
  events: RealtimeEvent[]
  eventIds: Record<string, true>
  progressBySession: Record<string, RecordingProgress>
  latestRoomStatus: Record<string, RoomRuntimeStatus>
  alerts: RealtimeAlert[]
  mergeLiveEventBatch: (batch: LiveEventBatch) => void
  applyRecordingProgress: (progress: RecordingProgress) => void
  applyRoomStatus: (status: RoomRuntimeStatus) => void
  reset: () => void
}

function compareEvents(left: RealtimeEvent, right: RealtimeEvent) {
  return left.receivedAt - right.receivedAt ||
    left.ingestSequence - right.ingestSequence ||
    left.id.localeCompare(right.id)
}

function recordingIsReconnecting(status?: RoomRuntimeStatus) {
  return status?.state === 'RECONNECTING' || status?.recordingStatus === 'reconnecting'
}

function recordingIsRecovered(status: RoomRuntimeStatus) {
  if (status.state === 'RECORDING' || status.recordingStatus === 'recording') return true
  return status.state === 'LIVE' &&
    status.recordingStatus !== 'disabled' &&
    status.recordingStatus !== 'unavailable'
}

function recordingHasError(status?: RoomRuntimeStatus) {
  return status?.recordingStatus === 'unavailable' || status?.recordingStatus === 'failed'
}

function safeErrorCode(value?: string) {
  return value && /^[A-Z0-9_]{1,64}$/.test(value) ? value : undefined
}

function appendAlert(alerts: RealtimeAlert[], alert?: RealtimeAlert) {
  if (!alert || alerts.some((candidate) => candidate.id === alert.id)) return alerts
  const next = [...alerts, alert]
  return next.length > maximumRealtimeAlerts ? next.slice(next.length - maximumRealtimeAlerts) : next
}

function statusAlert(previous: RoomRuntimeStatus | undefined, status: RoomRuntimeStatus): RealtimeAlert | undefined {
  const wasReconnecting = recordingIsReconnecting(previous)
  const reconnecting = recordingIsReconnecting(status)
  const hadError = recordingHasError(previous)
  const hasError = recordingHasError(status)
  const base = {
    roomId: status.roomId,
    sessionId: status.sessionId,
    occurredAt: status.changedAt,
  }
  if (!wasReconnecting && reconnecting) {
    return {
      ...base,
      id: `${status.roomId}:${status.revision}:gap-open:${status.operationId ?? ''}`,
      kind: 'gap_open', severity: 'warning',
      message: '采集连接出现缺口，系统正在自动重试。',
      errorCode: safeErrorCode(status.errorCode),
    }
  }
  if (wasReconnecting && !reconnecting && recordingIsRecovered(status)) {
    return {
      ...base,
      id: `${status.roomId}:${status.revision}:gap-recovered:${status.operationId ?? ''}`,
      kind: 'gap_recovered', severity: 'info',
      message: '采集已恢复，缺口记录已保留供后续核对。',
    }
  }
  if (hasError && (!hadError || previous?.errorCode !== status.errorCode)) {
    return {
      ...base,
      id: `${status.roomId}:${status.revision}:recording-error:${status.operationId ?? ''}`,
      kind: 'recording_error', severity: 'error',
      message: '录制当前不可用，请查看房间状态与诊断。',
      errorCode: safeErrorCode(status.errorCode),
    }
  }
  return undefined
}

const emptyState = () => ({
  events: [] as RealtimeEvent[],
  eventIds: {} as Record<string, true>,
  progressBySession: {} as Record<string, RecordingProgress>,
  latestRoomStatus: {} as Record<string, RoomRuntimeStatus>,
  alerts: [] as RealtimeAlert[],
})

export const useRealtimeStore = create<RealtimeState>((set) => ({
  ...emptyState(),
  mergeLiveEventBatch: (batch) => set((state) => {
    const eventIds = { ...state.eventIds }
    const additions: RealtimeEvent[] = []
    for (const event of batch.events) {
      if (eventIds[event.id]) continue
      eventIds[event.id] = true
      additions.push({ ...event, sessionId: batch.sessionId })
    }
    if (additions.length === 0) return state
    const events = [...state.events, ...additions].sort(compareEvents)
    if (events.length <= maximumRealtimeEvents) return { events, eventIds }
    const evicted = events.slice(0, events.length - maximumRealtimeEvents)
    for (const event of evicted) delete eventIds[event.id]
    return { events: events.slice(events.length - maximumRealtimeEvents), eventIds }
  }),
  applyRecordingProgress: (progress) => set((state) => {
    const fence = state.latestRoomStatus[progress.roomId]
    const fenceMatches = Boolean(fence &&
      fence.sessionId === progress.sessionId &&
      fence.operationId === progress.operationId)
    if (fence && !fenceMatches) return state
    const current = state.progressBySession[progress.sessionId]
    if (current?.operationId === progress.operationId && progress.updatedAt <= current.updatedAt) return state
    if (current && current.operationId !== progress.operationId && !fenceMatches) return state
    const progressBySession = { ...state.progressBySession }
    for (const [sessionId, value] of Object.entries(progressBySession)) {
      if (sessionId !== progress.sessionId && value.roomId === progress.roomId) delete progressBySession[sessionId]
    }
    progressBySession[progress.sessionId] = progress
    const entries = Object.entries(progressBySession)
    if (entries.length > maximumProgressSessions) {
      entries.sort((left, right) => right[1].updatedAt - left[1].updatedAt)
      return { progressBySession: Object.fromEntries(entries.slice(0, maximumProgressSessions)) }
    }
    return { progressBySession }
  }),
  applyRoomStatus: (status) => set((state) => {
    const previous = state.latestRoomStatus[status.roomId]
    if (previous && status.revision <= previous.revision) return state
    const progressBySession = { ...state.progressBySession }
    for (const [sessionId, progress] of Object.entries(progressBySession)) {
      if (progress.roomId === status.roomId &&
        (progress.sessionId !== status.sessionId || progress.operationId !== status.operationId)) {
        delete progressBySession[sessionId]
      }
    }
    return {
      latestRoomStatus: { ...state.latestRoomStatus, [status.roomId]: status },
      alerts: appendAlert(state.alerts, statusAlert(previous, status)),
      progressBySession,
    }
  }),
  reset: () => set(emptyState()),
}))
