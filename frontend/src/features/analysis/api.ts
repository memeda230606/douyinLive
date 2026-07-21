import { AnalyzeSession, GetAnalysisReport } from '../../generated/wailsjs/go/main/DesktopApp'
import { analysis } from '../../generated/wailsjs/go/models'
import { contractError } from '../../lib/contracts'
import { analysisReportSchema } from './contracts'

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
