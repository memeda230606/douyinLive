import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { useEffect, useState, type ReactNode } from 'react'

import { useThemeStore } from './theme'

export function AppProviders({ children }: { children: ReactNode }) {
  const [queryClient] = useState(() => new QueryClient())
  const syncTheme = useThemeStore((state) => state.syncTheme)

  useEffect(() => {
    const media = window.matchMedia('(prefers-color-scheme: dark)')
    syncTheme(media.matches)
    const listener = (event: MediaQueryListEvent) => syncTheme(event.matches)
    media.addEventListener('change', listener)
    return () => media.removeEventListener('change', listener)
  }, [syncTheme])

  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
}
