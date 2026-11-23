import { Check, Minus, Ban, X } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import { ms } from '@/lib/format'
import { SHORT, STATUS_WORD } from '@/lib/stages'
import type { Stage } from '@/lib/types'

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

/**
 * The line: every station on a belt, grouped by phase. Square lamps are
 * gates (a failure stops the line), round lamps are checks.
 */
export function Conveyor({ stages, running, selected, onSelect }: { stages: Stage[]; running: boolean; selected?: string; onSelect: (id: string) => void }) {
  let last = -1
  stages.forEach((s, i) => {
    if (s.status !== 'pending') last = i
  })
  const width = last < 0 ? 0 : ((last + 0.5) / stages.length) * 100
  const phases: { name: string; n: number }[] = []
  for (const s of stages) {
    const p = s.phase ?? ''
    if (phases.length && phases[phases.length - 1].name === p) phases[phases.length - 1].n++
    else phases.push({ name: p, n: 1 })
  }
  const cols = { gridTemplateColumns: `repeat(${stages.length}, minmax(36px, 1fr))` }
  return (
    <div className="overflow-x-auto pb-1">
      <div className="relative min-w-[760px]">
        <div className="mb-2 grid" style={cols} aria-hidden>
          {phases.map((p, i) => (
            <span key={i} className="mx-1 border-b pb-1 text-center text-xs font-medium text-muted-foreground" style={{ gridColumn: `span ${p.n}` }}>
              {p.name}
            </span>
          ))}
        </div>
        <div className="absolute inset-x-0 top-[35px] h-1.5 rounded-full bg-muted ring-1 ring-border ring-inset" />
        <div className={cn('absolute left-0 top-[35px] h-1.5 rounded-full bg-signal transition-[width] duration-500', running && 'belt-moving')} style={{ width: `${width}%` }} />
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
                      className={cn('group flex w-full flex-col items-center gap-1 rounded-md px-0.5 pb-2 outline-none focus-visible:ring-2 focus-visible:ring-ring', selected === s.id && 'bg-muted')}
                    >
                      <span className={cn('z-10 grid size-7 place-items-center border-2', s.gate ? 'rounded-md' : 'rounded-full', LAMP[s.status])}>
                        <Icon status={s.status} />
                      </span>
                      <span className={cn('w-full truncate text-center text-[10.5px] font-medium leading-tight tracking-[-0.02em] text-foreground/75 group-hover:text-foreground', s.status === 'failed' && 'text-destructive')}>
                        {SHORT[s.id] ?? s.name}
                      </span>
                      <span className="min-h-3 whitespace-nowrap text-[10px] leading-none text-muted-foreground tabular-nums">{time}</span>
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
    </div>
  )
}
