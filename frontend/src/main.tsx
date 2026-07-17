import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { App } from './app/App'
import { ErrorBoundary } from './app/ErrorBoundary'
import { AppProviders } from './app/providers'
import './styles/index.css'

const root = document.getElementById('root')
if (!root) {
  throw new Error('UI_CONTRACT_INVALID: missing root element')
}

createRoot(root).render(
  <StrictMode>
    <ErrorBoundary>
      <AppProviders><App /></AppProviders>
    </ErrorBoundary>
  </StrictMode>,
)
