import { useEffect, useMemo, useState } from 'react'
import { Minus, Play, Plus, Square } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { api, getJSON, postJSON } from '@/lib/api'
import { ms } from '@/lib/format'
import { LINE, SHORT } from '@/lib/stages'
import { cn } from '@/lib/utils'
import type { Brief, Example, FactoryStatus, Stage, Status, Summary } from '@/lib/types'
import { ago, StatusIcon } from './Shell'
import { remaining, stationProgress } from './Conveyor'

const GRADE_COLOR: Record<string, string> = { A: 'var(--good)', B: 'var(--good)', C: 'var(--warn)', D: 'var(--destructive)', F: 'var(--destructive)' }
const NAME = Object.fromEntries(LINE.map((s) => [s.id, s.name]))

const asStage = (b: Brief): Stage => ({ id: b.id, name: NAME[b.id] ?? b.id, tool: '', gate: false, status: b.status, summary: b.summary ?? '', started_at: b.started_at, duration_ms: b.ms, log: [] })
const started = (r: Summary) => (r.stages ?? []).some((s) => s.status !== 'pending')
const dur = (v: number) => {
  const s = Math.round(v / 1000)
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${String(s % 60).padStart(2, '0')} s`
}

/** Re-renders a few times a second while something moves. */
function useNow(active: boolean, every = 250) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    if (!active) return
    const t = setInterval(() => setNow(Date.now()), every)
    return () => clearInterval(t)
  }, [active, every])
  return now
}

/**
 * The factory floor: how the shift is going, every production line with the
 * work on it, a dispatch desk for new work orders, and which stations hold
 * the line up.
 */
export function Floor(props: { runs: Summary[]; status: FactoryStatus | null; examples: Example[]; onRuns: (r: Summary[]) => void; onStatus: (s: FactoryStatus) => void }) {
  const { runs, status, examples, onRuns, onStatus } = props
  const busy = runs.some((r) => r.status === 'running')

  // Poll fast while lines are busy, slowly when the floor is quiet.
  useEffect(() => {
    let live = true
    const tick = () => {
      getJSON<Summary[]>('/api/runs').then((r) => live && onRuns(r), () => {})
      getJSON<FactoryStatus>('/api/status').then((s) => live && onStatus(s), () => {})
    }
    tick()
    const t = setInterval(tick, busy ? 1000 : 4000)
    return () => {
      live = false
      clearInterval(t)
    }
  }, [busy, onRuns, onStatus])

  return (
    <div className="space-y-4 p-5">
      <ShiftBoard runs={runs} status={status} />
      <div className="grid items-start gap-4 xl:grid-cols-[minmax(0,1fr)_340px]">
        <Lanes runs={runs} status={status} />
        <Dispatch examples={examples} status={status} />
      </div>
      <div className="grid gap-4 xl:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
        <Quality runs={runs} />
        <Stations runs={runs} />
      </div>
    </div>
  )
}

function Panel({ title, note, children, className, action }: { title: string; note?: React.ReactNode; children: React.ReactNode; className?: string; action?: React.ReactNode }) {
  return (
    <section className={cn('rounded-lg border bg-card', className)}>
      <header className="flex items-baseline gap-2 border-b px-4 py-2.5">
        <h2 className="text-[13px] font-semibold">{title}</h2>
        {note && <span className="text-[12px] text-muted-foreground">{note}</span>}
        {action && <span className="ml-auto">{action}</span>}
      </header>
      {children}
    </section>
  )
}

function ShiftBoard({ runs, status }: { runs: Summary[]; status: FactoryStatus | null }) {
  const finished = runs.filter((r) => r.status !== 'running')
  const passed = finished.filter((r) => r.status === 'passed')
  const graded = finished.filter((r) => r.score != null).slice(0, 20)
  const mean = graded.length ? graded.reduce((a, r) => a + r.score!, 0) / graded.length : null
  const spend = runs.reduce((a, r) => a + (r.cost_usd ?? 0), 0)
  const lineTime = passed.length ? passed.slice(0, 20).reduce((a, r) => a + r.duration_ms, 0) / Math.min(20, passed.length) : null
  const cap = status?.capacity
  const today = finished.filter((r) => Date.now() - new Date(r.created_at).getTime() < 86400_000).length
  const tiles: { label: string; value: string; note: string; tone?: string }[] = [
    { label: 'Lines busy', value: cap ? `${cap.active} of ${cap.lines}` : '–', note: cap?.queued ? `${cap.queued} waiting for a line` : 'nothing waiting' },
    { label: 'Yield', value: finished.length ? `${Math.round((passed.length / finished.length) * 100)}%` : '–', note: `${passed.length} of ${finished.length} runs passed every gate` },
    { label: 'Mean score', value: mean != null ? mean.toFixed(0) : '–', note: `last ${graded.length} graded runs` },
    { label: 'Throughput', value: String(today), note: 'runs finished in the last 24 h' },
    { label: 'Line time', value: lineTime != null ? dur(lineTime) : '–', note: 'mean of passing runs' },
    { label: 'LLM spend', value: `$${spend.toFixed(2)}`, note: `across ${runs.filter((r) => r.cost_usd).length} runs with a model` },
  ]
  return (
    <div className="grid grid-cols-2 overflow-hidden rounded-lg border bg-card sm:grid-cols-3 xl:grid-cols-6">
      {tiles.map((t, i) => (
        <div key={t.label} className={cn('px-4 py-3', i > 0 && 'border-l', i >= 2 && 'max-sm:border-t', i >= 3 && 'max-xl:border-t')}>
          <div className="text-[12px] text-muted-foreground">{t.label}</div>
          <div className="mt-0.5 text-[22px] font-semibold tracking-[-0.02em] tabular-nums">{t.value}</div>
          <div className="truncate text-[11.5px] text-muted-foreground">{t.note}</div>
        </div>
      ))}
    </div>
  )
}

/** One lane per production line: the run on it, its stations and where the work piece is. */
function Lanes({ runs, status }: { runs: Summary[]; status: FactoryStatus | null }) {
  const active = runs.filter((r) => r.status === 'running' && started(r))
  const waiting = runs.filter((r) => r.status === 'running' && !started(r))
  const lines = Math.max(status?.capacity?.lines ?? 2, active.length)
  const now = useNow(active.length > 0)
  const recent = runs.find((r) => r.status !== 'running')
  return (
    <Panel title="Production lines" note={`${lines} lines, set with -parallel`}>
      <div className="space-y-2 p-3">
        {Array.from({ length: lines }, (_, i) => {
          const r = active[i]
          return r ? <Lane key={r.id} n={i + 1} run={r} now={now} /> : <IdleLane key={`idle-${i}`} n={i + 1} last={i === 0 ? recent : undefined} />
        })}
        {waiting.length > 0 && (
          <div className="flex flex-wrap items-center gap-2 rounded-md border border-dashed px-3 py-2 text-[12.5px] text-muted-foreground">
            Waiting for a line:
            {waiting.map((r) => (
              <a key={r.id} href={`#/run/${r.id}`} className="rounded border bg-background px-1.5 py-0.5 font-mono text-[11.5px] text-foreground hover:border-signal/60">
                {r.name} {r.id.slice(-4)}
              </a>
            ))}
          </div>
        )}
      </div>
    </Panel>
  )
}

