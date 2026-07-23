import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Download, FolderLock, FolderOpen, HardDrive, RefreshCw, RotateCw, Save, Shield, X } from 'lucide-react'
import { useEffect } from 'react'
import { useForm } from 'react-hook-form'

import { useUpdateStore } from '../../app/updateStore'
import { settingsFormSchema, type SettingsFormValues } from '../../lib/contracts'
import {
  cancelUpdateDownload, checkForUpdate, getSettings, getUpdateStatus,
  installPreparedUpdate, prepareUpdate, saveSettings, selectRecordingDirectory, userFacingError,
} from '../../lib/desktop'

const defaults: SettingsFormValues = {
  recordingDirectory: '', defaultQuality: 'auto', defaultSegmentMinutes: 10,
  maxConcurrentRecordings: 1, minimumFreeSpaceGiB: 10, saveDisplayNames: true,
  automaticUpdates: true,
}

function formValues(value: Awaited<ReturnType<typeof getSettings>>): SettingsFormValues {
  return {
    recordingDirectory: value.recordingDirectory,
    defaultQuality: value.defaultQuality,
    defaultSegmentMinutes: value.defaultSegmentMinutes,
    maxConcurrentRecordings: value.maxConcurrentRecordings,
    minimumFreeSpaceGiB: value.minimumFreeSpaceGiB,
    saveDisplayNames: value.saveDisplayNames,
    automaticUpdates: value.automaticUpdates,
  }
}

const updateStateLabel = {
  disabled: '当前构建不支持更新', idle: '已是最新版本', checking: '正在检查',
  available: '发现新版本', downloading: '正在下载', ready: '已准备安装',
  installing: '正在退出并安装', failed: '更新操作失败',
} as const

