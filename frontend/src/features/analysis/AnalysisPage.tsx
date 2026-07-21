import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, BarChart3, Clock3, Play, RefreshCw, ShieldCheck, Sparkles } from 'lucide-react'
import { useMemo, useState } from 'react'

import { userFacingError } from '../../lib/desktop'
import { listPlaybackSessions } from '../sessions/api'
import { analyzeSession, getAnalysisReport } from './api'
import type { AnalysisCandidate, AnalysisReport } from './contracts'

const warningLabels: Record<string, string> = {
  GAPS_PRESENT: '场次存在采集缺口，相关区间已降低完整度。',
  GIFT_VALUE_UNAVAILABLE: '礼物价值映射不完整，仅展示可靠的礼物数量。',
  LOW_COMPLETENESS: '场次整体完整度偏低，候选结论应谨慎使用。',
  TIMELINE_EXTENDED: '事件时间线超出录制结束时间，报告已按证据扩展。',
  UNPARSED_EVENTS_PRESENT: '存在未解析事件，基础统计仅计入可识别字段。',
}

const metricLabels: Record<string, string> = {
  chat_rate: '弹幕速率', unique_interactors: '独立互动', like_delta: '点赞',
  follow_count: '关注', gift_value_or_count: '礼物',
}

function formatDuration(milliseconds: number) {
  const seconds = Math.max(0, Math.floor(milliseconds / 1000))
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, '0')}`
}

function formatDate(milliseconds: number) {
  return new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(milliseconds))
}

function downsample(report: AnalysisReport) {
  if (report.buckets.length <= 240) return report.buckets
  const stride = Math.ceil(report.buckets.length / 240)
  return report.buckets.filter((_, index) => index % stride === 0)
}

export function AnalysisPage({ onOpenPlayback }: { onOpenPlayback: (sessionId: string, offsetMs: number) => void }) {
  const queryClient = useQueryClient()
  const [selectedId, setSelectedId] = useState<string>()
  const sessionsQuery = useQuery({
    queryKey: ['analysis', 'sessions'],
    queryFn: () => listPlaybackSessions(['completed', 'interrupted', 'failed']),
  })
  const sessions = sessionsQuery.data?.items ?? []
  const activeId = selectedId ?? sessions[0]?.id
  const reportQuery = useQuery({
    queryKey: ['analysis', 'report', activeId],
    queryFn: () => getAnalysisReport(activeId!), enabled: Boolean(activeId), retry: false,
  })
  const analyzeMutation = useMutation({
    mutationFn: () => analyzeSession(activeId!),
    onSuccess: (report) => queryClient.setQueryData(['analysis', 'report', activeId], report),
  })


  if (sessionsQuery.isLoading) return <main className="page analysis-page"><p className="eyebrow">基础分析</p><h1>正在读取可分析场次…</h1></main>
  if (sessionsQuery.isError) return <ErrorState title="分析场次不可用" error={sessionsQuery.error} />

  return <main className="page analysis-page">
    <header className="analysis-heading">
      <div><p className="eyebrow">可追溯基础分析</p><h1>场次分析</h1><p className="page-subtitle">固定 10 秒分桶，展示互动趋势、峰值、低谷、高光与缺口影响。</p></div>
      {sessions.length > 0 && <label className="session-filter">分析场次
        <select value={activeId} onChange={(event) => setSelectedId(event.target.value)}>
          {sessions.map((session) => <option key={session.id} value={session.id}>{session.title || session.roomAlias} · {formatDate(session.startedAt)}</option>)}
        </select>
      </label>}
    </header>

    {sessions.length === 0 ? <section className="sessions-empty"><BarChart3 aria-hidden="true" /><h2>还没有可分析场次</h2><p>场次进入终态后即可生成基础分析。</p></section>
      : reportQuery.isLoading ? <section className="analysis-state"><RefreshCw aria-hidden="true" /><h2>正在读取分析报告…</h2></section>
      : reportQuery.data ? <ReportView report={reportQuery.data} onOpenPlayback={onOpenPlayback} onRegenerate={() => analyzeMutation.mutate()} regenerating={analyzeMutation.isPending} />
      : <section className="analysis-state"><BarChart3 aria-hidden="true" /><h2>尚未生成基础分析</h2><p>{reportQuery.isError ? userFacingError(reportQuery.error) : '为当前场次生成版本化分析报告。'}</p><button className="button button--primary" disabled={analyzeMutation.isPending} onClick={() => analyzeMutation.mutate()} type="button">{analyzeMutation.isPending ? '正在分析…' : '生成分析'}</button>{analyzeMutation.isError && <span className="timeline-error"><AlertTriangle aria-hidden="true" />{userFacingError(analyzeMutation.error)}</span>}</section>}
  </main>
}

function ErrorState({ title, error }: { title: string; error: unknown }) {
  return <main className="page analysis-page"><p className="eyebrow">基础分析</p><h1>{title}</h1><p className="page-subtitle">{userFacingError(error)}</p></main>
}

function ReportView({ report, onOpenPlayback, onRegenerate, regenerating }: { report: AnalysisReport; onOpenPlayback: (sessionId: string, offsetMs: number) => void; onRegenerate: () => void; regenerating: boolean }) {
  const points = useMemo(() => downsample(report), [report])
  const maximum = Math.max(1, ...points.map((bucket) => bucket.chatCount + bucket.likeDelta + bucket.giftCount))
  return <>
    <section className="analysis-summary" aria-label="分析摘要">
      <SummaryCard label="弹幕" value={report.summary.totals.chatCount} detail={`${report.summary.totals.uniqueChatters} 位发言用户`} />
      <SummaryCard label="点赞增量" value={report.summary.totals.likeDelta} detail={`${report.summary.totals.activeUsers} 位活跃用户`} />
      <SummaryCard label="礼物" value={report.summary.totals.giftCount} detail={report.summary.totals.giftValue === undefined ? '价值不可可靠计算' : `可靠价值 ${report.summary.totals.giftValue}`} />
      <SummaryCard label="完整度" value={`${Math.round(report.summary.completeness * 100)}%`} detail={`${report.summary.bucketCount} 个十秒分桶`} />
    </section>

    {report.summary.warnings.length > 0 && <section className="analysis-warnings" aria-label="数据质量提示">{report.summary.warnings.map((warning) => <p key={warning}><AlertTriangle aria-hidden="true" />{warningLabels[warning]}</p>)}</section>}

    <section className="analysis-panel">
      <div className="analysis-panel__heading"><div><p className="eyebrow">互动趋势</p><h2>十秒指标分桶</h2></div><span><ShieldCheck aria-hidden="true" />缺口已计入完整度</span></div>
      <div className="analysis-chart" role="img" aria-label={`互动趋势，共 ${report.summary.bucketCount} 个十秒分桶`}>
        {points.map((bucket) => <span key={bucket.bucketStartMs} style={{ height: `${Math.max(2, (bucket.chatCount + bucket.likeDelta + bucket.giftCount) / maximum * 100)}%`, opacity: Math.max(.25, bucket.completeness) }} title={`${formatDuration(bucket.bucketStartMs)} · 弹幕 ${bucket.chatCount} · 点赞 ${bucket.likeDelta} · 礼物 ${bucket.giftCount} · 完整度 ${Math.round(bucket.completeness * 100)}%`} />)}
      </div>
    </section>

    <div className="analysis-candidate-grid">
      <CandidateList title="高光片段" icon={<Sparkles aria-hidden="true" />} candidates={report.highlights} report={report} onOpenPlayback={onOpenPlayback} />
      <CandidateList title="互动峰值" icon={<BarChart3 aria-hidden="true" />} candidates={report.peaks} report={report} onOpenPlayback={onOpenPlayback} />
      <CandidateList title="互动低谷" icon={<Clock3 aria-hidden="true" />} candidates={report.troughs} report={report} onOpenPlayback={onOpenPlayback} />
    </div>

    <footer className="analysis-version"><code>{report.analysisVersion}</code><span>算法 {report.algorithmVersion} · 完成于 {formatDate(report.completedAt)}</span><button className="button button--secondary" disabled={regenerating} onClick={onRegenerate} type="button">{regenerating ? '正在校验输入…' : '重新分析'}</button></footer>
  </>
}

function SummaryCard({ label, value, detail }: { label: string; value: string | number; detail: string }) {
  return <article><span>{label}</span><strong>{value}</strong><small>{detail}</small></article>
}

function CandidateList({ title, icon, candidates, report, onOpenPlayback }: { title: string; icon: React.ReactNode; candidates: AnalysisCandidate[]; report: AnalysisReport; onOpenPlayback: (sessionId: string, offsetMs: number) => void }) {
  return <section className="candidate-list"><header>{icon}<h2>{title}</h2><span>{candidates.length}</span></header>{candidates.length === 0 ? <p className="candidate-list__empty">当前数据未形成可靠候选。</p> : candidates.map((candidate) => <button aria-label={`${title} ${formatDuration(candidate.startMs)} 得分 ${candidate.score.toFixed(2)}`} key={candidate.id} onClick={() => onOpenPlayback(report.sessionId, candidate.startMs)} type="button"><span>{formatDuration(candidate.startMs)}–{formatDuration(candidate.endMs)}</span><strong>得分 {candidate.score.toFixed(2)}</strong><small>完整度 {Math.round(candidate.completeness * 100)}% · {candidate.contributions.slice(0, 3).map((item) => metricLabels[item.metric]).join('、')}</small><Play aria-hidden="true" /></button>)}</section>
}
