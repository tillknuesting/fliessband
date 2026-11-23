import { useEffect, useRef, useState } from 'react'
import { Check, Minus, Ban, X } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { ms } from '@/lib/format'
import { SHORT, STATUS_WORD } from '@/lib/stages'
import type { Stage, Status } from '@/lib/types'

const LAMP: Record<Stage['status'], string> = {
  passed: 'bg-good border-good text-white',
  failed: 'bg-destructive border-destructive text-white',
  warning: 'bg-warn border-warn text-foreground',
  skipped: 'bg-card border-dashed border-muted-foreground/50 text-muted-foreground',
  pending: 'bg-card border-border text-muted-foreground',
  running: 'lamp-running border-foreground',
  canceled: 'bg-card border-foreground/60 text-foreground/70',
}

function Icon({ status }: { status: Stage['status'] }) {
  const c = 'size-4 stroke-[3]'
  switch (status) {
    case 'passed':
      return <Check className={c} />
    case 'failed':
      return <X className={c} />
    case 'warning':
      return <span className="text-sm font-bold">!</span>
    case 'skipped':
      return <Minus className={c} />
    case 'canceled':
      return <Ban className={c} />
    default:
      return null
  }
}

/** Typical station times in ms, from real runs. They only shape the animation and the estimate. */
export const TYPICAL: Record<string, number> = {
  spec: 50, scaffold: 50, protolint: 200, breaking: 100, codegen: 100, implement: 60000, policy: 50, deps: 2000,
  build: 4500, vet: 500, lint: 3000, test: 5000, acceptance: 800, review: 30000, mutation: 60000, fuzz: 25000,
  vulncheck: 2000, integration: 12000, smoke: 1500, sbom: 500, measure: 100,
}

const elapsed = (s: Stage, now: number) => (s.started_at ? Math.max(0, now - new Date(s.started_at).getTime()) : 0)

/** How far a running station has likely got, 0..1. It eases towards 1 and never claims to be done. */
export function stationProgress(s: Stage, now: number) {
  // A station that reports a count ("40/150 mutants run") knows better than any estimate.
  const m = /(\d+)\s*\/\s*(\d+)/.exec(s.summary)
  if (m && +m[2] > 0) return Math.min(0.98, +m[1] / +m[2])
  return 1 - Math.exp(-elapsed(s, now) / (TYPICAL[s.id] ?? 3000))
}

/** Seconds the rest of the line will likely take. */
export function remaining(stages: Stage[], now: number) {
  let ms = 0
  for (const s of stages) {
    if (s.status === 'pending') ms += TYPICAL[s.id] ?? 3000
    else if (s.status === 'running') ms += Math.max(0, (TYPICAL[s.id] ?? 3000) - elapsed(s, now))
  }
  return ms
}

/** Where the work piece should be, in % of the line: inside the running station it passes under the machine as the work goes on. */
function target(stages: Stage[], runStatus: Status | undefined, now: number) {
  const n = stages.length
  const running = stages.findIndex((s) => s.status === 'running')
  if (running >= 0) return { x: ((running + 0.2 + 0.6 * stationProgress(stages[running], now)) / n) * 100, state: 'moving' as const }
  const failed = stages.findIndex((s) => s.status === 'failed')
  if (failed >= 0) return { x: ((failed + 0.5) / n) * 100, state: 'stopped' as const }
  if (runStatus === 'passed') return { x: 100, state: 'shipped' as const }
  let last = -1
  stages.forEach((s, i) => {
    if (s.status !== 'pending') last = i
  })
  if (last < 0) return null
  return { x: ((last + 0.8) / n) * 100, state: runStatus === 'canceled' ? ('stopped' as const) : ('moving' as const) }
}

const reduced = () => window.matchMedia('(prefers-reduced-motion: reduce)').matches

/**
 * Moves the work piece and the striped belt every frame, outside React: a
 * critically damped spring follows the target, so the piece glides from
 * station to station instead of jumping.
 */
