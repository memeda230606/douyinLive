import { AlertCircle, CircleDot, CircleOff, LoaderCircle, Radio, RefreshCw, Timer } from 'lucide-react'

import type { RoomRuntimeStatus } from '../../lib/contracts'

export const statusMeta = {
  STOPPED: { label: '已停止', icon: CircleOff },
  WAITING: { label: '等待开播', icon: Timer },
  STARTING: { label: '正在连接', icon: LoaderCircle },
  LIVE: { label: '直播中', icon: Radio },
  RECORDING: { label: '录制中', icon: CircleDot },
  RECONNECTING: { label: '正在重连', icon: RefreshCw },
  FINALIZING: { label: '录制收尾', icon: LoaderCircle },
  ERROR: { label: '需要处理', icon: AlertCircle },
} as const

export function RoomStatusBadge({ status, fallback }: { status?: RoomRuntimeStatus; fallback: 'STOPPED' | 'WAITING' }) {
  const state = status?.state ?? fallback
  const meta = statusMeta[state]
  const Icon = meta.icon
  return <span className={`room-status room-status--${state.toLowerCase()}`}><Icon aria-hidden="true" />{meta.label}</span>
}
