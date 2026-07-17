import '@testing-library/jest-dom/vitest'

window.matchMedia = window.matchMedia ?? (() => ({ matches: false }) as MediaQueryList)
