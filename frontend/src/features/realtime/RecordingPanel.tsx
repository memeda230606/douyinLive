import { AlertCircle, CheckCircle2, Info, RotateCcw, TriangleAlert } from 'lucide-react'
import { useEffect, useState } from 'react'

import type { RecordingProgress, RoomRuntimeStatus } from '../../lib/contracts'
import type { RealtimeAlert } from '../../app/realtimeStore'

function formatDuration(milliseconds: number) {
  const totalSeconds = Math.floor(milliseconds / 1_000)
  const hours = Math.floor(totalSeconds / 3_600)
  const minutes = Math.floor((totalSeconds % 3_600) / 60)
  const seconds = totalSeconds % 60
  return [hours, minutes, seconds].map((value) => String(value).padStart(2, '0')).join(':')
}

function formatBytes(bytes: number) {
  if (bytes < 1_024) return `${bytes} B`
  if (bytes < 1_048_576) return `${(bytes / 1_024).toFixed(1)} KiB`
  if (bytes < 1_073_741_824) return `${(bytes / 1_048_576).toFixed(1)} MiB`
  return `${(bytes / 1_073_741_824).toFixed(2)} GiB`
}

function useClock(enabled: boolean) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!enabled) return undefined
    setNow(Date.now())
    const timer = window.setInterval(() => setNow(Date.now()), 1_000)
    return () => window.clearInterval(timer)
  }, [enabled])
  return now
}

type RecordingPresentation = {
  key: string
  label: string
  Icon: typeof Info
}

function recordingPresentation(status?: RoomRuntimeStatus): RecordingPresentation {
  if (!status) return { key: 'idle', label: '尚无录制状态', Icon: Info }
  if (status.recordingStatus === 'unavailable') return { key: 'unavailable', label: '录制不可用', Icon: AlertCircle }
  if (status.state === 'ERROR') {
    return status.sessionId || status.recordingStatus
      ? { key: 'error', label: '录制异常', Icon: AlertCircle }
      : { key: 'room-error', label: '房间异常', Icon: AlertCircle }
  }
  if (status.state === 'RECONNECTING' || status.recordingStatus === 'reconnecting') {
    return { key: 'reconnecting', label: '正在恢复', Icon: RotateCcw }
  }
  if (status.state === 'FINALIZING' || status.recordingStatus === 'finalizing') {
    return { key: 'finalizing', label: '正在收尾', Icon: RotateCcw }
  }
  switch (status.recordingStatus) {
    case 'pending': return { key: 'pending', label: '等待录制', Icon: Info }
    case 'disabled': return { key: 'disabled', label: '录制已关闭', Icon: Info }
    case 'starting': return { key: 'starting', label: '正在启动', Icon: RotateCcw }
    case 'recording': return { key: 'recording', label: '录制中', Icon: CheckCircle2 }
    case 'completed': return { key: 'completed', label: '录制完成', Icon: CheckCircle2 }
    case 'incomplete': return { key: 'incomplete', label: '录制不完整', Icon: AlertCircle }
    case 'failed': return { key: 'failed', label: '录制失败', Icon: AlertCircle }
    default:
      if (status.state === 'RECORDING') return { key: 'recording', label: '录制中', Icon: CheckCircle2 }
      if (status.state === 'STARTING') return { key: 'starting', label: '正在启动', Icon: RotateCcw }
      return { key: 'idle', label: '尚无录制状态', Icon: Info }
  }
}

export function RecordingPanel({
  alerts, progress, status,
}: {
  alerts: RealtimeAlert[]
  progress?: RecordingProgress
  status?: RoomRuntimeStatus
}) {
  const retryEnabled = Boolean(status?.retryAt !== undefined && status.retryAt > Date.now())
  const now = useClock(retryEnabled)
  const retrySeconds = retryEnabled && status?.retryAt !== undefined ? Math.max(0, Math.ceil((status.retryAt - now) / 1_000)) : undefined
  const presentation = recordingPresentation(status)
  const PresentationIcon = presentation.Icon

  return (
    <aside className="recording-panel" aria-label="录制状态">
      <div className="recording-panel__heading">
        <div><p className="eyebrow">Recording</p><h2>录制状态</h2></div>
        <span className={`recording-state recording-state--${presentation.key}`}>
          <PresentationIcon aria-hidden="true" />
          {presentation.label}
        </span>
      </div>

      {retrySeconds !== undefined && status?.retryAt !== undefined && status.retryAt > now && (
        <div className="retry-countdown" role="status"><RotateCcw aria-hidden="true" /><span>将在 <strong>{retrySeconds} 秒</strong>后重试</span></div>
      )}

      <dl className="recording-metrics">
        <div><dt>录制时长</dt><dd>{formatDuration(progress?.elapsedMs ?? 0)}</dd></div>
        <div><dt>已写入</dt><dd>{formatBytes(progress?.bytesWritten ?? 0)}</dd></div>
        <div><dt>分片</dt><dd>{progress?.segmentCount ?? 0}</dd></div>
        <div><dt>速度</dt><dd>{(progress?.speed ?? 0).toFixed(2)}×</dd></div>
        <div><dt>帧率</dt><dd>{(progress?.fps ?? 0).toFixed(1)}</dd></div>
        <div><dt>重启</dt><dd>{progress?.restartCount ?? 0}</dd></div>
      </dl>

      <section className="alert-panel" aria-labelledby="realtime-alert-title">
        <div className="alert-panel__heading"><h3 id="realtime-alert-title">采集告警</h3><span>{alerts.length}</span></div>
        {alerts.length === 0 ? (
          <div className="alert-panel__empty"><CheckCircle2 aria-hidden="true" />暂无连接或录制告警</div>
        ) : (
          <ol className="alert-list">
            {[...alerts].reverse().map((alert) => {
              const Icon = alert.severity === 'error' ? AlertCircle : alert.severity === 'warning' ? TriangleAlert : Info
              return (
                <li className={`alert-item alert-item--${alert.severity}`} key={alert.id}>
                  <Icon aria-hidden="true" />
                  <div><strong>{alert.kind === 'gap_open' ? '连接缺口' : alert.kind === 'gap_recovered' ? '连接已恢复' : '录制异常'}</strong><span>{alert.message}</span>{alert.errorCode && <code>{alert.errorCode}</code>}</div>
                </li>
              )
            })}
          </ol>
        )}
      </section>
    </aside>
  )
}
