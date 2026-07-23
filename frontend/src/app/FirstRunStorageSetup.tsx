import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { FolderOpen, HardDrive, ShieldCheck } from 'lucide-react'
import { FormEvent, useEffect, useState } from 'react'

import {
  getSettings, saveSettings, selectRecordingDirectory, userFacingError,
} from '../lib/desktop'

export function FirstRunStorageSetup() {
  const queryClient = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings, retry: false })
  const [directory, setDirectory] = useState('')
  const picker = useMutation({
    mutationFn: () => selectRecordingDirectory(directory),
    onSuccess: (selected) => {
      if (selected) setDirectory(selected)
    },
  })
  const confirmation = useMutation({
    mutationFn: async () => {
      if (!settings.data) throw new Error('SETTINGS_SERVICE_UNAVAILABLE')
      return saveSettings({
        recordingDirectory: directory,
        defaultQuality: settings.data.defaultQuality,
        defaultSegmentMinutes: settings.data.defaultSegmentMinutes,
        maxConcurrentRecordings: settings.data.maxConcurrentRecordings,
        minimumFreeSpaceGiB: settings.data.minimumFreeSpaceGiB,
        saveDisplayNames: settings.data.saveDisplayNames,
        automaticUpdates: settings.data.automaticUpdates,
      })
    },
    onSuccess: (value) => queryClient.setQueryData(['settings'], value),
  })

  useEffect(() => {
    if (settings.data && !directory) setDirectory(settings.data.recordingDirectory)
  }, [directory, settings.data])

  if (settings.data?.recordingDirectoryConfirmed) return null

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (directory.trim()) confirmation.mutate()
  }

  return (
    <div className="modal-backdrop first-run-backdrop">
      <section
        aria-labelledby="first-run-storage-title"
        aria-modal="true"
        className="modal first-run-storage"
        role="dialog"
      >
        <div className="first-run-storage__icon" aria-hidden="true"><HardDrive /></div>
        <p className="eyebrow">首次使用设置</p>
        <h2 id="first-run-storage-title">选择录制文件的保存位置</h2>
        <p className="first-run-storage__intro">
          直播录像可能占用较多空间。请选择容量充足、长期可用的本地磁盘目录。
        </p>

        {settings.isPending && <div className="page-state">正在读取默认目录…</div>}
        {settings.isError && (
          <div className="first-run-storage__error" role="alert">
            <span>{userFacingError(settings.error)}</span>
            <button className="button" type="button" onClick={() => settings.refetch()}>重试</button>
          </div>
        )}
        {settings.data && (
          <form onSubmit={submit}>
            <label className="field" htmlFor="first-run-recording-directory">
              <span>录制目录</span>
            </label>
            <div className="directory-control">
              <input
                autoFocus
                id="first-run-recording-directory"
                required
                value={directory}
                onChange={(event) => setDirectory(event.target.value)}
              />
              <button
                className="button"
                disabled={picker.isPending || confirmation.isPending}
                type="button"
                onClick={() => picker.mutate()}
              >
                <FolderOpen aria-hidden="true" />选择文件夹
              </button>
            </div>
            <small className="first-run-storage__hint">
              保存时会创建目录并验证可写性。之后可在“设置 → 存储与录制”中修改。
            </small>
            <div className="first-run-storage__note">
              <ShieldCheck aria-hidden="true" />
              <span>仅录像媒体保存在此目录；应用配置、数据库和日志仍保存在受保护的应用数据目录。</span>
            </div>
            {picker.isError && <div className="inline-alert" role="alert">{userFacingError(picker.error)}</div>}
            {confirmation.isError && <div className="inline-alert" role="alert">{userFacingError(confirmation.error)}</div>}
            <div className="modal__actions first-run-storage__actions">
              <span>确认后，新启动的录制会立即使用此目录。</span>
              <button
                className="button button--primary"
                disabled={confirmation.isPending || !directory.trim()}
                type="submit"
              >
                {confirmation.isPending ? '正在验证…' : '确认并开始使用'}
              </button>
            </div>
          </form>
        )}
      </section>
    </div>
  )
}
