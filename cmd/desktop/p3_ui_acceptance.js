(() => {
  if (window.__p3UIAcceptanceRunning) return
  window.__p3UIAcceptanceRunning = true

  const schema = 'P3-UI-ACC-001/v1'
  const roomAlias = __P3_UI_ROOM_ALIAS__
  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const normalize = (value) => String(value ?? '').replace(/\s+/g, ' ').trim()
  const visible = (element) => Boolean(element && element.isConnected && element.getClientRects().length)
  const waitFor = async (predicate, code, timeout = 45000) => {
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
  const button = (label, root = document) => [...root.querySelectorAll('button')]
    .find((candidate) => visible(candidate) && !candidate.disabled && normalize(candidate.textContent) === label)
  const clickButton = (label, root = document) => {
    const target = button(label, root)
    if (!target) throw new Error('BUTTON_NOT_FOUND')
    target.click()
  }
  const binding = async (name) => waitFor(
    () => typeof window.go?.main?.DesktopApp?.[name] === 'function' && window.go.main.DesktopApp[name],
    'ACCEPTANCE_BINDING_NOT_READY',
  )
  const emitStage = async (stage) => (await binding('EmitP3UIAcceptanceFixture'))(stage)
  const report = async (result) => (await binding('ReportP3UIAcceptanceResult'))(JSON.stringify(result))
  const summaryCount = () => {
    const text = normalize(document.querySelector('.timeline-panel__heading p')?.textContent)
    const match = text.match(/当前场次已保留 (\d+) 条匹配事件/)
    return match ? Number(match[1]) : -1
  }
  const metric = (label) => [...document.querySelectorAll('.recording-metrics > div')]
    .find((row) => normalize(row.querySelector('dt')?.textContent) === label)
    ?.querySelector('dd')?.textContent
  const statusLabel = () => normalize(document.querySelector('.realtime-page__heading .room-status')?.textContent)
  const recordingLabel = () => normalize(document.querySelector('.recording-state')?.textContent)
  const bodyText = () => normalize(document.body.textContent)

  const verifyInitial = async () => {
    await emitStage(1)
    await waitFor(() => summaryCount() === 2000, 'TIMELINE_CAPACITY_FAILED')
    await waitFor(() => statusLabel() === '录制中' && recordingLabel() === '录制中', 'INITIAL_STATUS_FAILED')
    await waitFor(() => (
      normalize(metric('录制时长')) === '00:02:05' &&
      normalize(metric('已写入')) === '5.0 MiB' &&
      normalize(metric('分片')) === '3' &&
      normalize(metric('速度')) === '1.02×' &&
      normalize(metric('帧率')) === '29.7' &&
      normalize(metric('重启')) === '1'
    ), 'PROGRESS_METRICS_FAILED')

    const staleHidden = !bodyText().includes('P3UI_STALE_SHOULD_NOT_RENDER')
    const wrongSessionHidden = !bodyText().includes('P3UI_WRONG_SESSION_SHOULD_NOT_RENDER')
    if (!staleHidden) throw new Error('STATUS_FENCE_FAILED')
    if (!wrongSessionHidden) throw new Error('SESSION_FENCE_FAILED')

    const giftFilter = document.querySelector('[data-event-filter="gift"]')
    if (!giftFilter || !visible(giftFilter)) throw new Error('FILTER_NOT_FOUND')
    giftFilter.click()
    const filteredEventCount = await waitFor(() => summaryCount() > 0 && summaryCount() < 2000 && summaryCount(), 'FILTER_COUNT_FAILED')
    await waitFor(() => {
      const rows = [...document.querySelectorAll('.event-row')].filter(visible)
      return rows.length > 0 && rows.every((row) => normalize(row.querySelector('.event-row__kind')?.textContent) === '礼物')
    }, 'FILTER_CONTENT_FAILED')

    clickButton('全部', document.querySelector('.event-filters'))
    await waitFor(() => summaryCount() === 2000, 'FILTER_RESET_FAILED')
    clickButton('跳到最新')
    await waitFor(() => bodyText().includes('容量上限后最新事件'), 'LATEST_EVENT_FAILED')

    const layout = document.querySelector('.realtime-layout')?.getBoundingClientRect()
    const timeline = document.querySelector('.timeline-panel')?.getBoundingClientRect()
    const recording = document.querySelector('.recording-panel')?.getBoundingClientRect()
    const layoutUsable = Boolean(
      window.innerWidth >= 960 && layout && timeline && recording &&
      layout.width > 700 && timeline.width > 420 && recording.width > 250 &&
      document.documentElement.scrollWidth <= window.innerWidth + 2,
    )
    if (!layoutUsable) throw new Error('LAYOUT_UNUSABLE')
    return { filteredEventCount, staleHidden, wrongSessionHidden, layoutUsable }
  }

  const verifyReconnect = async () => {
    await emitStage(2)
    await waitFor(() => statusLabel() === '正在重连' && recordingLabel() === '正在恢复', 'RECONNECT_STATUS_FAILED')
    await waitFor(() => /将在 \d+ 秒后重试/.test(normalize(document.querySelector('.retry-countdown')?.textContent)), 'RETRY_COUNTDOWN_FAILED')
    await waitFor(() => bodyText().includes('连接缺口') && bodyText().includes('FFMPEG_EXITED'), 'GAP_OPEN_ALERT_FAILED')
  }

  const verifyRecovered = async () => {
    await emitStage(3)
    await waitFor(() => statusLabel() === '录制中' && recordingLabel() === '录制中', 'RECOVERY_STATUS_FAILED')
    await waitFor(() => !document.querySelector('.retry-countdown'), 'RETRY_COUNTDOWN_NOT_CLEARED')
    await waitFor(() => bodyText().includes('连接已恢复'), 'GAP_RECOVERY_ALERT_FAILED')
    await waitFor(() => document.querySelectorAll('.alert-item').length === 2, 'ALERT_COUNT_FAILED')
    await waitFor(() => (
      normalize(metric('录制时长')) === '00:02:10' &&
      normalize(metric('已写入')) === '7.0 MiB' &&
      normalize(metric('分片')) === '4' &&
      normalize(metric('重启')) === '2'
    ), 'RECOVERED_PROGRESS_FAILED')
    if (bodyText().includes('P3UI_STALE_SHOULD_NOT_RENDER') || bodyText().includes('录制异常')) {
      throw new Error('RECOVERED_STATUS_FENCE_FAILED')
    }
    const forbidden = ['msToken', 'a_bogus', 'signature=', 'https://', 'http://', 'C:\\']
    if (forbidden.some((marker) => bodyText().includes(marker))) throw new Error('PRIVACY_BOUNDARY_FAILED')
  }

  const run = async () => {
    await waitFor(() => document.readyState === 'complete' && document.querySelector('.app-shell'), 'APP_SHELL_NOT_READY')
    window.dispatchEvent(new Event('focus'))
    document.dispatchEvent(new Event('visibilitychange'))
    await sleep(250)
    const realtimeNav = await waitFor(
      () => document.querySelector('button[aria-label="实时监控"]'),
      'REALTIME_NAV_MISSING',
    )
    await waitFor(() => !realtimeNav.disabled && realtimeNav, 'REALTIME_NAV_DISABLED', 10000)
    realtimeNav.click()
    await waitFor(() => normalize(document.querySelector('.realtime-room-title h1')?.textContent) === roomAlias, 'REALTIME_ROOM_NOT_READY')

    const initial = await verifyInitial()
    await verifyReconnect()
    await verifyRecovered()
    const alerts = document.querySelectorAll('.alert-item').length
    const finalStatusLabel = statusLabel()
    return {
      schema,
      success: true,
      checks: {
        roomVisible: true,
        statusFence: initial.staleHidden,
        timelineCapacity: summaryCount() === 2000,
        sessionFence: initial.wrongSessionHidden,
        eventFilter: initial.filteredEventCount > 0,
        progressMetrics: normalize(metric('录制时长')) === '00:02:10',
        operationFence: normalize(metric('已写入')) === '7.0 MiB',
        retryCountdown: true,
        gapAlerts: alerts === 2,
        privacySafe: true,
        layoutUsable: initial.layoutUsable,
      },
      visibleEventCount: summaryCount(),
      filteredEventCount: initial.filteredEventCount,
      alertCount: alerts,
      statusLabel: finalStatusLabel,
    }
  }

  run().then(async (result) => {
    await report(result)
    document.documentElement.dataset.p3UiAcceptance = 'pass'
  }).catch(async (error) => {
    const candidate = String(error?.message ?? 'ACCEPTANCE_FAILED')
    const errorCode = /^[A-Z0-9_]{1,64}$/.test(candidate) ? candidate : 'ACCEPTANCE_FAILED'
    document.documentElement.dataset.p3UiAcceptance = 'fail'
    try { await report({ schema, success: false, errorCode }) } catch {}
  })
})()
