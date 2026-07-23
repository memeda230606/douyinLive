import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { AppShell } from './AppShell'
import type { BootstrapDTO } from './bootstrap'

vi.mock('../lib/desktop', () => ({
  listRooms: vi.fn(async () => []),
  getRoomStatus: vi.fn(),
  getSettings: vi.fn(async () => ({
    version: 1, storageRoot: 'C:\\Data', recordingDirectory: 'C:\\Recordings',
    defaultQuality: 'auto', defaultSegmentMinutes: 10, maxConcurrentRecordings: 1,
    minimumFreeSpaceGiB: 10, saveDisplayNames: true, automaticUpdates: true,
  })),
  getUpdateStatus: vi.fn(async () => ({
    version: 1, state: 'idle', currentVersion: '0.2.0', installBlocked: false,
  })),
  userFacingError: vi.fn(() => '操作失败'),
}))

vi.mock('../generated/wailsjs/runtime/runtime', () => ({
  EventsOn: vi.fn(() => vi.fn()),
  LogError: vi.fn(),
}))

const bootstrap: BootstrapDTO = {
  apiVersion: 'v1',
  name: '抖音直播分析',
  version: 'test',
  build: {
    productVersion: 'test', gitCommit: 'unknown', buildTime: 'unknown', buildSource: 'local',
    goVersion: 'go1.26.4', wailsVersion: '2.13.0', nodeVersion: 'v24.18.0',
    ffmpegVersion: '8.1.2-essentials_build-www.gyan.dev', ffmpegSHA256: 'a'.repeat(64), ffmpegLicense: 'GPL-3.0-or-later',
    databaseSchemaVersion: 6, settingsSchemaVersion: 3, analysisAlgorithmVersion: 'basic-analysis/v1', exportSchemaVersion: 'analysis-export/v1',
  },
  state: 'RUNNING',
  data: { ready: true, schemaVersion: 1, mode: 'READ_WRITE', loggingReady: true },
  capabilities: [
    { id: 'overview', label: '总览', available: true },
    { id: 'rooms', label: '直播间', available: true },
    { id: 'realtime', label: '实时', available: true },
    { id: 'sessions', label: '历史场次', available: false },
    { id: 'analysis', label: '分析', available: false },
    { id: 'diagnostics', label: '诊断', available: true },
    { id: 'settings', label: '设置', available: true },
  ],
}

describe('AppShell', () => {
  it('renders navigation, empty state and the room form without exposing a cookie value', async () => {
    const user = userEvent.setup()
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(<QueryClientProvider client={queryClient}><AppShell bootstrap={bootstrap} /></QueryClientProvider>)
    expect(screen.getByText('抖音直播分析')).toBeInTheDocument()
    expect(screen.getByText('版本 test')).toBeInTheDocument()
    const navigation = screen.getByRole('navigation')
    expect(within(navigation).getByRole('button', { name: /历史场次/ })).toBeDisabled()
    expect(screen.getByRole('heading', { name: '直播间运行总览' })).toBeInTheDocument()
    expect(await screen.findByText('添加第一个直播间')).toBeInTheDocument()

    const realtimeNavigation = within(navigation).getByRole('button', { name: '实时' })
    expect(realtimeNavigation).toHaveAttribute('aria-label', '实时')
    await user.click(realtimeNavigation)
    expect(screen.getByRole('heading', { name: '没有可查看的直播间' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '返回直播间' }))

    await user.click(within(navigation).getByRole('button', { name: '直播间' }))
    expect(screen.getByRole('heading', { name: '直播间', level: 1 })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '添加直播间' }))
    expect(screen.getByRole('dialog', { name: '添加直播间' })).toBeInTheDocument()
    const cookie = screen.getByLabelText(/Cookie（可选）/)
    expect(cookie).toHaveAttribute('type', 'password')
    expect(cookie).toHaveValue('')
  })
})