function Lane({ n, run, now }: { n: number; run: Summary; now: number }) {
  const stages = (run.stages ?? []).map(asStage)
  const i = stages.findIndex((s) => s.status === 'running')
  const x = i >= 0 ? ((i + 0.2 + 0.6 * stationProgress(stages[i], now)) / stages.length) * 100 : 0
  const [stopping, setStopping] = useState(false)
  return (
    <div className="group rounded-md border bg-background/60 px-3 py-2.5 transition-colors hover:border-signal/40">
      <div className="flex items-center gap-2 text-[13px]">
        <span className="grid size-5 place-items-center rounded bg-signal/15 text-[11px] font-semibold text-signal">{n}</span>
        <a href={`#/run/${run.id}`} className="font-medium hover:underline">
          {run.name}
        </a>
        <span className="font-mono text-[11.5px] text-muted-foreground">{run.id.slice(-4)}</span>
        {i >= 0 && <span className="text-muted-foreground">at {stages[i].name}</span>}
        <span className="ml-auto text-[12px] text-muted-foreground tabular-nums">
          {dur(now - new Date(run.created_at).getTime())}, about {dur(remaining(stages, now))} left
        </span>
        <button
          disabled={stopping}
          onClick={async () => {
            setStopping(true)
            await api(`/api/runs/${encodeURIComponent(run.id)}/cancel`, { method: 'POST' }).catch(() => {})
          }}
          className="grid size-6 place-items-center rounded text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100 hover:bg-destructive/10 hover:text-destructive focus-visible:opacity-100 disabled:opacity-40"
          aria-label={`Stop ${run.name}`}
          title="Stop the run"
        >
          <Square className="size-3.5" />
        </button>
      </div>
      <div className="relative mt-2.5">
        <div className="grid gap-[3px]" style={{ gridTemplateColumns: `repeat(${stages.length}, 1fr)` }}>
          {stages.map((s) => (
            <Tooltip key={s.id}>
              <TooltipTrigger asChild>
                <span
                  className={cn(
                    'h-2.5 rounded-[2px] transition-colors duration-300',
                    { passed: 'bg-good/70', failed: 'bg-destructive', warning: 'bg-warn', skipped: 'bg-muted', pending: 'bg-muted-foreground/15', running: 'mini-running bg-signal', canceled: 'bg-muted' }[s.status],
                  )}
                />
              </TooltipTrigger>
              <TooltipContent>
                {s.name}: {s.status === 'running' ? s.summary || 'running' : s.status}
                {s.duration_ms ? `, ${ms(s.duration_ms)}` : ''}
              </TooltipContent>
            </Tooltip>
          ))}
        </div>
        <div className="pointer-events-none absolute -top-[3px] h-4 w-1 -translate-x-1/2 rounded-full bg-signal shadow-[0_0_8px_var(--signal)] transition-[left] duration-300 ease-linear" style={{ left: `${x}%` }} />
      </div>
    </div>
  )
}