function useGlide(stages: Stage[], runStatus: Status | undefined, piece: React.RefObject<HTMLDivElement | null>, belt: React.RefObject<HTMLDivElement | null>) {
  const latest = useRef({ stages, runStatus })
  latest.current = { stages, runStatus }
  useEffect(() => {
    let x = -1
    let v = 0
    let last = performance.now()
    let frame = 0
    const k = 90
    const c = 2 * Math.sqrt(k)
    const tick = (t: number) => {
      const dt = Math.min(0.05, (t - last) / 1000)
      last = t
      const tg = target(latest.current.stages, latest.current.runStatus, Date.now())
      const goal = tg ? tg.x : 0
      if (x < 0 || reduced()) {
        x = goal
        v = 0
      } else {
        v += (k * (goal - x) - c * v) * dt
        x += v * dt
      }
      if (piece.current) piece.current.style.left = `${x}%`
      if (belt.current) belt.current.style.width = `${Math.min(100, Math.max(0, x))}%`
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [piece, belt])
}

/** Ticks a running station's clock ten times a second. */
function LiveTime({ stage }: { stage: Stage }) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 100)
    return () => clearInterval(t)
  }, [])
  return <span className="text-signal">{(elapsed(stage, now) / 1000).toFixed(1)} s</span>
}

const GRADE_TONE: Record<string, string> = { A: 'text-good', B: 'text-good', C: 'text-warn', D: 'text-destructive', F: 'text-destructive' }

/**
 * The line: every station stands over a belt, grouped by phase. Square lamps
 * are gates (a failure stops the line), round lamps are checks. A work piece
 * rides the belt to whichever station is working on it.
 */