export function SettingsPage() {
  const queryClient = useQueryClient()
  const eventStatus = useUpdateStore((state) => state.status)
  const setUpdateStatus = useUpdateStore((state) => state.setStatus)
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings, retry: false })
  const updateQuery = useQuery({
    queryKey: ['update-status'], queryFn: getUpdateStatus, retry: false,
  })
  const form = useForm<SettingsFormValues>({ resolver: zodResolver(settingsFormSchema), defaultValues: defaults })
  const mutation = useMutation({
    mutationFn: saveSettings,
    onSuccess: (value) => {
      queryClient.setQueryData(['settings'], value)
      form.reset(formValues(value))
    },
  })
  const directoryPicker = useMutation({
    mutationFn: () => selectRecordingDirectory(form.getValues('recordingDirectory')),
    onSuccess: (selected) => {
      if (selected) form.setValue('recordingDirectory', selected, { shouldDirty: true, shouldTouch: true, shouldValidate: true })
    },
  })
  const updateMutationOptions = {
    onSuccess: (value: Awaited<ReturnType<typeof getUpdateStatus>>) => {
      setUpdateStatus(value)
      queryClient.setQueryData(['update-status'], value)
    },
  }
  const checkMutation = useMutation({ mutationFn: checkForUpdate, ...updateMutationOptions })
  const prepareMutation = useMutation({ mutationFn: prepareUpdate, ...updateMutationOptions })
  const cancelMutation = useMutation({ mutationFn: cancelUpdateDownload, ...updateMutationOptions })
  const installMutation = useMutation({ mutationFn: installPreparedUpdate, ...updateMutationOptions })
  const updateStatus = eventStatus ?? updateQuery.data
  const updateError = checkMutation.error ?? prepareMutation.error ?? cancelMutation.error ?? installMutation.error
  const updatePending = checkMutation.isPending || prepareMutation.isPending ||
    cancelMutation.isPending || installMutation.isPending
  const progress = updateStatus?.totalBytes
    ? Math.min(100, Math.round(((updateStatus.downloadedBytes ?? 0) / updateStatus.totalBytes) * 100))
    : 0

  useEffect(() => {
    if (settings.data) form.reset(formValues(settings.data))
  }, [form, settings.data])
  useEffect(() => {
    if (updateQuery.data) setUpdateStatus(updateQuery.data)
  }, [setUpdateStatus, updateQuery.data])

  return (
    <main className="page page--narrow">
      <div className="page__heading"><div><p className="eyebrow">应用配置</p><h1>设置</h1><p>管理本地录制目录、默认质量和隐私选项。</p></div></div>
      {settings.isPending && <div className="page-state">正在读取设置…</div>}
      {settings.isError && <div className="page-state page-state--error" role="alert">{userFacingError(settings.error)}</div>}
      {settings.data && (
        <form className="settings-layout" onSubmit={form.handleSubmit((values) => mutation.mutate(values))}>
          <section className="settings-section">
            <div className="settings-section__heading"><HardDrive aria-hidden="true" /><div><h2>存储与录制</h2><p>目录必须是 Windows 绝对路径；保存时会创建目录并验证可写性。</p></div></div>
            <div className="form-grid">
              <div className="field field--wide">
                <label htmlFor="recording-directory">录制目录</label>
                <div className="directory-control">
                  <input id="recording-directory" {...form.register('recordingDirectory')} />
                  <button className="button" disabled={directoryPicker.isPending} type="button" onClick={() => directoryPicker.mutate()}>
                    <FolderOpen aria-hidden="true" />选择文件夹
                  </button>
                </div>
                <small>{form.formState.errors.recordingDirectory?.message || '新启动的录制会使用保存后的目录；正在录制的场次不受影响。'}</small>
              </div>
              {directoryPicker.isError && <div className="inline-alert field--wide" role="alert">{userFacingError(directoryPicker.error)}</div>}
              <label className="field"><span>默认录制质量</span><select {...form.register('defaultQuality')}><option value="auto">自动选择</option><option value="original">原画</option><option value="ultra">超清</option><option value="high">高清</option><option value="standard">标清</option></select></label>
              <label className="field"><span>默认分片时长</span><div className="input-suffix"><input type="number" min="5" max="30" {...form.register('defaultSegmentMinutes', { valueAsNumber: true })} /><span>分钟</span></div></label>
              <label className="field"><span>并发录制上限</span><input type="number" min="1" max="4" {...form.register('maxConcurrentRecordings', { valueAsNumber: true })} /></label>
              <label className="field"><span>最小剩余空间</span><div className="input-suffix"><input type="number" min="1" max="1024" {...form.register('minimumFreeSpaceGiB', { valueAsNumber: true })} /><span>GiB</span></div></label>
            </div>
          </section>
          <section className="settings-section">
            <div className="settings-section__heading"><RefreshCw aria-hidden="true" /><div><h2>自动更新</h2><p>仅访问官方 OSS 公开只读前缀；安装始终需要你的确认。</p></div></div>
            <label className="switch-row"><div><strong>自动检查并后台下载</strong><span>关闭后不发起后台请求，仍可手动检查。</span></div><input type="checkbox" {...form.register('automaticUpdates')} /></label>
            <div className="update-panel">
              <div className="update-panel__summary">
                <div><strong>{updateStatus ? updateStateLabel[updateStatus.state] : '正在读取更新状态'}</strong><span>当前版本 {updateStatus?.currentVersion ?? '—'}{updateStatus?.availableVersion ? ` · 可用版本 ${updateStatus.availableVersion}` : ''}</span></div>
                {updateStatus?.publishedAt && <time>{new Date(updateStatus.publishedAt).toLocaleString()}</time>}
              </div>
              {updateStatus?.state === 'downloading' && (
                <div className="update-progress" aria-label={`更新下载进度 ${progress}%`}>
                  <span style={{ width: `${progress}%` }} />
                </div>
              )}
              {updateStatus?.releaseNotes && <pre className="update-notes">{updateStatus.releaseNotes}</pre>}
              {updateStatus?.installBlocked && <div className="inline-alert">当前暂不能安装：{updateStatus.blockReason}</div>}
              {updateError && <div className="inline-alert" role="alert">{userFacingError(updateError)}</div>}
              {updateStatus?.errorCode && <div className="inline-alert" role="alert">错误码：{updateStatus.errorCode}</div>}
              <div className="update-actions">
                <button className="button" disabled={updatePending || updateStatus?.state === 'checking' || updateStatus?.state === 'downloading' || updateStatus?.state === 'installing'} type="button" onClick={() => checkMutation.mutate()}><RefreshCw aria-hidden="true" />立即检查</button>
                {updateStatus?.state === 'available' && <button className="button button--primary" disabled={updatePending || updateStatus.installBlocked} type="button" onClick={() => prepareMutation.mutate()}><Download aria-hidden="true" />下载更新</button>}
                {updateStatus?.state === 'downloading' && <button className="button" disabled={cancelMutation.isPending} type="button" onClick={() => cancelMutation.mutate()}><X aria-hidden="true" />取消下载</button>}
                {updateStatus?.state === 'ready' && <button className="button button--primary" disabled={updatePending || updateStatus.installBlocked} type="button" onClick={() => installMutation.mutate()}><RotateCw aria-hidden="true" />安装并重启</button>}
              </div>
            </div>
          </section>
          <section className="settings-section">
            <div className="settings-section__heading"><Shield aria-hidden="true" /><div><h2>隐私</h2><p>敏感凭据始终由 Go 服务加密保存，前端无法读取。</p></div></div>
            <label className="switch-row"><div><strong>保存显示名称</strong><span>关闭后，后续事件将优先使用脱敏显示名。</span></div><input type="checkbox" {...form.register('saveDisplayNames')} /></label>
            <div className="privacy-note"><FolderLock aria-hidden="true" /><div><strong>本地优先</strong><p>Cookie、签名和完整流地址不会写入 UI 状态、数据库或诊断导出。</p></div></div>
          </section>
          {mutation.isError && <div className="inline-alert" role="alert">{userFacingError(mutation.error)}</div>}
          <div className="settings-actions"><span>{mutation.isSuccess && !form.formState.isDirty ? '设置已保存' : '更改仅在保存后生效'}</span><button className="button button--primary" disabled={mutation.isPending || !form.formState.isDirty} type="submit"><Save aria-hidden="true" />{mutation.isPending ? '正在保存…' : '保存设置'}</button></div>
        </form>
      )}
    </main>
  )
}