function IdleLane({ n, last }: { n: number; last?: Summary }) {
  return (
    <div className="flex items-center gap-2 rounded-md border border-dashed px-3 py-2.5 text-[13px] text-muted-foreground">
      <span className="grid size-5 place-items-center rounded bg-muted text-[11px] font-semibold">{n}</span>
      Idle
      {last && (
        <span className="ml-auto flex items-center gap-1.5 text-[12px]">
          last off the line:
          <a href={`#/run/${last.id}`} className="flex items-center gap-1 text-foreground hover:underline">
            <StatusIcon status={last.status} /> {last.name}
            {last.grade && <b style={{ color: GRADE_COLOR[last.grade] }}>{last.grade}</b>}
          </a>
          {ago(last.created_at)}
        </span>
      )}
    </div>
  )
}

const TOGGLES: { key: 'llm' | 'mutation' | 'fuzz' | 'vuln' | 'integration' | 'smoke'; label: string; station: string }[] = [
  { key: 'llm', label: 'LLM implements', station: 'implement' },
  { key: 'mutation', label: 'Mutation', station: 'mutation' },
  { key: 'fuzz', label: 'Fuzz', station: 'fuzz' },
  { key: 'vuln', label: 'Vulns', station: 'vulncheck' },
  { key: 'integration', label: 'PostgreSQL', station: 'integration' },
  { key: 'smoke', label: 'Smoke', station: 'smoke' },
]

