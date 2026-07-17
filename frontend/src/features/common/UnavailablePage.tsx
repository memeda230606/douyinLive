import { Construction } from 'lucide-react'

export function UnavailablePage({ title }: { title: string }) {
  return <main className="page"><section className="empty-panel"><div><h1>{title}</h1><p>这项能力将在后续开发阶段开放。</p></div><Construction aria-hidden="true" /></section></main>
}
