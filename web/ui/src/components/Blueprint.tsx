import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { FileCode2, KeyRound, Sparkles } from 'lucide-react'
import { api } from '@/lib/api'
import { cn } from '@/lib/utils'
import type { Outline, OutlineField, Report, Run, Stage, Validation } from '@/lib/types'
import { Empty } from './Views'

type Open = (path: string, line?: number) => void
type Lamp = 'planned' | 'building' | 'built' | 'passed' | 'failed' | 'stub'

interface Rpc {
  name: string
  custom: boolean
  entity?: string
  touches: string[]
  doc?: string
  request: OutlineField[]
  response: OutlineField[]
  examples: number
  holdout: number
}

const LAMP_WORD: Record<Lamp, string> = {
  planned: 'In the spec',
  building: 'Being built',
  built: 'Built',
  passed: 'Verified',
  failed: 'Failing',
  stub: 'Stub',
}

const LAMP_CLASS: Record<Lamp, string> = {
  planned: 'border-muted-foreground/40 bg-transparent',
  building: 'bp-building border-signal',
  built: 'border-good/70 bg-good/25',
  passed: 'border-good bg-good',
  failed: 'border-destructive bg-destructive',
  stub: 'border-warn border-dashed bg-transparent',
}

// The outline of a run's own spec, fetched once per spec text.
const outlines = new Map<string, Promise<Outline | undefined>>()
function outlineOf(spec: string) {
  let p = outlines.get(spec)
  if (!p) {
    p = api('/api/validate', { method: 'POST', body: spec })
      .then((r) => r.json())
      .then((v: Validation) => v.outline)
      .catch(() => undefined)
    outlines.set(spec, p)
  }
  return p
}

function useOutline(run: Run | null, validation: Validation | null) {
  const [outline, setOutline] = useState<Outline | undefined>()
  const spec = run?.spec
  useEffect(() => {
    let live = true
    if (spec) outlineOf(spec).then((o) => live && setOutline(o))
    return () => {
      live = false
    }
  }, [spec])
  return spec ? outline : validation?.outline
}

function stageStatus(stages: Stage[] | undefined, id: string) {
  return stages?.find((s) => s.id === id)?.status
}

/** One lamp per RPC, from the finished report or, while the line runs, from how far it got. */
function lampFor(rpc: Rpc, run: Run | null): Lamp {
  if (!run) return 'planned'
  const rep = run.report
  if (rep) {
    if (rep.completeness.stubbed?.includes(rpc.name)) return 'stub'
    const ex = rep.acceptance?.examples.filter((e) => e.method === rpc.name) ?? []
    if (ex.some((e) => e.status === 'failed')) return 'failed'
    if (ex.length && ex.every((e) => e.status === 'passed')) return 'passed'
    const fn = rep.functions.find((f) => f.name === `Server.${rpc.name}`)
    return fn && (fn.coverage ?? 0) > 0 ? 'passed' : 'built'
  }
  const st = run.stages
  if (rpc.custom && !run.options?.llm && stageStatus(st, 'scaffold') === 'passed') return 'stub'
  if (stageStatus(st, 'acceptance') === 'passed') return 'passed'
  if (stageStatus(st, 'acceptance') === 'failed') return 'failed'
  const built = stageStatus(st, 'build') === 'passed'
  if (rpc.custom) return built ? 'built' : run.status === 'running' ? 'building' : 'planned'
  return built || stageStatus(st, 'scaffold') === 'passed' ? 'built' : run.status === 'running' ? 'building' : 'planned'
}

function Sig({ fields }: { fields: OutlineField[] }) {
  return (
    <>
      {fields.map((f, i) => (
        <span key={f.name}>
          {i > 0 && ', '}
          <span className="text-foreground">{f.name}</span> <span className="text-chart-1">{f.repeated ? `[]${f.type}` : f.type}</span>
        </span>
      ))}
    </>
  )
}

