import { Activity, Database, Radio, ShieldCheck } from 'lucide-react'

import type { DataStatusDTO } from '../../app/bootstrap'

export function OverviewPage({ data }: { data: DataStatusDTO }) {
  const summary = [
    { label: '正在直播', value: '0', detail: '尚未添加直播间', icon: Radio },
    { label: '等待开播', value: '0', detail: '监控服务将在后续阶段接入', icon: Activity },
    {
      label: '本地数据',
      value: data.ready ? '就绪' : '不可用',
      detail: data.ready
        ? `SQLite Schema v${data.schemaVersion} · ${data.loggingReady ? 'JSONL 日志已启用' : '日志未就绪'}`
        : '请查看启动日志',
      icon: Database,
    },
  ]

  return (
    <main className="page">
      <div className="page__heading">
        <div>
          <p className="eyebrow">运行总览</p>
          <h1>数据基础已就绪</h1>
          <p>前端通过受控应用服务访问 Go 核心，后续功能将在此骨架上逐步启用。</p>
        </div>
        <div className="ready-badge"><ShieldCheck aria-hidden="true" />本地存储正常</div>
      </div>

      <section className="summary-grid" aria-label="关键状态">
        {summary.map(({ label, value, detail, icon: Icon }) => (
          <article className="metric-card" key={label}>
            <div className="metric-card__icon"><Icon aria-hidden="true" /></div>
            <span>{label}</span><strong>{value}</strong><p>{detail}</p>
          </article>
        ))}
      </section>

      <section className="empty-panel">
        <div><h2>还没有直播间配置</h2><p>SQLite 与日志基础已就绪，下一步将开放房间配置管理。</p></div>
        <button type="button" disabled>添加第一个直播间</button>
      </section>
    </main>
  )
}
