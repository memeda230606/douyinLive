import { Component, type ErrorInfo, type ReactNode } from 'react'
import { AlertTriangle } from 'lucide-react'

type ErrorBoundaryState = { failed: boolean }

export class ErrorBoundary extends Component<{ children: ReactNode }, ErrorBoundaryState> {
  state: ErrorBoundaryState = { failed: false }

  static getDerivedStateFromError(): ErrorBoundaryState { return { failed: true } }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('UI_CONTRACT_INVALID', error, info.componentStack)
  }

  render() {
    if (this.state.failed) {
      return (
        <main className="startup-state startup-state--error" role="alert">
          <AlertTriangle aria-hidden="true" />
          <div><h1>界面发生错误</h1><p>后台任务未被自动停止。请重新启动应用恢复界面。</p></div>
        </main>
      )
    }
    return this.props.children
  }
}
