import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Cookie, Edit3, Plus, Radio, Search, ShieldCheck, Trash2, X } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useForm } from 'react-hook-form'

import { useRoomStatusStore } from '../../app/roomStatus'
import { roomFormSchema, type RoomConfig, type RoomFormValues } from '../../lib/contracts'
import { clearRoomCookie, getRoomStatus, removeRoom, saveRoom, startMonitoring, stopMonitoring, userFacingError } from '../../lib/desktop'
import { RoomStatusBadge } from './RoomStatusBadge'
import type { RoomsDashboard } from './useRoomsDashboard'

type Editor = RoomConfig | 'new' | null

const defaults: RoomFormValues = {
  liveId: '', alias: '', monitorEnabled: true, recordEnabled: true,
  quality: 'auto', segmentMinutes: 10, cookie: '',
}

function editorValues(editor: Editor): RoomFormValues {
  if (!editor || editor === 'new') return defaults
  return {
    liveId: editor.liveId,
    alias: editor.alias,
    monitorEnabled: editor.monitorEnabled,
    recordEnabled: editor.recordEnabled,
    quality: editor.recordingProfile.quality,
    segmentMinutes: editor.recordingProfile.segmentMinutes,
    cookie: '',
  }
}

export function RoomsPage({ dashboard, openEditor, onEditorHandled }: { dashboard: RoomsDashboard; openEditor: boolean; onEditorHandled: () => void }) {
  const [editor, setEditor] = useState<Editor>(null)
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState<'all' | 'active' | 'error'>('all')
  const [actionRoom, setActionRoom] = useState<string>()
  const queryClient = useQueryClient()
  const updateStatus = useRoomStatusStore((state) => state.update)
  const removeStatus = useRoomStatusStore((state) => state.remove)
  const form = useForm<RoomFormValues>({ resolver: zodResolver(roomFormSchema), defaultValues: defaults })

  useEffect(() => {
    if (openEditor) {
      setEditor('new')
      onEditorHandled()
    }
  }, [onEditorHandled, openEditor])

  useEffect(() => { form.reset(editorValues(editor)) }, [editor, form])

  const saveMutation = useMutation({
    mutationFn: ({ id, values }: { id?: string; values: RoomFormValues }) => saveRoom(id, values),
    onSuccess: async (room) => {
      form.reset({ ...defaults, cookie: '' })
      setEditor(null)
      await queryClient.invalidateQueries({ queryKey: ['rooms'] })
      try { updateStatus(await getRoomStatus(room.id)) } catch { /* event/query will retry on navigation */ }
    },
  })

  const rooms = dashboard.roomsQuery.data ?? []
  const visibleRooms = useMemo(() => rooms.filter((room) => {
    const status = dashboard.statuses[room.id]
    const matchesSearch = `${room.alias} ${room.liveId} ${room.anchorName ?? ''}`.toLowerCase().includes(search.trim().toLowerCase())
    if (!matchesSearch) return false
    if (filter === 'active') return room.monitorEnabled
    if (filter === 'error') return status?.state === 'ERROR'
    return true
  }), [dashboard.statuses, filter, rooms, search])

  async function toggleMonitoring(room: RoomConfig) {
    setActionRoom(room.id)
    try {
      if (room.monitorEnabled) await stopMonitoring(room.id)
      else await startMonitoring(room.id)
      await queryClient.invalidateQueries({ queryKey: ['rooms'] })
      updateStatus(await getRoomStatus(room.id))
    } finally { setActionRoom(undefined) }
  }

  async function deleteRoom(room: RoomConfig) {
    if (!window.confirm(`确定删除“${room.alias}”的配置吗？历史数据将保留。`)) return
    setActionRoom(room.id)
    try {
      await removeRoom(room.id)
      removeStatus(room.id)
      await queryClient.invalidateQueries({ queryKey: ['rooms'] })
    } finally { setActionRoom(undefined) }
  }

  async function clearCookie(room: RoomConfig) {
    if (!window.confirm(`清除“${room.alias}”保存的 Cookie？`)) return
    setActionRoom(room.id)
    try {
      await clearRoomCookie(room.id)
      await queryClient.invalidateQueries({ queryKey: ['rooms'] })
    } finally { setActionRoom(undefined) }
  }

  return (
    <main className="page">
      <div className="page__heading">
        <div><p className="eyebrow">直播间管理</p><h1>直播间</h1><p>配置自动监听和录制策略，运行状态会实时更新。</p></div>
        <button className="button button--primary" type="button" onClick={() => setEditor('new')}><Plus aria-hidden="true" />添加直播间</button>
      </div>

      <div className="filter-bar">
        <label className="search-field"><Search aria-hidden="true" /><span className="sr-only">搜索直播间</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索别名或 Live ID" /></label>
        <div className="segmented" aria-label="状态筛选">
          {([['all', '全部'], ['active', '监听中'], ['error', '需处理']] as const).map(([value, label]) => <button className={filter === value ? 'is-active' : ''} type="button" onClick={() => setFilter(value)} key={value}>{label}</button>)}
        </div>
      </div>

      {dashboard.roomsQuery.isPending && <div className="page-state">正在读取直播间…</div>}
      {dashboard.roomsQuery.isError && <div className="page-state page-state--error" role="alert">{userFacingError(dashboard.roomsQuery.error)}</div>}
      {!dashboard.roomsQuery.isPending && rooms.length === 0 && <section className="empty-panel"><div><h2>还没有直播间</h2><p>添加 Live ID 后即可开始等待开播。</p></div><button type="button" onClick={() => setEditor('new')}><Plus aria-hidden="true" />添加第一个直播间</button></section>}
      {rooms.length > 0 && visibleRooms.length === 0 && <div className="page-state">没有符合当前筛选条件的直播间。</div>}

      <section className="room-grid" aria-label="直播间列表">
        {visibleRooms.map((room) => {
          const status = dashboard.statuses[room.id]
          const busy = actionRoom === room.id
          const sessionLocked = Boolean(status?.sessionId) || status?.state === 'STARTING' || status?.state === 'FINALIZING'
          return (
            <article className="room-card" key={room.id}>
              <div className="room-card__heading"><div className="room-avatar"><Radio aria-hidden="true" /></div><div><h2>{room.alias}</h2><p>Live ID · {room.liveId}</p></div><RoomStatusBadge status={status} fallback={room.monitorEnabled ? 'WAITING' : 'STOPPED'} /></div>
              <div className="room-card__message"><strong>{status?.title || status?.message || (room.monitorEnabled ? '正在获取运行状态' : '监控未启用')}</strong><span>{status?.liveName || room.anchorName || '尚未获取主播信息'}</span></div>
              <dl className="room-card__facts"><div><dt>自动录制</dt><dd>{room.recordEnabled ? '已启用' : '已关闭'}</dd></div><div><dt>录制质量</dt><dd>{qualityLabel(room.recordingProfile.quality)}</dd></div><div><dt>Cookie</dt><dd>{room.cookie.configured ? '已安全保存' : '未配置'}</dd></div></dl>
              {status?.state === 'ERROR' && <div className="inline-alert" role="alert">{status.message}{status.errorCode && <small>{status.errorCode}</small>}</div>}
              <div className="room-card__actions">
                <button className={room.monitorEnabled ? 'button' : 'button button--primary'} disabled={busy} type="button" onClick={() => void toggleMonitoring(room)}>{room.monitorEnabled ? '停止监控' : '开始监控'}</button>
                <button className="icon-button" aria-label={`编辑 ${room.alias}`} disabled={busy || sessionLocked} type="button" onClick={() => setEditor(room)}><Edit3 aria-hidden="true" /></button>
                {room.cookie.configured && <button className="icon-button" aria-label={`清除 ${room.alias} Cookie`} disabled={busy} type="button" onClick={() => void clearCookie(room)}><Cookie aria-hidden="true" /></button>}
                <button className="icon-button icon-button--danger" aria-label={`删除 ${room.alias}`} disabled={busy || sessionLocked} type="button" onClick={() => void deleteRoom(room)}><Trash2 aria-hidden="true" /></button>
              </div>
            </article>
          )
        })}
      </section>

      {editor && (
        <div className="modal-backdrop" role="presentation">
          <section className="modal" role="dialog" aria-modal="true" aria-labelledby="room-editor-title">
            <div className="modal__heading"><div><p className="eyebrow">{editor === 'new' ? '新增配置' : '编辑配置'}</p><h2 id="room-editor-title">{editor === 'new' ? '添加直播间' : `编辑 ${editor.alias}`}</h2></div><button className="icon-button" type="button" aria-label="关闭" onClick={() => setEditor(null)}><X aria-hidden="true" /></button></div>
            <form onSubmit={form.handleSubmit((values) => saveMutation.mutate({ id: editor === 'new' ? undefined : editor.id, values }))}>
              <div className="form-grid">
                <label className="field field--wide"><span>Live ID 或直播间 URL</span><input autoFocus {...form.register('liveId')} placeholder="live.douyin.com/…" /><small>{form.formState.errors.liveId?.message || '仅保留直播间标识，不显示平台内部 Room ID。'}</small></label>
                <label className="field field--wide"><span>别名</span><input {...form.register('alias')} placeholder="例如：主直播间" /><small>{form.formState.errors.alias?.message || '留空时使用 Live ID。'}</small></label>
                <label className="field"><span>录制质量</span><select {...form.register('quality')}><option value="auto">自动选择</option><option value="original">原画</option><option value="ultra">超清</option><option value="high">高清</option><option value="standard">标清</option></select></label>
                <label className="field"><span>分片时长（分钟）</span><input type="number" min="1" max="60" {...form.register('segmentMinutes', { valueAsNumber: true })} /></label>
                <label className="field field--wide"><span>Cookie（可选）</span><input type="password" autoComplete="off" {...form.register('cookie')} placeholder={editor !== 'new' && editor.cookie.configured ? '已配置；留空保持不变' : '仅在必要时配置'} /><small>{form.formState.errors.cookie?.message || '保存后立即清空输入；应用不会回显 Cookie。'}</small></label>
              </div>
              <div className="check-row"><label><input type="checkbox" {...form.register('monitorEnabled')} />保存后自动等待开播</label><label><input type="checkbox" {...form.register('recordEnabled')} />开播后自动录制</label></div>
              {saveMutation.isError && <div className="inline-alert" role="alert">{userFacingError(saveMutation.error)}</div>}
              <div className="modal__actions"><span><ShieldCheck aria-hidden="true" />凭据仅在 Go 服务中加密处理</span><button className="button" type="button" onClick={() => setEditor(null)}>取消</button><button className="button button--primary" disabled={saveMutation.isPending} type="submit">{saveMutation.isPending ? '正在保存…' : '保存配置'}</button></div>
            </form>
          </section>
        </div>
      )}
    </main>
  )
}

function qualityLabel(value: string) {
  return ({ auto: '自动', original: '原画', ultra: '超清', high: '高清', standard: '标清' } as Record<string, string>)[value] ?? value
}
