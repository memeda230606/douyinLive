import { Activity, BarChart3, Bug, History, Home, Moon, Radio, Settings, Sun } from 'lucide-react'

import { OverviewPage } from '../features/overview/OverviewPage'
import type { BootstrapDTO } from './bootstrap'
import { useThemeStore } from './theme'

const iconByCapability = {
  overview: Home,
  rooms: Radio,
  sessions: History,
  analysis: BarChart3,
  diagnostics: Bug,
  settings: Settings,
} as const

export function AppShell({ bootstrap }: { bootstrap: BootstrapDTO }) {
  const { resolvedTheme, toggleTheme } = useThemeStore()

  return (
    <div className="app-shell">
      <aside className="sidebar" aria-label="主导航">
        <div className="brand">
          <div className="brand__mark" aria-hidden="true"><Activity /></div>
          <div><strong>{bootstrap.name}</strong><span>本地采集与复盘</span></div>
        </div>

        <nav className="navigation">
          {bootstrap.capabilities.map((item) => {
            const Icon = iconByCapability[item.id as keyof typeof iconByCapability] ?? Activity
            return (
              <button
                className={item.id === 'overview' ? 'navigation__item navigation__item--active' : 'navigation__item'}
                disabled={!item.available}
                key={item.id}
                type="button"
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
          <div className="topbar__metrics" aria-label="运行概览">
            <span>监听 0</span><span>录制 0</span><span>无后台任务</span>
          </div>
        </header>
        <OverviewPage data={bootstrap.data} />
      </section>
    </div>
  )
}
