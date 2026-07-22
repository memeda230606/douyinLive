import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import * as sessionsApi from '../sessions/api'
import * as api from './api'
import { AnalysisPage } from './AnalysisPage'

vi.mock('./api')
vi.mock('../sessions/api')
vi.mock('../../lib/desktop', () => ({ userFacingError: () => '尚无报告' }))

const sessionId = '019aa000-0000-7000-8000-000000000001'
const report = {
  version: 1 as const, id: '019aa000-0000-7000-8000-000000000002', sessionId, status: 'completed' as const,
  analysisVersion: 'basic-analysis/v1+0123456789abcdef', algorithmVersion: 'basic-analysis/v1' as const,
  startedAt: 1, completedAt: 2,
  summary: { durationMs: 20_000, bucketSizeMs: 10_000 as const, bucketCount: 2, completeness: .9,
    totals: { chatCount: 5, uniqueChatters: 2, likeDelta: 8, giftCount: 1, followCount: 0, enterCount: 0, activeUsers: 2, messageTotal: 6 },
    peakCount: 1, troughCount: 0, highlightCount: 1, warnings: ['GAPS_PRESENT' as const] },
  buckets: [0, 10_000].map((bucketStartMs) => ({ bucketStartMs, bucketSizeMs: 10_000 as const, chatCount: 2, uniqueChatters: 1, likeDelta: 4, giftCount: bucketStartMs ? 1 : 0, followCount: 0, enterCount: 0, activeUsers: 1, messageTotal: 3, completeness: bucketStartMs ? .8 : 1 })),
  peaks: [{ id: 'candidate-0123456789abcdef', kind: 'peak' as const, startMs: 10_000, endMs: 20_000, score: 3, threshold: 2, baselineMedian: 1, baselineMad: 1, completeness: .8, contributions: [{ metric: 'chat_rate' as const, weight: .3, score: 3 }], evidenceBucketMs: [10_000], algorithmVersion: 'basic-analysis/v1' as const }],
  troughs: [],
  highlights: [{ id: 'candidate-fedcba9876543210', kind: 'highlight' as const, startMs: 10_000, endMs: 20_000, score: 3, threshold: 2, baselineMedian: 1, baselineMad: 1, completeness: .8, contributions: [{ metric: 'chat_rate' as const, weight: .3, score: 3 }], evidenceBucketMs: [10_000], algorithmVersion: 'basic-analysis/v1' as const, sourceCandidateId: 'candidate-0123456789abcdef' }],
}

function renderPage(onOpenPlayback = vi.fn()) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  render(<QueryClientProvider client={client}><AnalysisPage onOpenPlayback={onOpenPlayback} /></QueryClientProvider>)
  return onOpenPlayback
}

