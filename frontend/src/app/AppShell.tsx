import { Activity, AlertTriangle, BarChart3, Bug, History, Home, Moon, Radio, Settings, Sun } from 'lucide-react'
import { useState } from 'react'

import { UnavailablePage } from '../features/common/UnavailablePage'
import { DiagnosticsPage } from '../features/diagnostics/DiagnosticsPage'
import { OverviewPage } from '../features/overview/OverviewPage'
import { RealtimeRoomPage } from '../features/realtime/RealtimeRoomPage'
import { RoomsPage } from '../features/rooms/RoomsPage'
import { useRoomsDashboard } from '../features/rooms/useRoomsDashboard'
import { SessionsPage } from '../features/sessions/SessionsPage'
import { SettingsPage } from '../features/settings/SettingsPage'
import { AppEventBridge } from './AppEventBridge'
import type { BootstrapDTO } from './bootstrap'
import { useThemeStore } from './theme'

const iconByCapability = {
  overview: Home,
  rooms: Radio,
  realtime: Activity,
  sessions: History,
  analysis: BarChart3,
  diagnostics: Bug,
  settings: Settings,
} as const

type PageID = keyof typeof iconByCapability

export function AppShell({ bootstrap }: { bootstrap: BootstrapDTO }) {
  const { resolvedTheme, toggleTheme } = useThemeStore()
  const [activePage, setActivePage] = useState<PageID>('overview')
  const [openRoomEditor, setOpenRoomEditor] = useState(false)
  const [realtimeRoomId, setRealtimeRoomId] = useState<string>()
  const dashboard = useRoomsDashboard()
  const rooms = dashboard.roomsQuery.data ?? []
  const statusValues = Object.values(dashboard.statuses)
  const listening = rooms.filter((room) => room.monitorEnabled).length
  const live = statusValues.filter((status) => status.state === 'LIVE' || status.state === 'RECORDING').length
  const errors = statusValues.filter((status) => status.state === 'ERROR').length

  function addRoom() {
    setActivePage('rooms')
    setOpenRoomEditor(true)
  }

  function openRealtime(roomId?: string) {
    setRealtimeRoomId(roomId ?? rooms[0]?.id)
    setActivePage('realtime')
  }

  return (
    <div className="app-shell">
      <AppEventBridge />
      <aside className="sidebar" aria-label="主导航">
        <div className="brand">
          <div className="brand__mark" aria-hidden="true"><Activity /></div>
          <div><strong>{bootstrap.name}</strong><span>本地采集与复盘</span></div>
        </div>

        <nav className="navigation">
          {bootstrap.capabilities.map((item) => {
            const Icon = iconByCapability[item.id as keyof typeof iconByCapability] ?? Activity
            const selected = item.id === activePage
            return (
              <button
                aria-label={item.label}
                aria-current={selected ? 'page' : undefined}
                className={selected ? 'navigation__item navigation__item--active' : 'navigation__item'}
                disabled={!item.available}
                key={item.id}
                type="button"
                onClick={() => item.id === 'realtime' ? openRealtime() : setActivePage(item.id as PageID)}
              >
                <Icon aria-hidden="true" />
                <span>{item.label}</span>
                {!item.available && <small>即将开放</small>}
              </button>
            )
          })}
        </nav>

        <div className="sidebar__footer">
          <span>版本 {bootstrap.version}</span>
          <button type="button" className="icon-button" onClick={toggleTheme} aria-label="切换明暗主题">
            {resolvedTheme === 'dark' ? <Sun aria-hidden="true" /> : <Moon aria-hidden="true" />}
          </button>
        </div>
      </aside>

      <section className="workspace">
        <header className="topbar">
          <div className="topbar__status"><span className="status-dot" aria-hidden="true" />桌面服务已连接</div>
          <div className="topbar__metrics" aria-label="全局运行概览">
            <span><Radio aria-hidden="true" />监听 {listening}</span>
            <span><Activity aria-hidden="true" />直播 {live}</span>
            <span className={errors ? 'topbar__alert' : ''}>{errors ? <AlertTriangle aria-hidden="true" /> : null}{errors ? `异常 ${errors}` : '运行正常'}</span>
          </div>
        </header>
        {activePage === 'overview' && <OverviewPage data={bootstrap.data} dashboard={dashboard} onAddRoom={addRoom} />}
        {activePage === 'rooms' && <RoomsPage dashboard={dashboard} openEditor={openRoomEditor} onEditorHandled={() => setOpenRoomEditor(false)} onOpenRealtime={openRealtime} />}
        {activePage === 'realtime' && <RealtimeRoomPage rooms={rooms} statuses={dashboard.statuses} roomId={realtimeRoomId} onRoomChange={setRealtimeRoomId} onBack={() => setActivePage('rooms')} />}
        {activePage === 'settings' && <SettingsPage />}
        {activePage === 'diagnostics' && <DiagnosticsPage bootstrap={bootstrap} />}
        {activePage === 'sessions' && <SessionsPage />}
        {activePage === 'analysis' && <UnavailablePage title="分析" />}
      </section>
    </div>
  )
}