export function Conveyor({
  stages,
  running,
  runStatus,
  grade,
  selected,
  onSelect,
}: {
  stages: Stage[]
  running: boolean
  runStatus?: Status
  grade?: string
  selected?: string
  onSelect: (id: string) => void
}) {
  const piece = target(stages, runStatus, Date.now())
  const pieceRef = useRef<HTMLDivElement>(null)
  const beltRef = useRef<HTMLDivElement>(null)
  useGlide(stages, runStatus, pieceRef, beltRef)
  const phases: { name: string; n: number }[] = []
  for (const s of stages) {
    const p = s.phase ?? ''
    if (phases.length && phases[phases.length - 1].name === p) phases[phases.length - 1].n++
    else phases.push({ name: p, n: 1 })
  }
  const cols = { gridTemplateColumns: `repeat(${stages.length}, minmax(36px, 1fr))` }
  return (
    <div className="line-floor overflow-x-auto rounded-lg border px-3 pt-3 pb-4">
      <div className="relative min-w-[760px]">
        <div className="mb-2 grid" style={cols} aria-hidden>
          {phases.map((p, i) => (
            <span key={i} className="mx-1 flex items-center gap-1.5 text-[11px] font-medium text-muted-foreground" style={{ gridColumn: `span ${p.n}` }}>
              <span className="h-px flex-1 bg-border" />
              {p.name}
              <span className="h-px flex-1 bg-border" />
            </span>
          ))}
        </div>
        <ol className="relative grid" style={cols}>
          {stages.map((s) => {
            const time = s.status === 'running' ? <LiveTime stage={s} /> : s.status === 'skipped' ? 'skipped' : s.duration_ms ? ms(s.duration_ms) : ''
            return (
              <li key={s.id} className="flex justify-center">
                <Tooltip>
                  <TooltipTrigger asChild>
                    <button
                      type="button"
                      onClick={() => onSelect(s.id)}
                      aria-label={`${s.name}, ${s.gate ? 'gate' : 'check'}, ${STATUS_WORD[s.status]}`}
                      className={cn('group flex w-full flex-col items-center gap-1 rounded-md px-0.5 pt-1.5 pb-1 outline-none focus-visible:ring-2 focus-visible:ring-ring', selected === s.id && 'bg-accent')}
                    >
                      <span className={cn('z-10 grid size-7 place-items-center border-2 transition-colors duration-300', s.gate ? 'rounded-md' : 'rounded-full', LAMP[s.status])}>
                        <Icon status={s.status} />
                      </span>
                      <span className={cn('w-full truncate text-center text-[10.5px] font-medium leading-tight tracking-[-0.02em] text-foreground/75 group-hover:text-foreground', s.status === 'failed' && 'text-destructive', s.status === 'running' && 'text-foreground')}>
                        {SHORT[s.id] ?? s.name}
                      </span>
                      <span className="min-h-3 whitespace-nowrap text-[10px] leading-none text-muted-foreground tabular-nums">{time}</span>
                      {/* The machine's foot on the belt. */}
                      <span className={cn('mt-0.5 h-2 w-px', s.status === 'pending' ? 'bg-border' : s.status === 'failed' ? 'bg-destructive' : 'bg-signal/70')} aria-hidden />
                    </button>
                  </TooltipTrigger>
                  <TooltipContent className="max-w-80">
                    <p className="font-medium">
                      {s.name} ({s.gate ? 'gate' : 'check'})
                    </p>
                    <p>
                      {STATUS_WORD[s.status]}
                      {s.summary ? `: ${s.summary}` : ''}
                    </p>
                  </TooltipContent>
                </Tooltip>
              </li>
            )
          })}
        </ol>
        {/* The belt: rollers along its length, hazard stripes where the work has been. */}
        <div className="relative mt-px h-3" aria-hidden>
          <div className="belt-track absolute inset-0 rounded-full" />
          <div ref={beltRef} className={cn('belt-done absolute inset-y-0 left-0 rounded-full', running && 'belt-moving', piece?.state === 'stopped' && 'belt-stopped')} />
          {piece && (
            <div ref={pieceRef} className={cn('absolute bottom-[7px] -translate-x-1/2 transition-transform duration-300', piece.state === 'shipped' && '-translate-x-full')}>
              <div
                className={cn(
                  'work-piece grid h-[18px] min-w-[22px] place-items-center rounded-[3px] border px-1 text-[10.5px] leading-none font-bold',
                  piece.state === 'moving' && running && 'work-piece-moving',
                  piece.state === 'stopped' && 'border-destructive bg-destructive/90 text-white',
                  piece.state === 'shipped' && 'border-foreground/20 bg-card',
                  piece.state === 'moving' && 'border-black/30 bg-signal text-black',
                )}
              >
                {piece.state === 'shipped' && grade ? <span className={GRADE_TONE[grade]}>{grade}</span> : piece.state === 'stopped' ? <X className="size-3 stroke-[3]" /> : null}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  )
}

export function StationDetail({ stage }: { stage: Stage | null }) {
  if (!stage)
    return (
      <p className="rounded-md bg-muted px-3 py-2.5 text-sm text-muted-foreground">
        Square lamps are <b className="text-foreground">gates</b>: a failure stops the line. Round lamps are <b className="text-foreground">checks</b>: they report but never stop it.
      </p>
    )
  return (
    <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1 rounded-md bg-muted px-3 py-2.5 text-sm" aria-live="polite">
      <b className="font-semibold">
        {stage.name}
        {stage.attempt && stage.attempt > 1 ? ` (attempt ${stage.attempt})` : ''}
      </b>
      <code className="font-mono text-xs text-muted-foreground">{stage.tool}</code>
      <span className="text-xs text-muted-foreground">
        {stage.gate ? 'Gate' : 'Check'}, {STATUS_WORD[stage.status].toLowerCase()}
      </span>
      <span>{stage.summary || (stage.status === 'pending' ? 'Waiting for earlier stations.' : '')}</span>
      {stage.status === 'running' && stage.log.length > 0 && (
        <code className="live-tail basis-full truncate font-mono text-xs text-muted-foreground">{stage.log[stage.log.length - 1]}</code>
      )}
    </div>
  )
}