describe('AnalysisPage', () => {
  beforeEach(() => {
    vi.mocked(sessionsApi.listPlaybackSessions).mockResolvedValue({ version: 1, items: [{ id: sessionId, roomConfigId: '019aa000-0000-7000-8000-000000000003', roomAlias: '测试直播间', title: '新品场', status: 'completed', recordingStatus: 'completed', startedAt: 1_700_000_000_000, endedAt: 1_700_000_020_000, captureOffsetMs: 0, clockSource: 'media', integrityScore: .9 }] })
    vi.mocked(api.getAnalysisReport).mockResolvedValue(report)
    vi.mocked(api.analyzeSession).mockResolvedValue(report)
    vi.mocked(api.getASRStatus).mockResolvedValue({ version: 1, providerId: 'disabled', state: 'disabled', configured: false, available: false, errorCode: 'ASR_NOT_CONFIGURED' })
    vi.mocked(api.exportAnalysisReport).mockResolvedValue({
      version: 1, exportId: '019aa000-0000-7000-8000-000000000010',
      directoryName: 'analysis-019aa000-0000-7000-8000-000000000002-019aa000-0000-7000-8000-000000000010',
      generatedAt: '2026-07-22T08:00:00.000Z', includeText: true,
      files: [
        { name: 'events.csv', mediaType: 'text/csv; charset=utf-8', rowCount: 1, sizeBytes: 100, sha256: 'a'.repeat(64) },
        { name: 'metric-buckets.csv', mediaType: 'text/csv; charset=utf-8', rowCount: 2, sizeBytes: 100, sha256: 'b'.repeat(64) },
        { name: 'transcripts.csv', mediaType: 'text/csv; charset=utf-8', rowCount: 0, sizeBytes: 100, sha256: 'c'.repeat(64) },
        { name: 'media-segments.csv', mediaType: 'text/csv; charset=utf-8', rowCount: 1, sizeBytes: 100, sha256: 'd'.repeat(64) },
        { name: 'manifest.json', mediaType: 'application/json', rowCount: 1, sizeBytes: 100, sha256: 'e'.repeat(64) },
      ],
    })
  })

  it('renders summary, quality warning, version and jumps a highlight into playback', async () => {
    const user = userEvent.setup()
    const onOpenPlayback = renderPage()
    expect(await screen.findByRole('heading', { name: '十秒指标分桶' })).toBeInTheDocument()
    expect(screen.getByText('主播话术尚未启用')).toBeInTheDocument()
    expect(screen.getByText(/基础互动指标、峰值、低谷和高光不受影响/)).toBeInTheDocument()
    expect(screen.getByText('场次存在采集缺口，相关区间已降低完整度。')).toBeInTheDocument()
    expect(screen.getByText('basic-analysis/v1+0123456789abcdef')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /高光片段.*得分 3.00/ }))
    expect(onOpenPlayback).toHaveBeenCalledWith(sessionId, 10_000)
  })

  it('can generate a missing report', async () => {
    vi.mocked(api.getAnalysisReport).mockRejectedValueOnce(new Error('not found'))
    const user = userEvent.setup()
    renderPage()
    await user.click(await screen.findByRole('button', { name: '生成分析' }))
    expect(api.analyzeSession).toHaveBeenCalledWith(sessionId)
    expect(await screen.findByRole('heading', { name: '十秒指标分桶' })).toBeInTheDocument()
  })

  it('keeps the basic report available when provider validation fails', async () => {
    vi.mocked(api.getASRStatus).mockRejectedValueOnce(new Error('contract failure'))
    renderPage()
    expect(await screen.findByRole('heading', { name: '十秒指标分桶' })).toBeInTheDocument()
    expect(screen.getByText('主播话术暂时不可用')).toBeInTheDocument()
    expect(screen.getByText(/基础互动指标、峰值、低谷和高光仍可正常生成/)).toBeInTheDocument()
  })

  it('hides the degradation notice when a provider is ready', async () => {
    vi.mocked(api.getASRStatus).mockResolvedValueOnce({
      version: 1, providerId: 'local-test', state: 'ready', configured: true, available: true,
    })
    renderPage()
    expect(await screen.findByRole('heading', { name: '十秒指标分桶' })).toBeInTheDocument()
    expect(screen.queryByLabelText('转写能力状态')).not.toBeInTheDocument()
  })

  it('defaults to privacy-safe export and requires an explicit text opt-in', async () => {
    const user = userEvent.setup()
    renderPage()
    expect(await screen.findByRole('heading', { name: 'CSV / JSON 报告包' })).toBeInTheDocument()
    expect(screen.getByText(/默认排除昵称、弹幕正文、转写正文/)).toBeInTheDocument()
    await user.click(screen.getByRole('checkbox', { name: /显式包含弹幕与转写正文/ }))
    await user.click(screen.getByRole('button', { name: '导出 CSV/JSON' }))
    expect(api.exportAnalysisReport).toHaveBeenCalledWith(sessionId, true)
    expect(await screen.findByText(/已写入应用导出目录/)).toHaveTextContent('5 个文件')
  })
})
