import { describe, expect, it } from 'vitest'

import { analysisReportSchema } from './contracts'

const report = {
  version: 1, id: '019aa000-0000-7000-8000-000000000001', sessionId: '019aa000-0000-7000-8000-000000000002',
  status: 'completed', analysisVersion: 'basic-analysis/v1+0123456789abcdef', algorithmVersion: 'basic-analysis/v1',
  startedAt: 1, completedAt: 2,
  summary: { durationMs: 10_000, bucketSizeMs: 10_000, bucketCount: 1, completeness: 1,
    totals: { chatCount: 1, uniqueChatters: 1, likeDelta: 0, giftCount: 0, followCount: 0, enterCount: 0, activeUsers: 1, messageTotal: 1 },
    peakCount: 0, troughCount: 0, highlightCount: 0, warnings: [] },
  buckets: [{ bucketStartMs: 0, bucketSizeMs: 10_000, chatCount: 1, uniqueChatters: 1, likeDelta: 0, giftCount: 0, followCount: 0, enterCount: 0, activeUsers: 1, messageTotal: 1, completeness: 1 }],
  peaks: [], troughs: [], highlights: [],
}

describe('analysisReportSchema', () => {
  it('accepts the versioned privacy-safe report', () => {
    expect(analysisReportSchema.parse(report)).toEqual(report)
  })

  it('rejects private fields, inconsistent counts and unknown warnings', () => {
    expect(analysisReportSchema.safeParse({ ...report, rawContent: 'secret' }).success).toBe(false)
    expect(analysisReportSchema.safeParse({ ...report, summary: { ...report.summary, bucketCount: 2 } }).success).toBe(false)
    expect(analysisReportSchema.safeParse({ ...report, summary: { ...report.summary, warnings: ['UNKNOWN'] } }).success).toBe(false)
  })
})