/** The dispatch desk: pick a spec, switch optional stations, send work orders to the queue. */
function Dispatch({ examples, status }: { examples: Example[]; status: FactoryStatus | null }) {
  const [spec, setSpec] = useState('')
  const [count, setCount] = useState(1)
  const [opts, setOpts] = useState({ llm: false, mutation: true, fuzz: true, vuln: true, integration: true, smoke: true })
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)
  const [sending, setSending] = useState(false)
  const chosen = examples.find((e) => e.name === spec) ?? examples[0]
  const cap = status?.capacity
  const free = cap ? Math.max(0, cap.lines - cap.active) : 0

  const send = async () => {
    if (!chosen) return
    setSending(true)
    setMsg(null)
    let ok = 0
    let err = ''
    for (let i = 0; i < count; i++) {
      try {
        await postJSON('/api/runs', { spec: chosen.source, options: { ...opts, llm: opts.llm && !!status?.llm } })
        ok++
      } catch (e) {
        err = (e as Error).message
        break
      }
    }
    setSending(false)
    setMsg(ok === count ? { ok: true, text: `${ok} work order${ok > 1 ? 's' : ''} for ${chosen.name} dispatched.` } : { ok: false, text: `${ok} of ${count} dispatched: ${err}` })
  }

  return (
    <Panel title="Dispatch" note="new work orders">
      <div className="space-y-3 p-4">
        <label className="block text-[12px] text-muted-foreground">
          Spec
          <select value={chosen?.name ?? ''} onChange={(e) => setSpec(e.target.value)} className="mt-1 block h-8 w-full rounded-md border bg-background px-2 text-[13px] text-foreground">
            {examples.map((e) => (
              <option key={e.name} value={e.name}>
                {e.file}
              </option>
            ))}
          </select>
        </label>
        <div>
          <div className="mb-1.5 text-[12px] text-muted-foreground">Optional stations</div>
          <div className="flex flex-wrap gap-1.5">
            {TOGGLES.map((t) => {
              const disabled = t.key === 'llm' && !status?.llm
              const on = opts[t.key] && !disabled
              return (
                <button
                  key={t.key}
                  disabled={disabled}
                  aria-pressed={on}
                  onClick={() => setOpts({ ...opts, [t.key]: !opts[t.key] })}
                  title={disabled ? 'Start fliessband with an LLM key to switch this on' : `${on ? 'Skip' : 'Run'} the ${NAME[t.station]} station`}
                  className={cn(
                    'flex h-7 items-center gap-1.5 rounded-md border px-2 text-[12.5px] transition-[background-color,border-color,color] duration-150 disabled:cursor-not-allowed disabled:opacity-40',
                    on ? 'border-signal/50 bg-signal/10 text-foreground' : 'text-muted-foreground hover:bg-accent',
                  )}
                >
                  <span className={cn('size-2 rounded-full transition-colors', on ? 'bg-signal shadow-[0_0_6px_var(--signal)]' : 'bg-muted-foreground/40')} />
                  {t.label}
                </button>
              )
            })}
          </div>
        </div>
        <div className="flex items-center gap-2">
          <div className="flex h-8 items-center rounded-md border">
            <button onClick={() => setCount(Math.max(1, count - 1))} className="grid size-8 place-items-center text-muted-foreground hover:text-foreground" aria-label="Fewer">
              <Minus className="size-3.5" />
            </button>
            <span className="w-6 text-center text-[13px] font-semibold tabular-nums" aria-live="polite">
              {count}
            </span>
            <button onClick={() => setCount(Math.min(5, count + 1))} className="grid size-8 place-items-center text-muted-foreground hover:text-foreground" aria-label="More">
              <Plus className="size-3.5" />
            </button>
          </div>
          <button
            onClick={send}
            disabled={sending || !chosen}
            className="flex h-8 flex-1 items-center justify-center gap-1.5 rounded-md bg-signal text-[13px] font-medium text-signal-foreground transition-[filter,transform] hover:brightness-110 active:scale-[0.98] disabled:opacity-50"
          >
            <Play className="size-3.5 fill-current" /> {sending ? 'Dispatching…' : `Dispatch ${count}`}
          </button>
        </div>
        <p className="text-[12px] text-muted-foreground">
          {count > free ? `${free} line${free === 1 ? '' : 's'} free now; the rest wait in the queue.` : `Starts at once on ${count === 1 ? 'a free line' : `${count} free lines`}.`}
        </p>
        {msg && <p className={cn('animate-in text-[12.5px] fade-in-0 slide-in-from-top-1', msg.ok ? 'text-good' : 'text-destructive')}>{msg.text}</p>}
      </div>
    </Panel>
  )
}

