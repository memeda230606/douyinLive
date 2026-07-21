import { AnalyzeSession, GetAnalysisReport, GetASRStatus } from '../../generated/wailsjs/go/main/DesktopApp'
import { analysis } from '../../generated/wailsjs/go/models'
import { contractError } from '../../lib/contracts'
import { analysisReportSchema, asrStatusSchema } from './contracts'

function parseReport(name: string, value: unknown) {
  const result = analysisReportSchema.safeParse(value)
  if (!result.success) throw contractError(name, value)
  return result.data
}

export async function analyzeSession(sessionId: string) {
  const value = await AnalyzeSession(new analysis.AnalyzeRequest({ sessionId }))
  return parseReport('analysis report', value)
}

export async function getAnalysisReport(sessionId: string) {
  return parseReport('analysis report', await GetAnalysisReport(sessionId))
}

export async function getASRStatus() {
  const value = await GetASRStatus()
  const result = asrStatusSchema.safeParse(value)
  if (!result.success) throw contractError('ASR status', value)
  return result.data
}
