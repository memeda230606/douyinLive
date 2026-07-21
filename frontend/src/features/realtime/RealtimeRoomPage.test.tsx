import { act, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useRealtimeStore } from '../../app/realtimeStore'
import type { LiveEvent, RoomConfig, RoomRuntimeStatus } from '../../lib/contracts'
import { RealtimeRoomPage } from './RealtimeRoomPage'

const roomId = '019bce70-0c00-7000-8000-000000000001'
const sessionId = '019bce70-0c00-7000-8000-000000000002'
const operationId = '019bce70-0c00-7000-8000-000000000003'
const maliciousText = '<img src=x onerror=alert(1)>'

const room: RoomConfig = {
  id: roomId,
  liveId: '100000000001',
  alias: '测试直播间',
  monitorEnabled: true,
  recordEnabled: true,
  recordingProfile: { quality: 'auto', segmentMinutes: 10 },
  cookie: { configured: false },
  createdAt: 1,
  updatedAt: 1,
}

const recordingStatus: RoomRuntimeStatus = {
  roomId,
  liveId: room.liveId,
  alias: room.alias,
  state: 'RECORDING',
  revision: 100,
  sessionId,
  operationId,
  recordingStatus: 'recording',
  changedAt: 100,
  message: '录制中',
}

function liveEvent(index: number, kind: LiveEvent['kind'], content: string): LiveEvent {
  return {
    id: `019bce70-0c00-7000-8000-${index.toString(16).padStart(12, '0')}`,
    ingestSequence: index,
    role: 'source',
    kind,
    receivedAt: index,
    sessionOffsetMs: index,
    displayName: `观众 ${index}`,
    content,
    parseStatus: kind === 'unknown' ? 'unknown' : 'parsed',
  }
}

function page(status: RoomRuntimeStatus = recordingStatus) {
  return (
    <RealtimeRoomPage
      rooms={[room]}
      statuses={{ [roomId]: status }}
      roomId={roomId}
      onBack={vi.fn()}
      onRoomChange={vi.fn()}
    />
  )
}

function renderPage(status: RoomRuntimeStatus = recordingStatus) {
  return render(page(status))
}

