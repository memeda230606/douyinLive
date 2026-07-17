import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { FolderLock, HardDrive, Save, Shield } from 'lucide-react'
import { useEffect } from 'react'
import { useForm } from 'react-hook-form'

import { settingsFormSchema, type SettingsFormValues } from '../../lib/contracts'
import { getSettings, saveSettings, userFacingError } from '../../lib/desktop'

const defaults: SettingsFormValues = {
  recordingDirectory: '', defaultQuality: 'auto', defaultSegmentMinutes: 10,
  maxConcurrentRecordings: 1, minimumFreeSpaceGiB: 10, saveDisplayNames: true,
}

function formValues(value: Awaited<ReturnType<typeof getSettings>>): SettingsFormValues {
  return {
    recordingDirectory: value.recordingDirectory,
    defaultQuality: value.defaultQuality,
    defaultSegmentMinutes: value.defaultSegmentMinutes,
    maxConcurrentRecordings: value.maxConcurrentRecordings,
    minimumFreeSpaceGiB: value.minimumFreeSpaceGiB,
    saveDisplayNames: value.saveDisplayNames,
  }
}

export function SettingsPage() {
  const queryClient = useQueryClient()
  const settings = useQuery({ queryKey: ['settings'], queryFn: getSettings, retry: false })
  const form = useForm<SettingsFormValues>({ resolver: zodResolver(settingsFormSchema), defaultValues: defaults })
  const mutation = useMutation({
    mutationFn: saveSettings,
    onSuccess: (value) => {
      queryClient.setQueryData(['settings'], value)
      form.reset(formValues(value))
    },
  })

  useEffect(() => {
    if (settings.data) form.reset(formValues(settings.data))
  }, [form, settings.data])

  return (
    <main className="page page--narrow">
      <div className="page__heading"><div><p className="eyebrow">应用配置</p><h1>设置</h1><p>管理本地录制目录、默认质量和隐私选项。</p></div></div>
      {settings.isPending && <div className="page-state">正在读取设置…</div>}
      {settings.isError && <div className="page-state page-state--error" role="alert">{userFacingError(settings.error)}</div>}
      {settings.data && (
        <form className="settings-layout" onSubmit={form.handleSubmit((values) => mutation.mutate(values))}>
          <section className="settings-section">
            <div className="settings-section__heading"><HardDrive aria-hidden="true" /><div><h2>存储与录制</h2><p>目录必须是 Windows 绝对路径，并在保存时验证可写性。</p></div></div>
            <div className="form-grid">
              <p className="field field--wide">当前录制仅支持应用数据目录内的场次媒体目录；外部录制根将在媒体清单阶段启用。</p>
              <label className="field field--wide"><span>录制目录</span><input {...form.register('recordingDirectory')} /><small>{form.formState.errors.recordingDirectory?.message || `应用数据目录：${settings.data.storageRoot}`}</small></label>
              <label className="field"><span>默认录制质量</span><select {...form.register('defaultQuality')}><option value="auto">自动选择</option><option value="original">原画</option><option value="ultra">超清</option><option value="high">高清</option><option value="standard">标清</option></select></label>
              <label className="field"><span>默认分片时长</span><div className="input-suffix"><input type="number" min="5" max="30" {...form.register('defaultSegmentMinutes', { valueAsNumber: true })} /><span>分钟</span></div></label>
              <label className="field"><span>并发录制上限</span><input type="number" min="1" max="4" {...form.register('maxConcurrentRecordings', { valueAsNumber: true })} /></label>
              <label className="field"><span>最小剩余空间</span><div className="input-suffix"><input type="number" min="1" max="1024" {...form.register('minimumFreeSpaceGiB', { valueAsNumber: true })} /><span>GiB</span></div></label>
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
