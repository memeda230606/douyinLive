import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { FirstRunStorageSetup } from './FirstRunStorageSetup'

const desktop = vi.hoisted(() => ({
  getSettings: vi.fn(),
  saveSettings: vi.fn(),
  selectRecordingDirectory: vi.fn(),
  userFacingError: vi.fn(() => '操作失败'),
}))

vi.mock('../lib/desktop', () => desktop)

const initialSettings = {
  version: 4,
  storageRoot: 'C:\\Users\\tester\\AppData\\Local\\DouyinLive',
  recordingDirectory: 'C:\\Users\\tester\\AppData\\Local\\DouyinLive\\rooms',
  recordingDirectoryConfirmed: false,
  defaultQuality: 'auto' as const,
  defaultSegmentMinutes: 10,
  maxConcurrentRecordings: 1,
  minimumFreeSpaceGiB: 10,
  saveDisplayNames: true,
  automaticUpdates: true,
}

describe('FirstRunStorageSetup', () => {
  it('blocks first use until a writable recording directory is confirmed', async () => {
    desktop.getSettings.mockResolvedValue(initialSettings)
    desktop.selectRecordingDirectory.mockResolvedValue('D:\\LiveRecordings')
    desktop.saveSettings.mockImplementation(async (values) => ({
      ...initialSettings,
      ...values,
      recordingDirectoryConfirmed: true,
    }))
    const user = userEvent.setup()
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={queryClient}>
        <FirstRunStorageSetup />
      </QueryClientProvider>,
    )

    const dialog = await screen.findByRole('dialog', { name: '选择录制文件的保存位置' })
    expect(dialog).toBeInTheDocument()
    expect(await screen.findByLabelText('录制目录')).toHaveValue(initialSettings.recordingDirectory)

    await user.click(screen.getByRole('button', { name: '选择文件夹' }))
    expect(screen.getByLabelText('录制目录')).toHaveValue('D:\\LiveRecordings')
    await user.click(screen.getByRole('button', { name: '确认并开始使用' }))

    expect(desktop.saveSettings).toHaveBeenCalledWith({
      recordingDirectory: 'D:\\LiveRecordings',
      defaultQuality: 'auto',
      defaultSegmentMinutes: 10,
      maxConcurrentRecordings: 1,
      minimumFreeSpaceGiB: 10,
      saveDisplayNames: true,
      automaticUpdates: true,
    })
    await waitFor(() => {
      expect(screen.queryByRole('dialog', { name: '选择录制文件的保存位置' })).not.toBeInTheDocument()
    })
  })
})