/** Score of every finished run, oldest left. A dot per run; click to open it. */
function Quality({ runs }: { runs: Summary[] }) {
  const pts = runs.filter((r) => r.score != null).slice(0, 40).reverse()
  const [hover, setHover] = useState<number | null>(null)
  const W = 600
  const H = 170
  const pad = { l: 28, r: 10, t: 12, b: 20 }
  const x = (i: number) => pad.l + (pts.length < 2 ? (W - pad.l - pad.r) / 2 : (i / (pts.length - 1)) * (W - pad.l - pad.r))
  const y = (v: number) => pad.t + (1 - v / 100) * (H - pad.t - pad.b)
  const h = hover != null ? pts[hover] : null
  return (
    <Panel title="Quality" note={`score of the last ${pts.length} graded runs`}>
      {pts.length === 0 ? (
        <p className="p-6 text-center text-[13px] text-muted-foreground">Scores appear here as runs come off the line.</p>
      ) : (
        <div className="relative p-3">
          <svg viewBox={`0 0 ${W} ${H}`} className="w-full" onMouseLeave={() => setHover(null)} role="img" aria-label="Score per run">
            {[0, 40, 80, 100].map((v) => (
              <g key={v}>
                <line x1={pad.l} x2={W - pad.r} y1={y(v)} y2={y(v)} stroke="var(--border)" strokeDasharray={v === 40 ? '3 3' : undefined} />
                <text x={pad.l - 6} y={y(v)} textAnchor="end" dominantBaseline="middle" className="fill-muted-foreground text-[10px]">
                  {v}
                </text>
              </g>
            ))}
            <polyline points={pts.map((r, i) => `${x(i)},${y(r.score!)}`).join(' ')} fill="none" stroke="var(--signal)" strokeWidth="2" strokeLinejoin="round" />
            {pts.map((r, i) => (
              <g key={r.id} onMouseEnter={() => setHover(i)} onClick={() => (location.hash = `#/run/${r.id}`)} className="cursor-pointer">
                <circle cx={x(i)} cy={y(r.score!)} r="12" fill="transparent" />
                <circle cx={x(i)} cy={y(r.score!)} r={hover === i ? 5.5 : 4} fill={GRADE_COLOR[r.grade ?? ''] ?? 'var(--muted-foreground)'} stroke="var(--card)" strokeWidth="2" className="transition-[r] duration-100" />
              </g>
            ))}
          </svg>
          {h && hover != null && (
            <div className="pointer-events-none absolute top-2 rounded-md border bg-popover px-2.5 py-1.5 text-[12px] shadow-md" style={{ left: `clamp(8px, calc(${(x(hover) / W) * 100}% - 70px), calc(100% - 160px))` }}>
              <div className="font-medium">
                {h.name} <span className="font-mono text-muted-foreground">{h.id.slice(-4)}</span>
              </div>
              <div className="text-muted-foreground">
                {Math.round(h.score!)} {h.grade}, {ago(h.created_at)}
                {h.llm ? ', LLM' : ''}
              </div>
            </div>
          )}
        </div>
      )}
    </Panel>
  )
}

