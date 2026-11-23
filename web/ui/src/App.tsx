import { useCallback, useEffect, useRef, useState } from 'react'
import { TooltipProvider } from '@/components/ui/tooltip'
import { CommandMenu, Kbd, MOD, Sidebar, StatusIcon, useTheme } from '@/components/Shell'
import { PanelLeft, Search } from 'lucide-react'
import { History } from '@/components/History'
import { RunView } from '@/components/RunView'
import { SignIn } from '@/components/SignIn'
import { SpecPanel } from '@/components/SpecPanel'
import { api, getJSON, onUnauthorized } from '@/lib/api'
import { useHashRoute, useRun } from '@/lib/useRun'
import type { Example, FactoryStatus, Summary, Validation } from '@/lib/types'

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
  const [dark, toggleTheme] = useTheme()
  const [palette, setPalette] = useState(false)
  const [specOpen, setSpecOpen] = useState(() => localStorage.getItem('specPane') !== 'closed')
  const toggleSpec = useCallback(() => setSpecOpen(!specOpen), [specOpen])
  useEffect(() => localStorage.setItem('specPane', specOpen ? 'open' : 'closed'), [specOpen])

  // Keyboard: Cmd/Ctrl+K opens the command menu, [ toggles the spec, G then L/H switches views.
  useEffect(() => {
    let g = 0
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setPalette((o) => !o)
        return
      }
      const t = e.target as HTMLElement
      if (e.metaKey || e.ctrlKey || e.altKey || t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName)) return
      if (e.key === '[') toggleSpec()
      else if (e.key === 'g') g = Date.now()
      else if (Date.now() - g < 800 && (e.key === 'h' || e.key === 'l')) {
        location.hash = e.key === 'h' ? '#/history' : '#/'
        g = 0
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [toggleSpec])

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

  const pickExample = (name: string) => {
    setExample(name)
    const ex = examples.find((e) => e.name === name)
    if (ex) setSpec(ex.source)
    setSpecOpen(true)
    location.hash = '#/'
  }

  return (
    <TooltipProvider delayDuration={150}>
      <div className="flex h-screen overflow-hidden bg-sidebar">
        <Sidebar view={route.view} runId={run?.id ?? route.runId} runs={history} status={status} dark={dark} onTheme={toggleTheme} onSearch={() => setPalette(true)} />
        <div className="flex min-w-0 flex-1 flex-col bg-background md:my-2 md:mr-2 md:rounded-lg md:border">
          <header className="flex h-11 shrink-0 items-center gap-2 border-b px-4 text-[13px]">
            {route.view === 'line' && (
              <button
                onClick={toggleSpec}
                aria-pressed={specOpen}
                title="Show or hide the spec ( [ )"
                className="-ml-1.5 grid size-7 place-items-center rounded-md text-muted-foreground hover:bg-accent hover:text-foreground aria-pressed:text-foreground"
              >
                <PanelLeft className="size-4" />
              </button>
            )}
            <nav aria-label="Breadcrumb" className="flex min-w-0 items-center gap-1.5 text-muted-foreground">
              <a href={route.view === 'history' ? '#/history' : '#/'} className="hover:text-foreground">
                {route.view === 'history' ? 'History' : 'Line'}
              </a>
              {route.view === 'line' && run && (
                <>
                  <span aria-hidden>/</span>
                  <span className="flex min-w-0 items-center gap-1.5 text-foreground">
                    <StatusIcon status={run.status} />
                    <span className="truncate font-medium">{run.name}</span>
                    <span className="font-mono text-[11.5px] text-muted-foreground">{run.id}</span>
                  </span>
                </>
              )}
            </nav>
            <button
              onClick={() => setPalette(true)}
              className="ml-auto flex h-7 items-center gap-2 rounded-md px-2 text-muted-foreground hover:bg-accent hover:text-foreground"
            >
              <Search className="size-3.5" />
              <span className="hidden sm:inline">Search</span>
              <Kbd>{MOD}</Kbd>
              <Kbd>K</Kbd>
            </button>
          </header>
          <main className="min-h-0 flex-1 overflow-y-auto">
            {route.view === 'history' ? (
              <div className="p-5">
                <History runs={history} />
              </div>
            ) : (
              <div className={specOpen ? 'grid min-h-full items-start lg:grid-cols-[minmax(300px,330px)_minmax(0,1fr)]' : 'min-h-full'}>
                {(
                  <SpecPanel
                    hidden={!specOpen}
                    spec={spec}
                    onSpec={setSpec}
                    examples={examples}
                    example={example}
                    onExample={pickExample}
                    validation={validation}
                    status={status}
                    running={run?.status === 'running'}
                    onStarted={(id) => {
                      loadedSpecFor.current = id
                      location.hash = `#/run/${id}`
                    }}
                  />
                )}
                <RunView key={run?.id ?? 'none'} run={run} error={error} validation={validation} />
              </div>
            )}
          </main>
        </div>
        <CommandMenu open={palette} onOpenChange={setPalette} runs={history} examples={examples} onExample={pickExample} onTheme={toggleTheme} onToggleSpec={toggleSpec} />
        <SignIn open={signIn} />
      </div>
    </TooltipProvider>
  )
}
