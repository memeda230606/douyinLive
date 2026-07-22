import { describe, expect, it } from 'vitest'

import { analysisExportResultSchema, analysisReportSchema, asrStatusSchema } from './contracts'

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

describe('asrStatusSchema', () => {
  it('accepts disabled and ready states', () => {
    expect(asrStatusSchema.parse({
      version: 1, providerId: 'disabled', state: 'disabled', configured: false,
      available: false, errorCode: 'ASR_NOT_CONFIGURED',
    }).state).toBe('disabled')
    expect(asrStatusSchema.parse({
      version: 1, providerId: 'local-whisper', state: 'ready', configured: true, available: true,
    }).state).toBe('ready')
  })

  it('rejects private fields and inconsistent degradation states', () => {
    expect(asrStatusSchema.safeParse({
      version: 1, providerId: 'disabled', state: 'disabled', configured: false,
      available: false, errorCode: 'ASR_NOT_CONFIGURED', endpoint: 'private',
    }).success).toBe(false)
    expect(asrStatusSchema.safeParse({
      version: 1, providerId: 'disabled', state: 'disabled', configured: true,
      available: false, errorCode: 'ASR_NOT_CONFIGURED',
    }).success).toBe(false)
  })
})

const exportResult = {
  version: 1, exportId: '019aa000-0000-7000-8000-000000000010',
  directoryName: 'analysis-019aa000-0000-7000-8000-000000000011-019aa000-0000-7000-8000-000000000010',
  generatedAt: '2026-07-22T08:00:00.000Z', includeText: false,
  files: ['events.csv', 'metric-buckets.csv', 'transcripts.csv', 'media-segments.csv', 'manifest.json'].map((name, index) => ({
    name, mediaType: index === 4 ? 'application/json' : 'text/csv; charset=utf-8',
    rowCount: index, sizeBytes: 100 + index, sha256: 'a'.repeat(64),
  })),
}

describe('analysisExportResultSchema', () => {
  it('accepts the strict privacy-safe file manifest', () => {
    expect(analysisExportResultSchema.parse(exportResult)).toEqual(exportResult)
  })

  it('rejects paths, unknown files and reordered manifests', () => {
    expect(analysisExportResultSchema.safeParse({ ...exportResult, absolutePath: 'C:/private' }).success).toBe(false)
    expect(analysisExportResultSchema.safeParse({ ...exportResult, files: [
      { ...exportResult.files[0], name: 'private.txt' }, ...exportResult.files.slice(1),
    ] }).success).toBe(false)
    expect(analysisExportResultSchema.safeParse({ ...exportResult, files: [...exportResult.files].reverse() }).success).toBe(false)
  })
})