/** Where the line spends its time and where it stops, across recent runs. */
function Stations({ runs }: { runs: Summary[] }) {
  const done = runs.filter((r) => r.status !== 'running' && r.stages?.length).slice(0, 30)
  const [pick, setPick] = useState<string | null>(null)
  const rows = useMemo(
    () =>
      LINE.map((st) => {
        const xs = done.map((r) => r.stages!.find((b) => b.id === st.id)).filter((b): b is Brief => !!b)
        const ran = xs.filter((b) => b.status !== 'skipped' && b.status !== 'pending' && b.status !== 'canceled')
        return {
          id: st.id,
          name: st.name,
          mean: ran.length ? ran.reduce((a, b) => a + b.ms, 0) / ran.length : 0,
          max: ran.reduce((a, b) => Math.max(a, b.ms), 0),
          failed: xs.filter((b) => b.status === 'failed').length,
          skipped: xs.filter((b) => b.status === 'skipped').length,
          ran: ran.length,
        }
      }),
    [done],
  )
  const top = Math.max(1, ...rows.map((r) => r.mean))
  const bottleneck = rows.reduce((a, r) => (r.mean > a.mean ? r : a), rows[0])
  const picked = rows.find((r) => r.id === pick)
  const pickedRuns = pick
    ? done
        .map((r) => ({ r, b: r.stages!.find((b) => b.id === pick)! }))
        .filter((x) => x.b && x.b.status !== 'pending')
        .sort((a, b) => (b.b.status === 'failed' ? 1 : 0) - (a.b.status === 'failed' ? 1 : 0) || b.b.ms - a.b.ms)
        .slice(0, 6)
    : []
  return (
    <Panel title="Stations" note={`mean time over the last ${done.length} runs; click one for its runs`}>
      {done.length === 0 ? (
        <p className="p-6 text-center text-[13px] text-muted-foreground">Station times appear here once runs finish.</p>
      ) : (
        <div className="p-2">
          <ul>
            {rows.map((r) => (
              <li key={r.id}>
                <button
                  onClick={() => setPick(pick === r.id ? null : r.id)}
                  aria-pressed={pick === r.id}
                  className={cn('grid w-full grid-cols-[84px_1fr_64px_44px] items-center gap-2 rounded px-2 py-[3px] text-left text-[12px] transition-colors hover:bg-accent', pick === r.id && 'bg-accent')}
                >
                  <span className={cn('truncate', r.ran === 0 && 'text-muted-foreground')}>{SHORT[r.id] ?? r.name}</span>
                  <span className="h-2 rounded-full bg-muted">
                    <span
                      className={cn('block h-2 rounded-full transition-[width] duration-500', r.id === bottleneck.id ? 'bg-signal' : 'bg-foreground/35')}
                      style={{ width: `${Math.max(r.mean ? 1.5 : 0, (r.mean / top) * 100)}%` }}
                    />
                  </span>
                  <span className="text-right text-muted-foreground tabular-nums">{r.ran ? ms(r.mean) : 'skipped'}</span>
                  <span className={cn('text-right tabular-nums', r.failed ? 'font-medium text-destructive' : 'text-muted-foreground/60')}>{r.failed ? `${r.failed} ✕` : ''}</span>
                </button>
              </li>
            ))}
          </ul>
          <div className="mt-2 border-t px-2 pt-2 text-[12px]">
            {picked ? (
              <div className="animate-in space-y-1 fade-in-0">
                <div className="text-muted-foreground">
                  {picked.name}: ran {picked.ran} times, slowest {ms(picked.max)}, stopped the line {picked.failed} times, skipped {picked.skipped}.
                </div>
                {pickedRuns.map(({ r, b }) => (
                  <a key={r.id} href={`#/run/${r.id}`} className="flex items-center gap-2 rounded px-1 py-0.5 hover:bg-accent">
                    <StatusIcon status={b.status as Status} />
                    {r.name}
                    <span className="font-mono text-muted-foreground">{r.id.slice(-4)}</span>
                    <span className="ml-auto text-muted-foreground tabular-nums">{b.status === 'skipped' ? 'skipped' : ms(b.ms)}</span>
                  </a>
                ))}
              </div>
            ) : (
              <span className="text-muted-foreground">
                Bottleneck: <b className="text-foreground">{bottleneck.name}</b>, {ms(bottleneck.mean)} on average, {Math.round((bottleneck.mean / rows.reduce((a, r) => a + r.mean, 0)) * 100)}% of line time.
              </span>
            )}
          </div>
        </div>
      )}
    </Panel>
  )
}
