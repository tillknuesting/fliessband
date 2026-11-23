import { useEffect, useState } from 'react'
import { getJSON } from './api'
import type { Run, Stage } from './types'

type Event =
  | { type: 'snapshot'; data: Run }
  | { type: 'stage'; stage: string; data: Stage }
  | { type: 'log'; stage: string; line: string }
  | { type: 'run'; data: { status: Run['status'] } }

function apply(run: Run | null, ev: Event): Run | null {
  if (ev.type === 'snapshot') return ev.data
  if (!run) return run
  switch (ev.type) {
    case 'stage':
      return { ...run, stages: run.stages.map((s) => (s.id === ev.stage ? { ...s, ...ev.data, log: s.log } : s)) }
    case 'log':
      return { ...run, stages: run.stages.map((s) => (s.id === ev.stage ? { ...s, log: [...s.log, ev.line] } : s)) }
    case 'run':
      return { ...run, status: ev.data.status }
  }
}

/** Loads a run and, while it runs, follows its server-sent events. */
export function useRun(id: string | null, onFinished: () => void) {
  const [run, setRun] = useState<Run | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    setError(null)
    if (!id) {
      setRun(null)
      return
    }
    let cancelled = false
    let es: EventSource | null = null
    getJSON<Run>(`/api/runs/${encodeURIComponent(id)}`)
      .then((r) => {
        if (cancelled) return
        setRun(r)
        if (r.status !== 'running') return
        es = new EventSource(`/api/runs/${encodeURIComponent(id)}/events`)
        es.onmessage = (e) => setRun((prev) => apply(prev, JSON.parse(e.data) as Event))
        es.addEventListener('end', async () => {
          es?.close()
          const final = await getJSON<Run>(`/api/runs/${encodeURIComponent(id)}`)
          if (!cancelled) {
            setRun(final)
            onFinished()
          }
        })
      })
      .catch((err: Error) => {
        if (!cancelled) {
          setRun(null)
          setError(err.message)
        }
      })
    return () => {
      cancelled = true
      es?.close()
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id])

  return { run, error }
}

/** Reads the route from the location hash: #/run/<id> or #/history. */
export function useHashRoute() {
  const read = () => {
    const h = location.hash
    if (h.startsWith('#/history')) return { view: 'history' as const, runId: null }
    const m = h.match(/^#\/run\/(.+)$/)
    return { view: 'line' as const, runId: m ? decodeURIComponent(m[1]) : null }
  }
  const [route, setRoute] = useState(read)
  useEffect(() => {
    const on = () => setRoute(read())
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return route
}