/** A cubic wire from an RPC's port to an entity's socket. */
function wirePath(x1: number, y1: number, x2: number, y2: number) {
  const mx = (x2 - x1) * 0.55
  return `M${x1},${y1} C${x1 + mx},${y1} ${x2 - mx},${y2} ${x2},${y2}`
}

/**
 * The service as a blueprint: every RPC on the left, every entity on the
 * right, a wire wherever an RPC reads or writes an entity. Hover follows the
 * wires; click or the arrow keys open an RPC's spec, examples and code.
 */
export function Blueprint({ run, validation, open }: { run: Run | null; validation: Validation | null; open: Open }) {
  const outline = useOutline(run, validation)
  const rpcs = useMemo<Rpc[]>(() => {
    if (!outline) return []
    const custom: Rpc[] = outline.methods.map((m) => ({ ...m, custom: true }))
    const crud: Rpc[] = outline.entities.flatMap((e) =>
      e.rpcs.map((name) => ({ name, custom: false, entity: e.name, touches: [e.name], request: [], response: [], examples: 0, holdout: 0 })),
    )
    return [...custom, ...crud]
  }, [outline])

  const [selected, setSelected] = useState<string | null>(null)
  const [hover, setHover] = useState<{ rpc?: string; entity?: string } | null>(null)
  const sel = rpcs.find((r) => r.name === selected) ?? rpcs[0]

  // Wires are measured from the DOM so they follow any layout.
  const box = useRef<HTMLDivElement>(null)
  const rows = useRef(new Map<string, HTMLElement>())
  const cards = useRef(new Map<string, HTMLElement>())
  const [wires, setWires] = useState<{ id: string; rpc: string; entity: string; d: string; x2: number; y2: number }[]>([])
  const [size, setSize] = useState({ w: 0, h: 0 })
  const measure = useCallback(() => {
    const b = box.current?.getBoundingClientRect()
    if (!b) return
    const incoming = new Map<string, number>()
    const total = new Map<string, number>()
    for (const r of rpcs) for (const e of r.touches) total.set(e, (total.get(e) ?? 0) + 1)
    const out: typeof wires = []
    for (const r of rpcs) {
      const row = rows.current.get(r.name)?.getBoundingClientRect()
      if (!row) continue
      for (const e of r.touches) {
        const card = cards.current.get(e)?.getBoundingClientRect()
        if (!card) continue
        // Spread wires over the card's edge so they don't all meet in one point.
        const k = incoming.get(e) ?? 0
        incoming.set(e, k + 1)
        const span = Math.min(card.height - 24, 14 * (total.get(e) ?? 1))
        const y2 = card.top - b.top + 18 + (span * (k + 0.5)) / (total.get(e) ?? 1)
        const x2 = card.left - b.left
        out.push({ id: `${r.name}>${e}`, rpc: r.name, entity: e, x2, y2, d: wirePath(row.right - b.left, row.top - b.top + row.height / 2, x2, y2) })
      }
    }
    setWires(out)
    setSize({ w: b.width, h: b.height })
  }, [rpcs])
  useLayoutEffect(() => {
    measure()
    const ro = new ResizeObserver(measure)
    if (box.current) ro.observe(box.current)
    return () => ro.disconnect()
  }, [measure])

  const lamps = useMemo(() => new Map(rpcs.map((r) => [r.name, lampFor(r, run)])), [rpcs, run])
  const running = run?.status === 'running'

  if (!outline)
    return <Empty title="No service to show">Fix the spec errors listed under the editor and the service's blueprint appears here as you type.</Empty>

  const lit = (rpc: string, entity: string) => (hover?.rpc ? hover.rpc === rpc : hover?.entity ? hover.entity === entity : sel?.name === rpc)
  const rpcLit = (r: Rpc) => (hover?.rpc ? hover.rpc === r.name : hover?.entity ? r.touches.includes(hover.entity) : false)
  const entityLit = (e: string) => (hover?.rpc ? !!rpcs.find((r) => r.name === hover.rpc)?.touches.includes(e) : hover?.entity === e)
  const dim = !!hover

  const move = (d: number) => {
    const i = rpcs.findIndex((r) => r.name === sel?.name)
    const next = rpcs[(i + d + rpcs.length) % rpcs.length]
    setSelected(next.name)
    rows.current.get(next.name)?.focus()
  }

  const service = validation?.service && !run ? validation.service : undefined
  const groups: { title: string; note: string; items: Rpc[] }[] = [
    { title: 'Custom methods', note: 'written by the LLM', items: rpcs.filter((r) => r.custom) },
    ...(outline.entities.map((e) => ({ title: e.name, note: 'from templates', items: rpcs.filter((r) => r.entity === e.name) })) ?? []),
  ].filter((g) => g.items.length)

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 text-[12.5px] text-muted-foreground">
        {service && <span className="font-mono text-foreground">{service}</span>}
        <span>
          {rpcs.length} RPCs, {outline.entities.length} entities
        </span>
        <span className="flex flex-wrap gap-1">
          {outline.features.map((f) => (
            <span key={f} className="rounded border px-1.5 py-px text-[11px]">
              {f}
            </span>
          ))}
        </span>
        <span className="ml-auto flex items-center gap-3">
          {(['passed', 'built', 'building', 'failed', 'stub'] as Lamp[])
            .filter((l) => [...lamps.values()].includes(l))
            .map((l) => (
              <span key={l} className="flex items-center gap-1.5">
                <span className={cn('size-2.5 rounded-[3px] border', LAMP_CLASS[l])} />
                {LAMP_WORD[l]}
              </span>
            ))}
        </span>
      </div>

      <div ref={box} className="line-floor relative grid grid-cols-[minmax(210px,290px)_minmax(80px,1fr)_minmax(220px,300px)] rounded-lg border p-4" onMouseLeave={() => setHover(null)}>
        <svg className="pointer-events-none absolute inset-0" width={size.w} height={size.h} aria-hidden>
          {wires.map((w, i) => {
            const on = lit(w.rpc, w.entity)
            return (
              <g key={w.id} className={cn('transition-opacity duration-150', dim && !on && 'opacity-30')}>
                <path d={w.d} pathLength={1} className="bp-wire-draw" style={{ animationDelay: `${i * 18}ms` }} fill="none" stroke={on ? 'var(--signal)' : 'color-mix(in oklch, var(--foreground) 22%, transparent)'} strokeWidth={on ? 1.75 : 1} />
                {(on || running) && <path d={w.d} fill="none" className="bp-flow" stroke={on ? 'var(--signal)' : 'color-mix(in oklch, var(--signal) 60%, transparent)'} strokeWidth={on ? 2.5 : 1.5} strokeLinecap="round" />}
                <circle r="2.5" cx={w.x2} cy={w.y2} fill={on ? 'var(--signal)' : 'var(--muted-foreground)'} />
              </g>
            )
          })}
        </svg>

        <div
          role="listbox"
          aria-label="RPCs"
          className="relative space-y-3"
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown' || e.key === 'j') {
              e.preventDefault()
              move(1)
            } else if (e.key === 'ArrowUp' || e.key === 'k') {
              e.preventDefault()
              move(-1)
            }
          }}
        >
          {groups.map((g) => (
            <div key={g.title}>
              <div className="mb-1 flex items-baseline gap-1.5 px-2 text-[11.5px]">
                {g.items[0].custom && <Sparkles className="size-3 self-center text-signal" />}
                <span className="font-medium text-foreground/80">{g.title}</span>
                <span className="text-muted-foreground">{g.note}</span>
              </div>
              {g.items.map((r) => {
                const lamp = lamps.get(r.name) ?? 'planned'
                const isSel = sel?.name === r.name
                return (
                  <button
                    key={r.name}
                    ref={(el) => {
                      if (el) rows.current.set(r.name, el)
                      else rows.current.delete(r.name)
                    }}
                    role="option"
                    aria-selected={isSel}
                    tabIndex={isSel ? 0 : -1}
                    onClick={() => setSelected(r.name)}
                    onMouseEnter={() => setHover({ rpc: r.name })}
                    onFocus={() => setSelected(r.name)}
                    className={cn(
                      'group relative flex h-7 w-full items-center gap-2 rounded-md border border-transparent bg-background/0 px-2 text-left font-mono text-[12.5px] transition-[background-color,border-color,opacity] duration-150 outline-none',
                      'hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring',
                      isSel && 'border-border bg-card shadow-xs',
                      dim && !rpcLit(r) && !isSel && 'opacity-45',
                    )}
                  >
                    <span className={cn('size-2.5 shrink-0 rounded-[3px] border transition-colors duration-300', LAMP_CLASS[lamp])} title={LAMP_WORD[lamp]} />
                    <span className="truncate">{r.name}</span>
                    {r.examples > 0 && <span className="ml-auto shrink-0 font-sans text-[11px] text-muted-foreground tabular-nums">{r.examples} ex</span>}
                    {/* The port the wires leave from. */}
                    <span className={cn('absolute -right-[3px] size-1.5 rounded-full transition-colors', isSel || rpcLit(r) ? 'bg-signal' : 'bg-muted-foreground/50')} />
                  </button>
                )
              })}
            </div>
          ))}
        </div>

        <div />

        <div className="relative space-y-3">
          {outline.entities.map((e) => {
            const on = entityLit(e.name) || (!hover && !!sel?.touches.includes(e.name))
            return (
              <div
                key={e.name}
                ref={(el) => {
                  if (el) cards.current.set(e.name, el)
                  else cards.current.delete(e.name)
                }}
                onMouseEnter={() => setHover({ entity: e.name })}
                className={cn('rounded-lg border bg-card/90 shadow-xs backdrop-blur-[2px] transition-[border-color,opacity,box-shadow] duration-150', on && 'border-signal/60 shadow-[0_0_0_3px_color-mix(in_oklch,var(--signal)_14%,transparent)]', dim && !on && 'opacity-55')}
              >
                <div className="flex items-center gap-2 border-b px-3 py-1.5">
                  <span className="text-[13px] font-semibold">{e.name}</span>
                  <span className="ml-auto text-[11px] text-muted-foreground">{e.fields.length + 3} columns</span>
                </div>
                <ul className="px-3 py-1.5 font-mono text-[11.5px]">
                  <li className="flex gap-2 text-muted-foreground">
                    <span className="w-3">
                      <KeyRound className="size-3" />
                    </span>
                    id <span className="ml-auto">uuid v7</span>
                  </li>
                  {e.fields.map((f) => (
                    <li key={f.name} className="flex gap-2">
                      <span className="w-3 text-muted-foreground" title={f.unique ? 'unique' : f.required ? 'required' : undefined}>
                        {f.unique ? '◆' : f.required ? '•' : ''}
                      </span>
                      <span className="truncate">{f.name}</span>
                      <span className="ml-auto shrink-0 text-chart-1">{f.repeated ? `[]${f.type}` : f.type}</span>
                    </li>
                  ))}
                </ul>
              </div>
            )
          })}
        </div>
      </div>

      {sel && <RpcDetail key={sel.name} rpc={sel} lamp={lamps.get(sel.name) ?? 'planned'} rep={run?.report} open={open} />}
    </div>
  )
}

