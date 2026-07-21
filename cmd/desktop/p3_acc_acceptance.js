(() => {
  if (window.__p3ACCAcceptanceRunning) return
  window.__p3ACCAcceptanceRunning = true

  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const visible = (element) => Boolean(element && element.isConnected && element.getClientRects().length)
  const normalize = (value) => String(value ?? '').replace(/\s+/g, ' ').trim()
  const waitFor = async (predicate, timeout = 45000, interval = 100) => {
    const deadline = Date.now() + timeout
    while (Date.now() < deadline) {
      try {
        const value = predicate()
        if (value) return value
      } catch {}
      await sleep(interval)
    }
    throw new Error('P3ACC_UI_NOT_READY')
  }
  const binding = async (name) => waitFor(() => {
    const candidate = window.go?.main?.DesktopApp?.[name]
    return typeof candidate === 'function' ? candidate : null
  })
  const observe = async (phase) => (await binding('ObserveP3ACCAcceptanceUI'))(phase)
  const sampleResources = async () => {
    try {
      await (await binding('SampleP3ACCAcceptanceResources'))()
    } catch {
      try { await (await binding('MarkP3ACCAcceptanceResourceFailure'))() } catch {}
      throw new Error('P3ACC_RESOURCE_SAMPLE_FAILED')
    }
  }
  const afterTwoFrames = () => new Promise((resolve) => {
    window.requestAnimationFrame(() => window.requestAnimationFrame(resolve))
  })

  const run = async () => {
    await waitFor(() => document.readyState === 'complete' && document.querySelector('.app-shell'))
    window.dispatchEvent(new Event('focus'))
    document.dispatchEvent(new Event('visibilitychange'))
    const realtime = await waitFor(() => {
      const candidate = document.querySelector('button[aria-label="实时监控"]')
      return visible(candidate) && !candidate.disabled ? candidate : null
    })
    realtime.click()
    await waitFor(() => (
      visible(document.querySelector('.realtime-page')) &&
      visible(document.querySelector('.realtime-page__heading .room-status')) &&
      visible(document.querySelector('.recording-state')) &&
      visible(document.querySelector('.recording-metrics')) &&
      visible(document.querySelector('.timeline-panel'))
    ))
    const eventsOn = await waitFor(() => (
      typeof window.runtime?.EventsOn === 'function' ? window.runtime.EventsOn.bind(window.runtime) : null
    ))
    let latencyTracking = false
    let latencyAckChain = Promise.resolve()
    eventsOn('live:event', () => {
      if (!latencyTracking) return
      latencyAckChain = latencyAckChain
        .then(afterTwoFrames)
        .then(async () => {
          if (!visible(document.querySelector('.timeline-panel')) ||
              ![...document.querySelectorAll('.event-row')].some(visible)) {
            throw new Error('P3ACC_UI_LATENCY_FAILED')
          }
          await (await binding('AckP3ACCAcceptanceLiveEventRendered'))()
        })
        .catch(async () => {
          latencyTracking = false
          try { await (await binding('MarkP3ACCAcceptanceUILatencyFailure'))() } catch {}
          document.documentElement.dataset.p3AccAcceptance = 'error'
        })
    })
    latencyTracking = true
    await observe('READY')

    let resourceSamples = 0
    await sampleResources()
    resourceSamples += 1
    const resourceTimer = window.setInterval(() => {
      if (resourceSamples >= 120) {
        window.clearInterval(resourceTimer)
        return
      }
      resourceSamples += 1
      sampleResources().catch(() => window.clearInterval(resourceTimer))
    }, 30000)

    let recorded = false
    let progressAdvanced = false
    let timelineSeen = false
    let reconnecting = false
    let recovered = false
    let networkArmed = false
    let networkReconnecting = false
    let networkRecovered = false
    let offline = false
    let crashRequested = false
    let networkArmRequested = false
    let previousMetrics = ''
    // The outer controller owns the bounded application lifetime and may wait
    // for a real offline event for several hours. Keep this one-shot probe
    // alive until the final contract succeeds or the controller closes the app.
    for (;;) {
      const roomState = normalize(document.querySelector('.realtime-page__heading .room-status')?.textContent)
      const recordingState = normalize(document.querySelector('.recording-state')?.textContent)
      const metrics = [...document.querySelectorAll('.recording-metrics dd')]
        .filter(visible)
        .map((element) => normalize(element.textContent))
        .join('|')
      const timelineRows = [...document.querySelectorAll('.event-row')].filter(visible).length

      // Read only the harness' privacy-safe booleans/enums. No DOM-derived
      // values are ever returned to Go or written to the result artifact.
      const safeSnapshot = await (await binding('GetP3ACCAcceptanceSnapshot'))()
      if (safeSnapshot?.runtime?.crashInjected === true) crashRequested = true
      if (safeSnapshot?.runtime?.networkFaultArmed === true) networkArmed = true

      if (roomState === '录制中' && recordingState === '录制中') {
        if (!recorded) {
          await observe('RECORDING')
          recorded = true
        }
        if (reconnecting && !recovered) {
          await observe('RECOVERED')
          recovered = true
        }
        if (networkReconnecting && !networkRecovered) {
          await observe('NETWORK_RECOVERED')
          networkRecovered = true
        }
      }
      if (recorded && !progressAdvanced && previousMetrics && metrics && metrics !== previousMetrics) {
        await observe('PROGRESS_ADVANCED')
        progressAdvanced = true
      }
      if (recorded && !timelineSeen && timelineRows > 0) {
        await observe('TIMELINE_VISIBLE')
        timelineSeen = true
      }
      if (recorded && roomState === '正在重连' && recordingState === '正在恢复') {
        if (networkArmed && recovered && !networkReconnecting) {
          await observe('NETWORK_RECONNECTING')
          networkReconnecting = true
        } else if (!reconnecting) {
          await observe('RECONNECTING')
          reconnecting = true
        }
      }
      if (!crashRequested && recorded && progressAdvanced && timelineSeen &&
          safeSnapshot?.runtime?.recorderTargetMatched === true &&
          safeSnapshot?.runtime?.currentAttemptCommitted === true &&
          safeSnapshot?.resources?.stableWindowProven === true &&
          safeSnapshot?.resources?.cpuWithinTarget === true) {
        await (await binding('CrashP3ACCAcceptanceRecorder'))()
        crashRequested = true
        await waitFor(() => (
          normalize(document.querySelector('.realtime-page__heading .room-status')?.textContent) === '正在重连' &&
          normalize(document.querySelector('.recording-state')?.textContent) === '正在恢复'
        ), 15000, 25)
        if (!reconnecting) {
          await observe('RECONNECTING')
          reconnecting = true
        }
      }
      if (!networkArmRequested && recovered && safeSnapshot?.runtime?.recoveryProven === true) {
        await (await binding('ArmP3ACCAcceptanceNetworkFault'))()
        networkArmRequested = true
      }
      if (networkRecovered && roomState === '等待开播' && recordingState === '尚无录制状态' && !offline) {
        await observe('OFFLINE')
        offline = true
      }
      if (offline) {
        if (safeSnapshot?.stage === 'FINALIZED' &&
            safeSnapshot?.ui?.latencySampleCount > 0 &&
            safeSnapshot?.ui?.latencyPendingCount === 0 &&
            safeSnapshot?.ui?.latencyWithinTarget === true) {
          await observe('FINALIZED')
          window.clearInterval(resourceTimer)
          document.documentElement.dataset.p3AccAcceptance = 'ready'
          return
        }
      }
      if (metrics) previousMetrics = metrics
      await sleep(1000)
    }
  }

  run().catch(() => {
    document.documentElement.dataset.p3AccAcceptance = 'error'
  })
})()
