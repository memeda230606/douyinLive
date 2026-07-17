import { render, screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'

import { AppShell } from './AppShell'
import type { BootstrapDTO } from './bootstrap'

const bootstrap: BootstrapDTO = {
  apiVersion: 'v1',
  name: '抖音直播分析',
  version: 'test',
  state: 'RUNNING',
  data: { ready: true, schemaVersion: 1, mode: 'READ_WRITE', loggingReady: true },
  capabilities: [
    { id: 'overview', label: '总览', available: true },
    { id: 'rooms', label: '直播间', available: false },
  ],
}

describe('AppShell', () => {
  it('renders product identity and capability states', () => {
    render(<AppShell bootstrap={bootstrap} />)
    expect(screen.getByText('抖音直播分析')).toBeInTheDocument()
    expect(screen.getByText('版本 test')).toBeInTheDocument()
    const navigation = screen.getByRole('navigation')
    expect(within(navigation).getByRole('button', { name: /直播间/ })).toBeDisabled()
    expect(screen.getByText('数据基础已就绪')).toBeInTheDocument()
    expect(screen.getByText('SQLite Schema v1 · JSONL 日志已启用')).toBeInTheDocument()
  })
})