function RpcDetail({ rpc, lamp, rep, open }: { rpc: Rpc; lamp: Lamp; rep?: Report; open: Open }) {
  const fn = rep?.functions.find((f) => f.name === `Server.${rpc.name}`)
  const tests = rep?.functions.filter((f) => f.test && f.name.startsWith(`Test${rpc.name}`)).length ?? 0
  const examples = rep?.acceptance?.examples.filter((e) => e.method === rpc.name) ?? []
  const verb = rpc.custom ? null : rpc.name.replace(rpc.entity ?? '', '').replace(/s$/, '')
  return (
    <div className="grid animate-in gap-4 rounded-lg border bg-card p-4 duration-200 fade-in-0 slide-in-from-bottom-1 md:grid-cols-[1fr_auto]">
      <div className="min-w-0 space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <span className={cn('size-2.5 rounded-[3px] border', LAMP_CLASS[lamp])} />
          <h3 className="font-mono text-[14px] font-semibold">{rpc.name}</h3>
          <span className="text-[12px] text-muted-foreground">{LAMP_WORD[lamp]}</span>
          <span className={cn('rounded border px-1.5 py-px text-[11px]', rpc.custom ? 'border-signal/50 text-foreground' : 'text-muted-foreground')}>{rpc.custom ? 'LLM' : 'template'}</span>
        </div>
        {rpc.custom ? (
          <p className="font-mono text-[12.5px] text-muted-foreground">
            ({<Sig fields={rpc.request} />}) → ({<Sig fields={rpc.response} />})
          </p>
        ) : (
          <p className="text-[13px] text-muted-foreground">
            Generated {verb?.toLowerCase()} for {rpc.entity}: validation, store calls and error codes come from reviewed templates, and the shared store contract suite tests it on memory and PostgreSQL.
          </p>
        )}
        {rpc.doc && <p className="max-w-[75ch] text-[13px] leading-relaxed">{rpc.doc}</p>}
        {examples.length > 0 ? (
          <ul className="space-y-1 pt-1">
            {examples.map((e) => (
              <li key={e.name} className="flex items-center gap-2 text-[13px]">
                <span className={cn('size-2 rounded-full', e.status === 'passed' ? 'bg-good' : e.status === 'failed' ? 'bg-destructive' : 'bg-muted-foreground/50')} />
                {e.name}
                {e.holdout && <span className="rounded border px-1 text-[10.5px] text-muted-foreground">holdout</span>}
                {e.message && <span className="truncate text-xs text-destructive">{e.message}</span>}
              </li>
            ))}
          </ul>
        ) : (
          rpc.examples > 0 && (
            <p className="text-[12.5px] text-muted-foreground">
              {rpc.examples} examples in the spec{rpc.holdout ? `, ${rpc.holdout} of them holdout` : ''}. Run the line to check them.
            </p>
          )
        )}
      </div>
      {fn && (
        <div className="grid grid-cols-2 content-start gap-x-6 gap-y-2 text-[12px] md:w-60">
          <Metric label="Coverage" value={fn.coverage != null ? `${Math.round(fn.coverage)}%` : '–'} bar={fn.coverage} />
          <Metric label="Cyclomatic" value={String(fn.cyclomatic)} />
          <Metric label="Cognitive" value={String(fn.cognitive)} />
          <Metric label="Lines" value={String(fn.loc)} />
          {rpc.custom && <Metric label="Unit tests" value={String(tests)} />}
          <button onClick={() => open(fn.file, fn.line)} className="col-span-2 mt-1 flex h-7 items-center justify-center gap-1.5 rounded-md border text-[12.5px] hover:bg-accent">
            <FileCode2 className="size-3.5" /> Open {fn.file.split('/').pop()}:{fn.line}
          </button>
        </div>
      )}
    </div>
  )
}

function Metric({ label, value, bar }: { label: string; value: string; bar?: number }) {
  return (
    <div>
      <div className="text-muted-foreground">{label}</div>
      <div className="text-[15px] font-semibold tabular-nums">{value}</div>
      {bar != null && (
        <div className="mt-1 h-1 rounded-full bg-muted">
          <div className="h-1 rounded-full bg-chart-1" style={{ width: `${bar}%` }} />
        </div>
      )}
    </div>
  )
}
