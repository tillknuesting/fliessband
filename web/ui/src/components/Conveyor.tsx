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

/** Where the work piece sits: the running station, the one that stopped the line, or the end. */
function piecePosition(stages: Stage[], runStatus?: Status) {
  const n = stages.length
  const at = (i: number) => ((i + 0.5) / n) * 100
  const running = stages.findIndex((s) => s.status === 'running')
  if (running >= 0) return { x: at(running), state: 'moving' as const }
  const failed = stages.findIndex((s) => s.status === 'failed')
  if (failed >= 0) return { x: at(failed), state: 'stopped' as const }
  if (runStatus === 'passed') return { x: 100, state: 'shipped' as const }
  let last = -1
  stages.forEach((s, i) => {
    if (s.status !== 'pending') last = i
  })
  if (last < 0) return null
  return { x: at(last), state: runStatus === 'canceled' ? ('stopped' as const) : ('moving' as const) }
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
  const piece = piecePosition(stages, runStatus)
  const done = piece ? Math.min(piece.x, 100) : 0
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
            const time = s.status === 'running' ? 'running' : s.status === 'skipped' ? 'skipped' : s.duration_ms ? ms(s.duration_ms) : ''
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
          <div className={cn('belt-done absolute inset-y-0 left-0 rounded-full transition-[width] duration-700 ease-out', running && 'belt-moving', piece?.state === 'stopped' && 'belt-stopped')} style={{ width: `${done}%` }} />
          {piece && (
            <div
              className={cn('absolute bottom-[7px] -translate-x-1/2 transition-[left] duration-700 ease-[cubic-bezier(.5,0,.2,1)]', piece.state === 'shipped' && '-translate-x-full')}
              style={{ left: `${piece.x}%` }}
            >
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
