import { useQuery } from '@tanstack/react-query'
import { AlertTriangle } from 'lucide-react'

import { AppShell } from './AppShell'
import { loadBootstrap } from './bootstrap'

export function App() {
  const bootstrap = useQuery({
    queryKey: ['application', 'bootstrap'],
    queryFn: loadBootstrap,
    retry: false,
    staleTime: Number.POSITIVE_INFINITY,
  })

  if (bootstrap.isPending) {
    return <main className="startup-state">正在启动桌面服务…</main>
  }

  if (bootstrap.isError) {
    return (
      <main className="startup-state startup-state--error" role="alert">
        <AlertTriangle aria-hidden="true" />
        <div>
          <h1>桌面服务未就绪</h1>
          <p>请重新启动应用；若问题持续，请在后续诊断页导出日志。</p>
        </div>
      </main>
    )
  }

  return <AppShell bootstrap={bootstrap.data} />
}
