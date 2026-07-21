import { z } from 'zod'

const safeInteger = z.number().int().min(Number.MIN_SAFE_INTEGER).max(Number.MAX_SAFE_INTEGER)
const nonnegativeInteger = z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER)
const finite = z.number().finite()
const uuid = z.string().uuid()

const totalsSchema = z.object({
  chatCount: nonnegativeInteger, uniqueChatters: nonnegativeInteger, likeDelta: nonnegativeInteger,
  giftCount: nonnegativeInteger, giftValue: finite.nonnegative().optional(), followCount: nonnegativeInteger,
  enterCount: nonnegativeInteger, activeUsers: nonnegativeInteger, messageTotal: nonnegativeInteger,
}).strict()

export const metricBucketSchema = z.object({
  bucketStartMs: nonnegativeInteger, bucketSizeMs: z.literal(10_000), chatCount: nonnegativeInteger,
  uniqueChatters: nonnegativeInteger, likeDelta: nonnegativeInteger, giftCount: nonnegativeInteger,
  giftValue: finite.nonnegative().optional(), followCount: nonnegativeInteger, enterCount: nonnegativeInteger,
  activeUsers: nonnegativeInteger, messageTotal: nonnegativeInteger,
  completeness: finite.min(0).max(1),
}).strict()

const contributionSchema = z.object({
  metric: z.enum(['chat_rate', 'unique_interactors', 'like_delta', 'follow_count', 'gift_value_or_count']),
  weight: finite.min(0).max(1), score: finite,
}).strict()

export const candidateSchema = z.object({
  id: z.string().regex(/^candidate-[0-9a-f]{16}$/), kind: z.enum(['peak', 'trough', 'highlight']),
  startMs: nonnegativeInteger, endMs: nonnegativeInteger, score: finite, threshold: finite,
  baselineMedian: finite, baselineMad: finite.nonnegative(), completeness: finite.min(0).max(1),
  contributions: z.array(contributionSchema).max(7), evidenceBucketMs: z.array(nonnegativeInteger).max(100_000),
  algorithmVersion: z.literal('basic-analysis/v1'), sourceCandidateId: z.string().regex(/^candidate-[0-9a-f]{16}$/).optional(),
}).strict().refine((value) => value.endMs >= value.startMs, '候选结束时间不能早于开始时间')

export const analysisReportSchema = z.object({
  version: z.literal(1), id: uuid, sessionId: uuid, status: z.literal('completed'),
  analysisVersion: z.string().regex(/^basic-analysis\/v1\+[0-9a-f]{16}$/),
  algorithmVersion: z.literal('basic-analysis/v1'), startedAt: safeInteger, completedAt: safeInteger,
  summary: z.object({
    durationMs: nonnegativeInteger, bucketSizeMs: z.literal(10_000), bucketCount: nonnegativeInteger,
    completeness: finite.min(0).max(1), totals: totalsSchema, peakCount: nonnegativeInteger,
    troughCount: nonnegativeInteger, highlightCount: nonnegativeInteger,
    warnings: z.array(z.enum([
      'GAPS_PRESENT', 'GIFT_VALUE_UNAVAILABLE', 'LOW_COMPLETENESS',
      'TIMELINE_EXTENDED', 'UNPARSED_EVENTS_PRESENT',
    ])).max(5),
  }).strict(),
  buckets: z.array(metricBucketSchema).max(100_000), peaks: z.array(candidateSchema).max(100_000),
  troughs: z.array(candidateSchema).max(100_000), highlights: z.array(candidateSchema).max(100_000),
}).strict().superRefine((value, context) => {
  if (value.summary.bucketCount !== value.buckets.length) context.addIssue({ code: 'custom', message: '分桶数量不一致' })
  if (value.summary.peakCount !== value.peaks.length) context.addIssue({ code: 'custom', message: '峰值数量不一致' })
  if (value.summary.troughCount !== value.troughs.length) context.addIssue({ code: 'custom', message: '低谷数量不一致' })
  if (value.summary.highlightCount !== value.highlights.length) context.addIssue({ code: 'custom', message: '高光数量不一致' })
  if (value.completedAt < value.startedAt) context.addIssue({ code: 'custom', message: '报告完成时间不合法' })
})

export type AnalysisReport = z.infer<typeof analysisReportSchema>
export type AnalysisCandidate = z.infer<typeof candidateSchema>
