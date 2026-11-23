import { useMemo, useState } from 'react'
import { FileCode2, ShieldAlert, ShieldCheck, ShieldMinus, ShieldX } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { useTween } from '@/lib/tween'
import { cn } from '@/lib/utils'
import type { Run, SecurityCheck } from '@/lib/types'
import { Empty } from './Views'

type Open = (path: string, line?: number) => void
type St = SecurityCheck['status']

export const SEC: Record<St, { word: string; icon: typeof ShieldCheck; text: string; bg: string }> = {
  fail: { word: 'Failing', icon: ShieldX, text: 'text-destructive', bg: 'bg-destructive' },
  warn: { word: 'Warning', icon: ShieldAlert, text: 'text-warn', bg: 'bg-warn' },
  pass: { word: 'Passing', icon: ShieldCheck, text: 'text-good', bg: 'bg-good' },
  skip: { word: 'Not applicable', icon: ShieldMinus, text: 'text-muted-foreground', bg: 'bg-muted-foreground/30' },
}
const ORDER: St[] = ['fail', 'warn', 'pass', 'skip']

/**
 * The security audit of a run: every control the line checked in the files
 * it produced, grouped by area, with the file and line it read as evidence.
 */
export function Security({ run, open }: { run: Run | null; open: Open }) {
  const sec = run?.report?.security
  const [filter, setFilter] = useState<St | 'all'>('all')
  const [area, setArea] = useState<string | null>(null)
  const areas = useMemo(() => {
    const m = new Map<string, SecurityCheck[]>()
    for (const c of sec?.checks ?? []) m.set(c.area, [...(m.get(c.area) ?? []), c])
    return [...m.entries()]
  }, [sec])
  const applicable = sec ? sec.passed + sec.warned + sec.failed : 0
  const share = useTween(applicable ? (sec!.passed / applicable) * 100 : 0, 900)

  if (!run) return <Empty title="No run yet">Run the line and the generated service is audited here: transport, abuse limits, input validation, container, Kubernetes, CI, code and supply chain.</Empty>
  if (!sec)
    return (
      <Empty title={run.status === 'running' ? 'The audit runs at the end of the line' : 'No security audit for this run'}>
        {run.status === 'running' ? 'The Measure station audits the service once every other station has finished.' : 'This run was made before the audit existed, or stopped before the Measure station. Rerun it to audit the service.'}
      </Empty>
    )

  const counts: Record<St, number> = { pass: sec.passed, warn: sec.warned, fail: sec.failed, skip: sec.checks.length - applicable }
  const visible = (c: SecurityCheck) => (filter === 'all' || c.status === filter) && (!area || c.area === area)
  const verdict = sec.failed ? `${sec.failed} control${sec.failed > 1 ? 's' : ''} failing` : sec.warned ? `No failures, ${sec.warned} to harden` : 'Every control passes'

  return (
    <div className="space-y-4">
      <div className="grid gap-4 rounded-lg border bg-card p-4 md:grid-cols-[auto_1fr]">
        <div className="flex items-center gap-4">
          <div className="relative grid size-[88px] place-items-center">
            <svg viewBox="0 0 88 88" className="absolute inset-0 -rotate-90">
              <circle cx="44" cy="44" r="38" fill="none" stroke="var(--muted)" strokeWidth="7" />
              <circle cx="44" cy="44" r="38" fill="none" stroke={sec.failed ? 'var(--destructive)' : 'var(--good)'} strokeWidth="7" strokeLinecap="round" strokeDasharray={2 * Math.PI * 38} strokeDashoffset={2 * Math.PI * 38 * (1 - share / 100)} />
            </svg>
            <div className="text-center">
              <div className="text-[20px] leading-none font-bold tabular-nums">{Math.round(share)}%</div>
              <div className="text-[10.5px] text-muted-foreground">passing</div>
            </div>
          </div>
          <div>
            <div className="flex items-center gap-1.5 text-[15px] font-semibold">
              {(() => {
                const I = SEC[sec.failed ? 'fail' : sec.warned ? 'warn' : 'pass'].icon
                return <I className={cn('size-4', SEC[sec.failed ? 'fail' : sec.warned ? 'warn' : 'pass'].text)} />
              })()}
              {verdict}
            </div>
            <p className="mt-0.5 max-w-[46ch] text-[12.5px] text-muted-foreground">
              {sec.passed} of {applicable} controls pass. Each one was checked in the files this run produced or in a station's result; click a file to see the line.
            </p>
          </div>
        </div>
        {/* One row per area; each square is a control. Click an area to show only it. */}
        <div className="space-y-1 self-center">
          {areas.map(([name, cs]) => (
            <button
              key={name}
              onClick={() => setArea(area === name ? null : name)}
              aria-pressed={area === name}
              className={cn('grid w-full grid-cols-[110px_1fr_auto] items-center gap-3 rounded px-1.5 py-0.5 text-left text-[12px] transition-colors hover:bg-accent', area === name && 'bg-accent', area && area !== name && 'opacity-50')}
            >
              <span className="truncate text-muted-foreground">{name}</span>
              <span className="flex flex-wrap gap-[3px]">
                {cs.map((c) => (
                  <Tooltip key={c.id}>
                    <TooltipTrigger asChild>
                      <span className={cn('size-3 rounded-[3px] transition-transform hover:scale-125', SEC[c.status].bg)} />
                    </TooltipTrigger>
                    <TooltipContent>
                      {SEC[c.status].word}: {c.title}
                    </TooltipContent>
                  </Tooltip>
                ))}
              </span>
              <span className="text-muted-foreground tabular-nums">
                {cs.filter((c) => c.status === 'pass').length}/{cs.filter((c) => c.status !== 'skip').length}
              </span>
            </button>
          ))}
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-1.5">
        {(['all', ...ORDER] as const).map((f) => {
          const n = f === 'all' ? sec.checks.length : counts[f]
          if (f !== 'all' && n === 0) return null
          return (
            <button
              key={f}
              onClick={() => setFilter(f)}
              aria-pressed={filter === f}
              className={cn('flex h-7 items-center gap-1.5 rounded-md border px-2.5 text-[12.5px] transition-colors', filter === f ? 'border-signal/50 bg-signal/10' : 'text-muted-foreground hover:bg-accent')}
            >
              {f !== 'all' && <span className={cn('size-2 rounded-full', SEC[f].bg)} />}
              {f === 'all' ? 'All' : SEC[f].word}
              <span className="tabular-nums opacity-70">{n}</span>
            </button>
          )
        })}
        {area && (
          <button onClick={() => setArea(null)} className="ml-1 text-[12px] text-muted-foreground underline-offset-2 hover:underline">
            Showing {area} only, show all areas
          </button>
        )}
      </div>

      <div className="space-y-3">
        {areas.map(([name, cs]) => {
          const shown = [...cs].filter(visible).sort((a, b) => ORDER.indexOf(a.status) - ORDER.indexOf(b.status))
          if (!shown.length) return null
          return (
            <section key={name} className="animate-in rounded-lg border bg-card duration-200 fade-in-0">
              <h3 className="border-b px-4 py-2 text-[12.5px] font-semibold">{name}</h3>
              <ul className="divide-y">
                {shown.map((c) => {
                  const s = SEC[c.status]
                  return (
                    <li key={c.id} className="grid grid-cols-[18px_1fr_auto] items-start gap-3 px-4 py-2.5">
                      <s.icon className={cn('mt-0.5 size-4', s.text)} aria-label={s.word} />
                      <div className="min-w-0">
                        <div className={cn('text-[13px] font-medium', c.status === 'skip' && 'text-muted-foreground')}>{c.title}</div>
                        <p className="max-w-[90ch] text-[12.5px] leading-relaxed text-muted-foreground">{c.detail}</p>
                      </div>
                      {c.file && c.file !== 'spec.yaml' ? (
                        <button
                          onClick={() => open(c.file!, c.line)}
                          className="flex shrink-0 items-center gap-1 rounded border px-1.5 py-0.5 font-mono text-[11.5px] text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
                          title="Open the file at this line"
                        >
                          <FileCode2 className="size-3" />
                          {c.file.split('/').pop()}
                          {c.line ? `:${c.line}` : ''}
                        </button>
                      ) : c.file === 'spec.yaml' ? (
                        <span className="font-mono text-[11.5px] text-muted-foreground">spec</span>
                      ) : (
                        <span />
                      )}
                    </li>
                  )
                })}
              </ul>
            </section>
          )
        })}
      </div>
    </div>
  )
}
