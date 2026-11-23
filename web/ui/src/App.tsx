import { useCallback, useEffect, useRef, useState } from 'react'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { History } from '@/components/History'
import { RunView } from '@/components/RunView'
import { SignIn } from '@/components/SignIn'
import { SpecPanel } from '@/components/SpecPanel'
import { api, getJSON, onUnauthorized } from '@/lib/api'
import { useHashRoute, useRun } from '@/lib/useRun'
import { cn } from '@/lib/utils'
import type { Example, FactoryStatus, Summary, Validation } from '@/lib/types'

function Logo() {
  return (
    <svg viewBox="0 0 28 28" aria-hidden className="size-6">
      <rect x="2.75" y="16.75" width="22.5" height="7.5" rx="3.75" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <circle cx="6.5" cy="20.5" r="1.75" fill="currentColor" />
      <circle cx="14" cy="20.5" r="1.75" fill="currentColor" />
      <circle cx="21.5" cy="20.5" r="1.75" fill="currentColor" />
      <rect x="5" y="9" width="7" height="6.5" rx="1" fill="currentColor" />
      <rect x="15" y="6" width="8" height="9.5" rx="1" fill="var(--signal)" />
    </svg>
  )
}

function Env({ status }: { status: FactoryStatus | null }) {
  if (!status) return <span className="text-sm text-zinc-400">Factory API unreachable</span>
  const found = status.tools.filter((t) => t.found).length
  const dot = (ok: boolean) => <span className={cn('size-2 rounded-full', ok ? 'bg-emerald-400' : 'bg-amber-400')} />
  return (
    <div className="flex items-center gap-4 text-sm text-zinc-300">
      <Tooltip>
        <TooltipTrigger className="flex items-center gap-1.5">
          {dot(found === status.tools.length)}
          Toolchain <b className="text-white">{found} of {status.tools.length}</b>
        </TooltipTrigger>
        <TooltipContent>
          {status.tools.map((t) => (
            <div key={t.name}>
              {t.found ? '✓' : '✕'} {t.name}: {t.found ? t.version : 'not found'}
            </div>
          ))}
        </TooltipContent>
      </Tooltip>
      <Tooltip>
        <TooltipTrigger className="flex items-center gap-1.5">
          {status.llm ? dot(true) : <span className="size-2 rounded-full bg-zinc-500" />}
          {status.llm ? (
            <>
              LLM <b className="text-white">{status.llm.model}</b>
            </>
          ) : (
            'LLM off'
          )}
        </TooltipTrigger>
        <TooltipContent>{status.llm ? status.llm.base_url : 'Start fliessband with FLIESSBAND_LLM_API_KEY (or OPENAI_API_KEY) to let the LLM implement custom RPCs.'}</TooltipContent>
      </Tooltip>
    </div>
  )
}

export default function App() {
  const route = useHashRoute()
  const [signIn, setSignIn] = useState(false)
  const [status, setStatus] = useState<FactoryStatus | null>(null)
  const [examples, setExamples] = useState<Example[]>([])
  const [history, setHistory] = useState<Summary[]>([])
  const [spec, setSpec] = useState('')
  const [example, setExample] = useState('')
  const [validation, setValidation] = useState<Validation | null>(null)
  const loadedSpecFor = useRef<string | null>(null)

  const refreshHistory = useCallback(() => getJSON<Summary[]>('/api/runs').then(setHistory, () => {}), [])
  const { run, error } = useRun(route.runId, refreshHistory)

  useEffect(() => onUnauthorized(() => setSignIn(true)), [])

  useEffect(() => {
    fetch('/api/session')
      .then((r) => r.json())
      .then((s: { required: boolean; signed_in: boolean }) => {
        if (s.required && !s.signed_in) {
          setSignIn(true)
          return
        }
        getJSON<FactoryStatus>('/api/status').then(setStatus, () => setStatus(null))
        getJSON<Example[]>('/api/examples').then((ex) => {
          setExamples(ex)
          const first = ex.find((e) => e.name === 'inventory') ?? ex[0]
          if (first && !location.hash.startsWith('#/run/')) {
            setExample(first.name)
            setSpec(first.source)
          }
        }, () => {})
        getJSON<Summary[]>('/api/runs').then((h) => {
          setHistory(h)
          if (!location.hash && h[0]) location.hash = `#/run/${h[0].id}`
        }, () => {})
      }, () => {})
  }, [])

  // Show the spec a run was made from when the run is opened.
  useEffect(() => {
    if (!run || loadedSpecFor.current === run.id) return
    loadedSpecFor.current = run.id
    if (run.spec) setSpec(run.spec)
  }, [run])
  useEffect(() => {
    if (run && examples.some((e) => e.name === run.name)) setExample(run.name)
  }, [run?.id, run?.name, examples]) // eslint-disable-line react-hooks/exhaustive-deps

  // Validate as the spec changes.
  useEffect(() => {
    if (!spec) return
    const t = setTimeout(async () => {
      try {
        setValidation(await (await api('/api/validate', { method: 'POST', body: spec })).json())
      } catch (err) {
        setValidation({ ok: false, error: (err as Error).message, problems: [], examples: 0, holdout: 0 })
      }
    }, 300)
    return () => clearTimeout(t)
  }, [spec])

  useEffect(() => {
    if (route.view === 'history') refreshHistory()
  }, [route.view, refreshHistory])

  return (
    <TooltipProvider delayDuration={150}>
      <div className="min-h-screen bg-muted/50">
        <header className="flex h-13 items-center gap-6 bg-zinc-900 px-5 text-zinc-100">
          <a href="#/" className="flex items-center gap-2 text-lg font-bold tracking-tight">
            <Logo /> Fliessband
          </a>
          <nav aria-label="Views" className="flex gap-1">
            {(
              [
                ['line', 'Line', run ? `#/run/${run.id}` : '#/'],
                ['history', 'History', '#/history'],
              ] as const
            ).map(([v, label, href]) => (
              <a
                key={v}
                href={href}
                aria-current={route.view === v ? 'page' : undefined}
                className="rounded-md px-3 py-1.5 text-sm font-medium text-zinc-400 hover:text-white aria-[current=page]:bg-white/10 aria-[current=page]:text-white aria-[current=page]:shadow-[inset_0_-2px_0_var(--signal)]"
              >
                {label}
              </a>
            ))}
          </nav>
          <div className="ml-auto">
            <Env status={status} />
          </div>
        </header>
        <main className="p-4">
          {route.view === 'history' ? (
            <History runs={history} />
          ) : (
            <div className="grid items-start gap-4 lg:grid-cols-[minmax(340px,400px)_minmax(0,1fr)]">
              <SpecPanel
                spec={spec}
                onSpec={setSpec}
                examples={examples}
                example={example}
                onExample={(name) => {
                  setExample(name)
                  const ex = examples.find((e) => e.name === name)
                  if (ex) setSpec(ex.source)
                }}
                validation={validation}
                status={status}
                running={run?.status === 'running'}
                onStarted={(id) => {
                  loadedSpecFor.current = id
                  location.hash = `#/run/${id}`
                }}
              />
              <RunView key={run?.id ?? 'none'} run={run} error={error} validation={validation} />
            </div>
          )}
        </main>
        <SignIn open={signIn} />
      </div>
    </TooltipProvider>
  )
}
