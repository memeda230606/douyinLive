import { create } from 'zustand'

type ResolvedTheme = 'light' | 'dark'
type ThemeState = {
  preference: 'system' | ResolvedTheme
  resolvedTheme: ResolvedTheme
  syncTheme: (systemIsDark: boolean) => void
  toggleTheme: () => void
}

function storedPreference(): ThemeState['preference'] {
  const value = window.localStorage.getItem('douyin-live-theme')
  return value === 'light' || value === 'dark' ? value : 'system'
}

function applyTheme(theme: ResolvedTheme) {
  document.documentElement.dataset.theme = theme
  document.documentElement.style.colorScheme = theme
}

export const useThemeStore = create<ThemeState>((set, get) => ({
  preference: storedPreference(),
  resolvedTheme: 'dark',
  syncTheme: (systemIsDark) => {
    const preference = get().preference
    const resolvedTheme = preference === 'system' ? (systemIsDark ? 'dark' : 'light') : preference
    applyTheme(resolvedTheme)
    set({ resolvedTheme })
  },
  toggleTheme: () => {
    const resolvedTheme = get().resolvedTheme === 'dark' ? 'light' : 'dark'
    window.localStorage.setItem('douyin-live-theme', resolvedTheme)
    applyTheme(resolvedTheme)
    set({ preference: resolvedTheme, resolvedTheme })
  },
}))