describe('RealtimeRoomPage', () => {
  beforeEach(() => useRealtimeStore.getState().reset())

  it('renders six filters, a virtual timeline, alerts and untrusted text safely', async () => {
    const user = userEvent.setup()
    const leadingEvents = [
      liveEvent(1, 'chat', maliciousText),
      liveEvent(2, 'gift', '送出玫瑰'),
      liveEvent(3, 'like', '点赞直播间'),
      liveEvent(4, 'member', '进入直播间'),
      liveEvent(5, 'follow', '关注主播'),
      liveEvent(6, 'system', '系统提示'),
      liveEvent(7, 'unknown', '暂不支持的事件'),
    ]
    useRealtimeStore.getState().mergeLiveEventBatch({
      sessionId,
      emittedAt: 100,
      events: [...leadingEvents, ...Array.from({ length: 93 }, (_, offset) => liveEvent(offset + 8, 'chat', `消息 ${offset + 8}`))],
    })
    useRealtimeStore.getState().applyRecordingProgress({
      roomId, sessionId, operationId, state: 'recording', elapsedMs: 65_000,
      bytesWritten: 2_097_152, bytesAvailable: true, segmentCount: 2, frame: 1_625, restartCount: 1,
      fps: 25, speed: 1.01, updatedAt: 100,
    })
    useRealtimeStore.getState().applyRoomStatus({
      ...recordingStatus, state: 'RECONNECTING', recordingStatus: 'reconnecting', revision: 101, changedAt: 101,
    })
    useRealtimeStore.getState().applyRoomStatus({ ...recordingStatus, revision: 102, changedAt: 102 })

    renderPage()

    const filterGroup = screen.getByRole('group', { name: '事件类型筛选' })
    expect(filterGroup.querySelectorAll('[data-event-filter]')).toHaveLength(6)
    expect(within(filterGroup).getByRole('button', { name: '全部' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('combobox', { name: '选择直播间' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '返回直播间' })).toBeInTheDocument()
    const log = screen.getByRole('log', { name: '实时事件时间线' })
    expect(log).toHaveAttribute('tabindex', '0')
    expect(log).not.toHaveAttribute('aria-live')
    expect(within(log).getAllByTestId('event-row').length).toBeLessThan(30)
    expect(within(log).getByText(maliciousText)).toBeInTheDocument()
    expect(document.querySelector('img')).toBeNull()
    const firstOffset = within(log).getByText('+00:00:00.001')
    expect(firstOffset.closest('time')).toHaveAttribute('title', expect.stringContaining('接收时间'))
    expect(firstOffset.closest('time')).toHaveAccessibleName(/场次偏移/)
    expect(screen.getByText('00:01:05')).toBeInTheDocument()
    expect(screen.getByText('2.0 MiB')).toBeInTheDocument()
    expect(screen.getByText('连接缺口').closest('li')).toHaveClass('alert-item--warning')
    expect(screen.getByText('连接已恢复').closest('li')).toHaveClass('alert-item--info')

    const giftFilter = within(filterGroup).getByRole('button', { name: '礼物' })
    await user.click(giftFilter)
    expect(giftFilter).toHaveAttribute('aria-pressed', 'true')
    expect(within(log).getByText('送出玫瑰')).toBeInTheDocument()
    expect(within(log).queryByText(maliciousText)).not.toBeInTheDocument()
    await user.click(within(filterGroup).getByRole('button', { name: '全部' }))

    await user.click(within(filterGroup).getByRole('button', { name: '系统' }))
    expect(within(log).getByText('系统提示')).toBeInTheDocument()
    expect(within(log).getByText('暂不支持的事件')).toBeInTheDocument()
  })

  it('refreshes the clock immediately when retry countdown becomes enabled', () => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    const waitingStatus: RoomRuntimeStatus = {
      ...recordingStatus, state: 'LIVE', revision: 200, changedAt: 0,
    }
    const view = renderPage(waitingStatus)
    act(() => vi.advanceTimersByTime(10_000))
    const reconnectingStatus: RoomRuntimeStatus = {
      ...recordingStatus,
      state: 'RECONNECTING',
      recordingStatus: 'reconnecting',
      revision: 201,
      changedAt: 0,
      retryAt: 15_000,
      message: '正在重试',
    }
    try {
      view.rerender(page(reconnectingStatus))
      expect(screen.getByText('5 秒')).toBeInTheDocument()
      act(() => vi.advanceTimersByTime(1_000))
      expect(screen.getByText('4 秒')).toBeInTheDocument()
    } finally {
      view.unmount()
      vi.useRealTimers()
    }
  })

  it('lets the newest status control presentation and requires exact progress fencing', () => {
    useRealtimeStore.getState().applyRecordingProgress({
      roomId, sessionId, operationId, state: 'recording', elapsedMs: 1_000,
      bytesWritten: 2_048, bytesAvailable: true, segmentCount: 1, frame: 25, restartCount: 0,
      fps: 25, speed: 1, updatedAt: 1,
    })
    const reconnectingStatus: RoomRuntimeStatus = {
      ...recordingStatus, state: 'RECONNECTING', recordingStatus: 'reconnecting', revision: 201,
    }
    const view = renderPage(reconnectingStatus)
    expect(screen.getByText('正在恢复').closest('.recording-state')).toHaveClass('recording-state--reconnecting')
    expect(screen.getByText('2.0 KiB')).toBeInTheDocument()
    act(() => useRealtimeStore.getState().applyRecordingProgress({
      ...useRealtimeStore.getState().progressBySession[sessionId]!,
      bytesAvailable: false,
      updatedAt: 2,
    }))
    expect(screen.getByText('已写入').closest('div')).toHaveTextContent('—')
    act(() => useRealtimeStore.getState().applyRecordingProgress({
      ...useRealtimeStore.getState().progressBySession[sessionId]!,
      bytesAvailable: true,
      bytesWritten: 0,
      updatedAt: 3,
    }))
    expect(screen.getByText('0 B')).toBeInTheDocument()

    const finalizingStatus: RoomRuntimeStatus = {
      ...recordingStatus, state: 'FINALIZING', recordingStatus: 'finalizing', revision: 202,
    }
    view.rerender(page(finalizingStatus))
    expect(screen.getByText('正在收尾').closest('.recording-state')).toHaveClass('recording-state--finalizing')

    const exhaustedStatus: RoomRuntimeStatus = {
      ...recordingStatus, state: 'ERROR', recordingStatus: 'unavailable', revision: 203, message: '重试耗尽',
    }
    view.rerender(page(exhaustedStatus))
    expect(screen.getByText('录制不可用').closest('.recording-state')).toHaveClass('recording-state--unavailable')

    act(() => useRealtimeStore.setState({
      progressBySession: {
        [sessionId]: { ...useRealtimeStore.getState().progressBySession[sessionId]!, operationId: '019bce70-0c00-7000-8000-000000000004', bytesWritten: 9_999 },
      },
    }))
    expect(screen.getByText('已写入').closest('div')).toHaveTextContent('—')
    expect(screen.queryByText('9.8 KiB')).not.toBeInTheDocument()
  })

  it('chooses the higher revision when status timestamps are identical', () => {
    useRealtimeStore.getState().applyRoomStatus({
      ...recordingStatus, state: 'RECONNECTING', recordingStatus: 'reconnecting', revision: 301, changedAt: 500,
    })
    renderPage({ ...recordingStatus, state: 'FINALIZING', recordingStatus: 'finalizing', revision: 300, changedAt: 500 })
    expect(screen.getByText('正在恢复')).toBeInTheDocument()
    expect(screen.queryByText('正在收尾')).not.toBeInTheDocument()
  })

  it('shows a clear empty state when the current session has no events', () => {
    renderPage()
    expect(screen.getByText('暂时没有匹配的事件').closest('[role="status"]')).toBeInTheDocument()
  })
})
