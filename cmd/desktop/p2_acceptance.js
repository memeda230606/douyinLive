(() => {
  const schema = 'P2-ACC-001/v1'
  const phase = __P2_PHASE__
  const liveRoomURL = __P2_LIVE_ROOM_URL__
  const recordingDirectory = __P2_RECORDING_DIRECTORY__
  const aliasBefore = 'P2 验收房间'
  const aliasAfter = 'P2 验收房间-已编辑'

  const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
  const normalize = (value) => String(value ?? '').replace(/\s+/g, ' ').trim()
  const visible = (element) => Boolean(element && element.isConnected && element.getClientRects().length)
  const waitFor = async (predicate, code, timeout = 30000) => {
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
  const ariaTarget = (label, root = document) => [...root.querySelectorAll('[aria-label]')]
    .find((candidate) => visible(candidate) && !candidate.disabled && candidate.getAttribute('aria-label') === label)
  const clickAria = (label, root = document) => {
    const target = ariaTarget(label, root)
    if (!target) throw new Error('ARIA_TARGET_NOT_FOUND')
    target.click()
  }
  const control = (name) => document.querySelector(`[name="${name}"]`)
  const setValue = (name, value) => {
    const element = control(name)
    if (!element) throw new Error('FORM_CONTROL_NOT_FOUND')
    const prototype = element instanceof HTMLSelectElement ? HTMLSelectElement.prototype : HTMLInputElement.prototype
    const setter = Object.getOwnPropertyDescriptor(prototype, 'value')?.set
    if (!setter) throw new Error('FORM_SETTER_NOT_FOUND')
    element.focus()
    setter.call(element, String(value))
    element.dispatchEvent(new Event('input', { bubbles: true }))
    element.dispatchEvent(new Event('change', { bubbles: true }))
    element.blur()
  }
  const setCheckbox = (name, wanted) => {
    const element = control(name)
    if (!(element instanceof HTMLInputElement) || element.type !== 'checkbox') throw new Error('CHECKBOX_NOT_FOUND')
    if (element.checked !== wanted) element.click()
  }
  const roomCard = (alias) => [...document.querySelectorAll('article.room-card')]
    .find((candidate) => normalize(candidate.querySelector('h2')?.textContent) === alias)
  const roomFact = (card, label) => [...(card?.querySelectorAll('dl div') ?? [])]
    .find((row) => normalize(row.querySelector('dt')?.textContent) === label)?.querySelector('dd')?.textContent ?? ''
  const settingsMatch = () =>
    control('recordingDirectory')?.value.toLowerCase() === recordingDirectory.toLowerCase() &&
    control('defaultQuality')?.value === 'high' &&
    control('defaultSegmentMinutes')?.value === '11' &&
    control('maxConcurrentRecordings')?.value === '2' &&
    control('minimumFreeSpaceGiB')?.value === '7' &&
    control('saveDisplayNames')?.checked === false
  const openPage = async (name) => {
    clickButton(name)
    await waitFor(() => normalize(document.querySelector('main h1')?.textContent) === name, 'PAGE_NOT_READY')
  }
  const report = async (result) => {
    await waitFor(() => typeof window.go?.main?.DesktopApp?.ReportAcceptanceResult === 'function', 'REPORT_BINDING_NOT_READY')
    await window.go.main.DesktopApp.ReportAcceptanceResult(JSON.stringify(result))
  }

  const phaseOne = async () => {
    await openPage('直播间')
    await waitFor(() => normalize(document.querySelector('.empty-panel h2')?.textContent) === '还没有直播间', 'INITIAL_ROOMS_NOT_EMPTY')
    clickButton('添加直播间')
    await waitFor(() => control('liveId'), 'ROOM_EDITOR_NOT_READY')
    setValue('liveId', liveRoomURL)
    setValue('alias', aliasBefore)
    setValue('quality', 'high')
    setValue('segmentMinutes', 11)
    setCheckbox('monitorEnabled', false)
    setCheckbox('recordEnabled', false)
    clickButton('保存配置', document.querySelector('[role="dialog"]'))
    await waitFor(() => !document.querySelector('[role="dialog"]') && roomCard(aliasBefore), 'ROOM_CREATE_FAILED')
    clickAria(`编辑 ${aliasBefore}`)
    await waitFor(() => control('alias'), 'ROOM_EDIT_NOT_READY')
    setValue('alias', aliasAfter)
    clickButton('保存配置', document.querySelector('[role="dialog"]'))
    const editedCard = await waitFor(() => !document.querySelector('[role="dialog"]') && roomCard(aliasAfter), 'ROOM_EDIT_FAILED')
    if (normalize(roomFact(editedCard, '录制质量')) !== '高清' || normalize(roomFact(editedCard, '自动录制')) !== '已关闭') {
      throw new Error('ROOM_EDIT_NOT_APPLIED')
    }
    clickButton('开始监控', editedCard)
    const activeCard = await waitFor(() => {
      const card = roomCard(aliasAfter)
      const message = normalize(card?.querySelector('.room-card__message strong')?.textContent)
      return card && button('停止监控', card) && message && message !== '正在获取运行状态' && message !== '监控未启用' ? card : null
    }, 'MONITOR_START_FAILED', 45000)
    const statusLabel = normalize(activeCard.querySelector('.room-status')?.textContent)
    if (!statusLabel || statusLabel === '已停止' || statusLabel === '需要处理') throw new Error('MONITOR_NOT_ACTIVE')

    await openPage('设置')
    await waitFor(() => control('recordingDirectory'), 'SETTINGS_NOT_READY')
    setValue('recordingDirectory', recordingDirectory)
    setValue('defaultQuality', 'high')
    setValue('defaultSegmentMinutes', 11)
    setValue('maxConcurrentRecordings', 2)
    setValue('minimumFreeSpaceGiB', 7)
    setCheckbox('saveDisplayNames', false)
    clickButton('保存设置')
    await waitFor(() => normalize(document.querySelector('.settings-actions span')?.textContent) === '设置已保存' && settingsMatch(), 'SETTINGS_SAVE_FAILED')
    return { schema, phase, success: true, checks: { roomCreated: true, roomEdited: true, monitoringActive: true, settingsSaved: true }, statusLabel }
  }

  const phaseTwo = async () => {
    await openPage('直播间')
    const restoredCard = await waitFor(() => {
      const card = roomCard(aliasAfter)
      const message = normalize(card?.querySelector('.room-card__message strong')?.textContent)
      return card && button('停止监控', card) && message && message !== '正在获取运行状态' && message !== '监控未启用' ? card : null
    }, 'MONITOR_RESTORE_FAILED', 45000)
    const statusLabel = normalize(restoredCard.querySelector('.room-status')?.textContent)
    if (!statusLabel || statusLabel === '已停止' || statusLabel === '需要处理') throw new Error('MONITOR_NOT_RESTORED')
    await openPage('设置')
    await waitFor(() => control('recordingDirectory') && settingsMatch(), 'SETTINGS_NOT_PERSISTED')
    await openPage('直播间')
    const activeCard = await waitFor(() => roomCard(aliasAfter), 'ROOM_NOT_PERSISTED')
    clickButton('停止监控', activeCard)
    const stoppedCard = await waitFor(() => {
      const card = roomCard(aliasAfter)
      return card && button('开始监控', card) && normalize(card.querySelector('.room-status')?.textContent) === '已停止' ? card : null
    }, 'MONITOR_STOP_FAILED')
    let confirmationCount = 0
    const originalConfirm = window.confirm
    try {
      window.confirm = () => { confirmationCount += 1; return true }
      clickAria(`删除 ${aliasAfter}`, stoppedCard)
      await waitFor(() => normalize(document.querySelector('.empty-panel h2')?.textContent) === '还没有直播间' && !roomCard(aliasAfter), 'ROOM_DELETE_FAILED')
    } finally {
      window.confirm = originalConfirm
    }
    return { schema, phase, success: true, checks: { roomPersisted: true, monitoringRestored: true, settingsPersisted: true, monitoringStopped: true, roomDeleted: true }, statusLabel, confirmationCount }
  }

  const phaseThree = async () => {
    await openPage('直播间')
    await waitFor(() => normalize(document.querySelector('.empty-panel h2')?.textContent) === '还没有直播间' && !roomCard(aliasAfter), 'DELETE_NOT_PERSISTED')
    await openPage('设置')
    await waitFor(() => control('recordingDirectory') && settingsMatch(), 'SETTINGS_SECOND_RESTART_FAILED')
    return { schema, phase, success: true, checks: { deletionPersisted: true, settingsPersisted: true } }
  }

  const run = async () => {
    await waitFor(() => document.readyState === 'complete' && document.querySelector('.app-shell'), 'APP_SHELL_NOT_READY', 45000)
    if (phase === 1) return phaseOne()
    if (phase === 2) return phaseTwo()
    if (phase === 3) return phaseThree()
    throw new Error('PHASE_INVALID')
  }

  run().then(report).catch(async (error) => {
    const candidate = String(error?.message ?? 'ACCEPTANCE_FAILED')
    const errorCode = /^[A-Z0-9_]{1,64}$/.test(candidate) ? candidate : 'ACCEPTANCE_FAILED'
    try { await report({ schema, phase, success: false, errorCode }) } catch {}
  })
})()