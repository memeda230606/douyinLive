(() => {
  if (window.__p4AcceptanceRunning) return
  window.__p4AcceptanceRunning = true

  const schema = 'P4-ACC-001/v1'
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const normalize = (value) => String(value ?? '').replace(/\s+/g, ' ').trim()
  const visible = (element) => Boolean(element && element.isConnected && element.getClientRects().length)
  const bodyText = () => normalize(document.body.textContent)
  const waitFor = async (predicate, code, timeout = 60000) => {
    const deadline = Date.now() + timeout
    while (Date.now() < deadline) {
      try {
        const value = predicate()
        if (value) return value
      } catch {}
      await sleep(100)
    }
    throw new Error(code)
  }
  const binding = async (name) => waitFor(
    () => typeof window.go?.main?.DesktopApp?.[name] === 'function' && window.go.main.DesktopApp[name],
    'ACCEPTANCE_BINDING_NOT_READY',
  )
  const report = async (result) => (await binding('ReportP4AcceptanceResult'))(JSON.stringify(result))
  const nav = async (label) => {
    const target = await waitFor(
      () => [...document.querySelectorAll('.navigation button')].find((item) => visible(item) && normalize(item.getAttribute('aria-label')) === label && !item.disabled),
      'NAVIGATION_NOT_READY',
    )
    target.click()
  }

  const verifyHistory = async () => {
    await nav('历史场次')
    await waitFor(() => normalize(document.querySelector('.session-detail h2')?.textContent) === 'P4 回放分析验收场次', 'HISTORY_NOT_READY')
    await waitFor(() => bodyText().includes('4 条互动 · 2 个分片 · 0 个缺口'), 'TIMELINE_COUNTS_INVALID')
    const event = await waitFor(() => [...document.querySelectorAll('.sync-event')].find(visible), 'SYNC_EVENT_MISSING')
    event.click()
    const first = await waitFor(() => {
      const candidate = document.querySelector('video[aria-label="场次视频"]')
      return candidate && candidate.readyState >= 1 && Number.isFinite(candidate.duration) && candidate.duration >= 3.8 && candidate
    }, 'FIRST_MEDIA_DECODE_FAILED')
    await waitFor(() => Math.abs(first.currentTime - 1) <= 0.45, 'FIRST_MEDIA_SEEK_FAILED')
    const firstSource = first.currentSrc || first.src
    first.dispatchEvent(new Event('ended', { bubbles: true }))
    const second = await waitFor(() => {
      const candidate = document.querySelector('video[aria-label="场次视频"]')
      const source = candidate && (candidate.currentSrc || candidate.src)
      return candidate && source && source !== firstSource && candidate.readyState >= 1 && candidate.duration >= 3.8 && candidate
    }, 'CROSS_SEGMENT_ADVANCE_FAILED')
    await waitFor(() => second.currentTime <= 0.45, 'SECOND_MEDIA_SEEK_FAILED')
    const timelineAligned = normalize(document.querySelector('.player-meta')?.textContent).includes('当前位置 0:04 / 0:08')
    if (!timelineAligned) throw new Error('TIMELINE_ALIGNMENT_FAILED')
    return { mediaDecoded: true, crossSegmentAdvance: true, timelineAligned }
  }

  const verifyAnalysis = async () => {
    await nav('分析')
    await waitFor(() => normalize(document.querySelector('.analysis-heading h1')?.textContent) === '场次分析', 'ANALYSIS_NOT_READY')
    const analysisVisible = await waitFor(() => (
      bodyText().includes('P4 回放分析验收场次') &&
      bodyText().includes('十秒指标分桶') &&
      bodyText().includes('CSV / JSON 报告包')
    ), 'ANALYSIS_REPORT_MISSING')
    const asrDegraded = bodyText().includes('主播话术尚未启用') && bodyText().includes('基础互动指标、峰值、低谷和高光不受影响')
    if (!asrDegraded) throw new Error('ASR_DEGRADATION_MISSING')
    const exportButton = await waitFor(
      () => [...document.querySelectorAll('button')].find((item) => visible(item) && normalize(item.textContent) === '导出 CSV/JSON' && !item.disabled),
      'EXPORT_BUTTON_MISSING',
    )
    exportButton.click()
    const exportVisible = await waitFor(() => bodyText().includes('已写入应用导出目录') && bodyText().includes('（5 个文件）'), 'EXPORT_RESULT_MISSING')
    const forbidden = ['msToken', 'a_bogus', 'signature=', 'https://', 'http://', 'C:\\', 'D:\\', 'p4-acceptance-user', 'p4-acceptance-operation']
    const privacySafe = !forbidden.some((marker) => bodyText().includes(marker))
    if (!privacySafe) throw new Error('PRIVACY_BOUNDARY_FAILED')
    const page = document.querySelector('.analysis-page')?.getBoundingClientRect()
    const summary = document.querySelector('.analysis-summary')?.getBoundingClientRect()
    const exportPanel = document.querySelector('.analysis-export')?.getBoundingClientRect()
    const layoutUsable = Boolean(
      window.innerWidth >= 960 && page && summary && exportPanel &&
      page.width > 700 && summary.width > 600 && exportPanel.width > 600 &&
      document.documentElement.scrollWidth <= window.innerWidth + 2,
    )
    if (!layoutUsable) throw new Error('LAYOUT_UNUSABLE')
    return { analysisVisible: Boolean(analysisVisible), asrDegraded, exportVisible: Boolean(exportVisible), privacySafe, layoutUsable }
  }

  const run = async () => {
    await waitFor(() => document.readyState === 'complete' && document.querySelector('.app-shell'), 'APP_SHELL_NOT_READY')
    window.dispatchEvent(new Event('focus'))
    document.dispatchEvent(new Event('visibilitychange'))
    const history = await verifyHistory()
    const analysis = await verifyAnalysis()
    document.documentElement.dataset.p4Acceptance = 'pass'
    return {
      schema,
      success: true,
      checks: {
        historyVisible: true,
        mediaDecoded: history.mediaDecoded,
        crossSegmentAdvance: history.crossSegmentAdvance,
        timelineAligned: history.timelineAligned,
        analysisVisible: analysis.analysisVisible,
        asrDegraded: analysis.asrDegraded,
        exportVisible: analysis.exportVisible,
        privacySafe: analysis.privacySafe,
        layoutUsable: analysis.layoutUsable,
      },
    }
  }

  run().then(report).catch(async (error) => {
    const candidate = String(error?.message ?? 'ACCEPTANCE_FAILED')
    const errorCode = /^[A-Z0-9_]{1,64}$/.test(candidate) ? candidate : 'ACCEPTANCE_FAILED'
    document.documentElement.dataset.p4Acceptance = 'fail'
    try { await report({ schema, success: false, errorCode }) } catch {}
  })
})()
