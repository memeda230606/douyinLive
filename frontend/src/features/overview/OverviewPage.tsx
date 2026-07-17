import { Activity, AlertTriangle, Database, Plus, Radio, ShieldCheck } from 'lucide-react'

import type { DataStatusDTO } from '../../app/bootstrap'
import type { RoomsDashboard } from '../rooms/useRoomsDashboard'

export function OverviewPage({ data, dashboard, onAddRoom }: { data: DataStatusDTO; dashboard: RoomsDashboard; onAddRoom: () => void }) {
  const rooms = dashboard.roomsQuery.data ?? []
  const statuses = Object.values(dashboard.statuses)
  const live = statuses.filter((status) => ['LIVE', 'RECORDING'].includes(status.state)).length
  const finalizing = statuses.filter((status) => status.state === 'FINALIZING').length
  const waiting = statuses.filter((status) => ['WAITING', 'STARTING', 'RECONNECTING'].includes(status.state)).length
  const errors = statuses.filter((status) => status.state === 'ERROR').length
  const liveDetail = live
    ? `${live} 个房间直播或录制中${finalizing ? ` · ${finalizing} 个正在收尾` : ''}`
    : finalizing ? `${finalizing} 个房间正在完成录制收尾` : '当前没有直播中的房间'
  const summary = [
    { label: '正在直播', value: String(live), detail: liveDetail, icon: Radio },
    { label: '等待开播', value: String(waiting), detail: `${rooms.filter((room) => room.monitorEnabled).length} 个房间启用自动监听`, icon: Activity },
    { label: '需要处理', value: String(errors), detail: errors ? '请前往直播间查看错误状态' : '没有待处理的房间异常', icon: AlertTriangle },
    {
      label: '本地数据',
      value: data.ready ? '就绪' : '不可用',
      detail: data.ready ? `SQLite Schema v${data.schemaVersion} · ${data.loggingReady ? '日志已启用' : '日志未就绪'}` : '请查看诊断页',
      icon: Database,
    },
  ]

  return (
    <main className="page">
      <div className="page__heading">
        <div>
          <p className="eyebrow">运行总览</p>
          <h1>直播间运行总览</h1>
          <p>集中查看监听、开播与异常状态；所有数据保留在本机。</p>
        </div>
        <div className="ready-badge"><ShieldCheck aria-hidden="true" />{data.ready ? '本地存储正常' : '本地存储异常'}</div>
      </div>

      <section className="summary-grid" aria-label="关键状态">
        {summary.map(({ label, value, detail, icon: Icon }) => (
          <article className="metric-card" key={label}>
            <div className="metric-card__icon"><Icon aria-hidden="true" /></div>
            <span>{label}</span><strong>{value}</strong><p>{detail}</p>
          </article>
        ))}
      </section>

      {rooms.length === 0 ? (
        <section className="empty-panel">
          <div><h2>添加第一个直播间</h2><p>保存直播间标识后，可以在后台等待开播并实时查看连接状态。</p></div>
          <button type="button" onClick={onAddRoom}><Plus aria-hidden="true" />添加直播间</button>
        </section>
      ) : (
        <section className="section-panel">
          <div className="section-panel__heading"><div><h2>最近活动</h2><p>按最近更新展示房间运行状态。</p></div></div>
          <div className="activity-list">{rooms.slice(0, 4).map((room) => {
            const status = dashboard.statuses[room.id]
            return <div className="activity-row" key={room.id}><div><strong>{room.alias}</strong><span>{status?.title || `直播间 ${room.liveId}`}</span></div><span>{status?.message || (room.monitorEnabled ? '正在读取状态' : '已停止监控')}</span></div>
          })}</div>
        </section>
      )}
    </main>
  )
}
