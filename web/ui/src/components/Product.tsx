import { useEffect, useMemo, useState } from 'react'
import { ArrowDown, ArrowRight, ArrowUp, RotateCw, Sparkles } from 'lucide-react'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { api, getJSON, postJSON } from '@/lib/api'
import { changed, diffLines, hunks } from '@/lib/diff'
import { ms } from '@/lib/format'
import { useTween } from '@/lib/tween'
import { cn } from '@/lib/utils'
import type { FactoryStatus, Run, Summary } from '@/lib/types'
import { ago, StatusIcon } from './Shell'

const GRADE_COLOR: Record<string, string> = { A: 'var(--good)', B: 'var(--good)', C: 'var(--warn)', D: 'var(--destructive)', F: 'var(--destructive)' }
const OWNED = ['internal/service/custom.go', 'internal/service/custom_test.go']

/** Every product's versions in order, oldest first; a product is a spec name. */
export function versionsOf(runs: Summary[], name: string) {
  return runs.filter((r) => r.name === name).sort((a, b) => a.created_at.localeCompare(b.created_at))
}

export function startRerun(id: string, improve: boolean) {
  return postJSON<{ id: string }>(`/api/runs/${encodeURIComponent(id)}/rerun`, { improve })
}

/**
 * A product over its versions: how the score moved, what changed between
 * any two versions, and controls to run it again or improve on a version.
 */
export function Product({ name, runs, status }: { name: string; runs: Summary[]; status: FactoryStatus | null }) {
  const versions = useMemo(() => versionsOf(runs, name), [runs, name])
  const num = useMemo(() => new Map(versions.map((v, i) => [v.id, i + 1])), [versions])
  const finished = versions.filter((v) => v.status !== 'running')
  const [b, setB] = useState<string | null>(null)
  const [a, setA] = useState<string | null>(null)
  const B = versions.find((v) => v.id === b) ?? finished[finished.length - 1]
  const prev = B ? finished[finished.findIndex((v) => v.id === B.id) - 1] : undefined
  const A = versions.find((v) => v.id === a) ?? versions.find((v) => v.id === B?.base) ?? prev

  const [full, setFull] = useState<Record<string, Run>>({})
  useEffect(() => {
    for (const v of [A, B]) {
      if (v && v.status !== 'running' && !full[v.id]) getJSON<Run>(`/api/runs/${encodeURIComponent(v.id)}`).then((r) => setFull((f) => ({ ...f, [r.id]: r })), () => {})
    }
  }, [A, B, full])

  if (!versions.length) return <p className="p-10 text-center text-sm text-muted-foreground">No runs of {name} yet.</p>

  const pick = (id: string, asBase: boolean) => {
    if (asBase) setA(id)
    else {
      setB(id)
      setA(null)
    }
  }

  return (
    <div className="space-y-4 p-5">
      <Header name={name} versions={versions} B={B} num={num} status={status} />
      <Track versions={versions} num={num} a={A?.id} b={B?.id} onPick={pick} />
      {A && B && A.id !== B.id ? (
        <Compare A={A} B={B} na={num.get(A.id)!} nb={num.get(B.id)!} ra={full[A.id]} rb={full[B.id]} />
      ) : (
        <p className="rounded-lg border border-dashed p-6 text-center text-[13px] text-muted-foreground">
          {finished.length < 2 ? 'Run this product again to compare versions. Improve starts the next version from this one and fixes what it measured.' : 'Pick two different versions to compare.'}
        </p>
      )}
    </div>
  )
}

function Header({ name, versions, B, num, status }: { name: string; versions: Summary[]; B?: Summary; num: Map<string, number>; status: FactoryStatus | null }) {
  const latest = [...versions].reverse().find((v) => v.score != null)
  const first = versions.find((v) => v.score != null)
  const score = useTween(latest?.score ?? 0)
  const [busy, setBusy] = useState<string | null>(null)
  const [msg, setMsg] = useState<string | null>(null)
  const go = async (improve: boolean) => {
    if (!B) return
    setBusy(improve ? 'improve' : 'rerun')
    setMsg(null)
    try {
      await startRerun(B.id, improve)
      setMsg(`v${versions.length + 1} is on the line${improve ? `, starting from v${num.get(B.id)}` : ''}.`)
    } catch (e) {
      setMsg((e as Error).message)
    } finally {
      setBusy(null)
    }
  }
  const delta = latest && first && latest !== first ? latest.score! - first.score! : null
  return (
    <div className="flex flex-wrap items-center gap-x-6 gap-y-3">
      <div>
        <h1 className="text-[20px] font-semibold tracking-[-0.02em]">{name}</h1>
        <p className="text-[12.5px] text-muted-foreground">
          {versions.length} versions, first {ago(versions[0].created_at)}
        </p>
      </div>
      {latest && (
        <div className="flex items-baseline gap-2">
          <span className="text-[30px] leading-none font-bold tabular-nums">{Math.round(score)}</span>
          <span className="font-bold" style={{ color: GRADE_COLOR[latest.grade ?? ''] }}>
            {latest.grade}
          </span>
          {delta != null && <Delta value={delta} digits={0} suffix=" since v1" />}
        </div>
      )}
      <div className="ml-auto flex flex-col items-end gap-1.5">
        <div className="flex gap-2">
          <button
            onClick={() => go(false)}
            disabled={!!busy || !B}
            className="flex h-8 items-center gap-1.5 rounded-md border px-3 text-[13px] font-medium transition-colors hover:bg-accent disabled:opacity-50"
            title="Run the same spec with the same settings again"
          >
            <RotateCw className={cn('size-3.5', busy === 'rerun' && 'animate-spin')} /> Rerun v{B ? num.get(B.id) : ''}
          </button>
          <Tooltip>
            <TooltipTrigger asChild>
              <button
                onClick={() => go(true)}
                disabled={!!busy || !B}
                className="flex h-8 items-center gap-1.5 rounded-md bg-signal px-3 text-[13px] font-medium text-signal-foreground transition-[filter,transform] hover:brightness-110 active:scale-[0.98] disabled:opacity-50"
              >
                <Sparkles className={cn('size-3.5', busy === 'improve' && 'animate-pulse')} /> Improve v{B ? num.get(B.id) : ''}
              </button>
            </TooltipTrigger>
            <TooltipContent className="max-w-72">
              {status?.llm
                ? 'Starts the next version from this one. The model gets its code and what it lost points on: surviving mutants, failing visible examples, review findings, complex or uncovered functions. Holdout examples stay hidden.'
                : 'No LLM configured: the next version carries this implementation forward unchanged and the line measures it again. Start fliessband with an LLM key to improve it.'}
            </TooltipContent>
          </Tooltip>
        </div>
        {msg && <p className="animate-in text-[12px] text-muted-foreground fade-in-0 slide-in-from-top-1">{msg}</p>}
      </div>
    </div>
  )
}

/** The versions on a score track; click picks the version to inspect, shift-click the one to compare against. */
function Track({ versions, num, a, b, onPick }: { versions: Summary[]; num: Map<string, number>; a?: string; b?: string; onPick: (id: string, asBase: boolean) => void }) {
  const W = Math.max(520, versions.length * 64)
  const H = 150
  const pad = { l: 30, r: 24, t: 18, b: 26 }
  const x = (i: number) => pad.l + (versions.length < 2 ? (W - pad.l - pad.r) / 2 : (i / (versions.length - 1)) * (W - pad.l - pad.r))
  const y = (v: number) => pad.t + (1 - v / 100) * (H - pad.t - pad.b)
  const scored = versions.map((v, i) => ({ v, i })).filter((p) => p.v.score != null)
  return (
    <section className="rounded-lg border bg-card">
      <header className="flex items-baseline gap-2 border-b px-4 py-2.5 text-[12.5px]">
        <h2 className="text-[13px] font-semibold">Versions</h2>
        <span className="text-muted-foreground">click a version to inspect it, shift-click to compare against it</span>
        <span className="ml-auto flex items-center gap-3 text-muted-foreground">
          <span className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-full border-2 border-signal" /> inspecting
          </span>
          <span className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-full border-2 border-dashed border-foreground/50" /> compared against
          </span>
        </span>
      </header>
      <div className="overflow-x-auto p-2">
        <svg viewBox={`0 0 ${W} ${H}`} style={{ width: W, maxWidth: '100%', minWidth: Math.min(W, 520) }} className="block select-none" role="img" aria-label="Score per version">
          {[0, 50, 100].map((v) => (
            <g key={v}>
              <line x1={pad.l} x2={W - pad.r} y1={y(v)} y2={y(v)} stroke="var(--border)" />
              <text x={pad.l - 8} y={y(v)} textAnchor="end" dominantBaseline="middle" className="fill-muted-foreground text-[10px]">
                {v}
              </text>
            </g>
          ))}
          {/* Lineage: an improved version links back to the version it started from. */}
          {versions.map((v, i) => {
            const j = versions.findIndex((p) => p.id === v.base)
            if (j < 0 || v.score == null || versions[j].score == null) return null
            const x1 = x(j)
            const x2 = x(i)
            const y1 = y(versions[j].score!)
            const y2 = y(v.score)
            return <path key={`l-${v.id}`} d={`M${x1},${y1 - 12} Q${(x1 + x2) / 2},${Math.min(y1, y2) - 34} ${x2},${y2 - 12}`} fill="none" stroke="var(--signal)" strokeOpacity="0.5" strokeDasharray="3 3" className="bp-wire-draw" pathLength={1} />
          })}
          <polyline points={scored.map((p) => `${x(p.i)},${y(p.v.score!)}`).join(' ')} fill="none" stroke="color-mix(in oklch, var(--foreground) 30%, transparent)" strokeWidth="1.5" />
          {versions.map((v, i) => {
            const cy = v.score != null ? y(v.score) : y(0)
            const isB = v.id === b
            const isA = v.id === a
            return (
              <g
                key={v.id}
                transform={`translate(${x(i)},${cy})`}
                onClick={(e) => v.status !== 'running' && onPick(v.id, e.shiftKey)}
                role="button"
                tabIndex={v.status === 'running' ? -1 : 0}
                onKeyDown={(e) => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault()
                    if (v.status !== 'running') onPick(v.id, e.shiftKey)
                  }
                }}
                aria-label={`Version ${i + 1}, ${v.grade ?? v.status}`}
                className="cursor-pointer outline-none [&:focus-visible>circle:first-child]:stroke-ring [&:focus-visible>circle:first-child]:[stroke-width:2]"
              >
                <circle r="16" fill="transparent" />
                {isB && <circle r="13" fill="none" stroke="var(--signal)" strokeWidth="2" className="animate-in zoom-in-50 duration-200" />}
                {isA && <circle r="13" fill="none" stroke="var(--foreground)" strokeOpacity="0.5" strokeWidth="1.5" strokeDasharray="3 2.5" />}
                <circle
                  r="9"
                  fill={v.status === 'running' ? 'var(--card)' : (GRADE_COLOR[v.grade ?? ''] ?? 'var(--muted-foreground)')}
                  stroke={v.status === 'running' ? 'var(--signal)' : 'var(--card)'}
                  strokeWidth="2"
                  className={cn('transition-transform duration-150 hover:scale-125', v.status === 'running' && 'mini-running')}
                />
                <text y="0.5" textAnchor="middle" dominantBaseline="middle" className="pointer-events-none fill-white text-[9px] font-bold">
                  {v.status === 'running' ? '' : v.grade ?? '–'}
                </text>
                <text y={H - cy - 8} textAnchor="middle" className={cn('text-[10.5px]', isB ? 'fill-foreground font-semibold' : 'fill-muted-foreground')}>
                  v{num.get(v.id)}
                </text>
              </g>
            )
          })}
        </svg>
      </div>
    </section>
  )
}

function Delta({ value, digits = 1, suffix = '', lowerIsBetter = false }: { value: number; digits?: number; suffix?: string; lowerIsBetter?: boolean }) {
  const shown = useTween(value)
  const eps = Math.pow(10, -digits) / 2
  if (Math.abs(value) < eps) return <span className="flex items-center gap-0.5 text-[12px] text-muted-foreground"><ArrowRight className="size-3" />same</span>
  const good = lowerIsBetter ? value < 0 : value > 0
  const Icon = value > 0 ? ArrowUp : ArrowDown
  return (
    <span className={cn('flex items-center gap-0.5 text-[12px] font-medium tabular-nums', good ? 'text-good' : 'text-destructive')}>
      <Icon className="size-3" />
      {value > 0 ? '+' : '−'}
      {Math.abs(shown).toFixed(digits)}
      {suffix}
    </span>
  )
}

const pct100 = (v?: number) => (v == null ? undefined : v <= 1 ? v * 100 : v)

type Metric = { label: string; a?: number; b?: number; fmt: (v: number) => string; digits?: number; lower?: boolean; unit?: string }

function Compare({ A, B, na, nb, ra, rb }: { A: Summary; B: Summary; na: number; nb: number; ra?: Run; rb?: Run }) {
  const pctOf = (r: Summary) => (r.examples ? (r.examples_passed / r.examples) * 100 : undefined)
  const metrics: Metric[] = [
    { label: 'Score', a: A.score, b: B.score, fmt: (v) => v.toFixed(0), digits: 0 },
    { label: 'Mutation score', a: pct100(A.mutation_score), b: pct100(B.mutation_score), fmt: (v) => `${v.toFixed(0)}%`, digits: 0, unit: ' pt' },
    { label: 'Coverage', a: A.coverage, b: B.coverage, fmt: (v) => `${v.toFixed(1)}%`, unit: ' pt' },
    { label: 'Examples passing', a: pctOf(A), b: pctOf(B), fmt: (v) => `${v.toFixed(0)}%`, digits: 0, unit: ' pt' },
    { label: 'Tests', a: A.tests, b: B.tests, fmt: (v) => v.toFixed(0), digits: 0 },
    { label: 'Mean cyclomatic', a: A.mean_cyclomatic, b: B.mean_cyclomatic, fmt: (v) => v.toFixed(1), lower: true },
    { label: 'Findings', a: A.findings, b: B.findings, fmt: (v) => v.toFixed(0), digits: 0, lower: true },
    { label: 'Line time', a: A.duration_ms / 1000, b: B.duration_ms / 1000, fmt: (v) => ms(v * 1000), digits: 0, lower: true, unit: ' s' },
  ]
  return (
    <div key={`${A.id}-${B.id}`} className="animate-in space-y-4 duration-300 fade-in-0 slide-in-from-bottom-2">
      <div className="flex flex-wrap items-center gap-2 text-[13px]">
        <span className="text-muted-foreground">Comparing</span>
        <VersionChip n={na} r={A} dashed />
        <ArrowRight className="size-3.5 text-muted-foreground" />
        <VersionChip n={nb} r={B} />
        {B.base === A.id && <span className="rounded border border-signal/40 px-1.5 text-[11.5px] text-signal">improved from v{na}</span>}
      </div>
      <div className="grid grid-cols-2 overflow-hidden rounded-lg border bg-card sm:grid-cols-4">
        {metrics.map((m, i) => (
          <div key={m.label} className={cn('px-4 py-3', i % 4 !== 0 && 'sm:border-l', i % 2 !== 0 && 'max-sm:border-l', i >= 4 && 'sm:border-t', i >= 2 && 'max-sm:border-t')}>
            <div className="text-[12px] text-muted-foreground">{m.label}</div>
            <div className="mt-0.5 flex items-baseline gap-2">
              <span className="text-[20px] font-semibold tabular-nums">{m.b != null ? <Rolling value={m.b} fmt={m.fmt} /> : '–'}</span>
              {m.a != null && m.b != null && <Delta value={m.b - m.a} digits={m.digits ?? 1} lowerIsBetter={m.lower} suffix={m.unit ?? ''} />}
            </div>
            <div className="text-[11.5px] text-muted-foreground">was {m.a != null ? m.fmt(m.a) : '–'} in v{na}</div>
          </div>
        ))}
      </div>
      <Parts ra={ra} rb={rb} na={na} nb={nb} />
      <Diffs A={A} B={B} ra={ra} rb={rb} na={na} nb={nb} />
    </div>
  )
}

function Rolling({ value, fmt }: { value: number; fmt: (v: number) => string }) {
  return <>{fmt(useTween(value))}</>
}

function VersionChip({ n, r, dashed }: { n: number; r: Summary; dashed?: boolean }) {
  return (
    <a href={`#/run/${r.id}`} className={cn('flex items-center gap-1.5 rounded-md border px-2 py-0.5 hover:bg-accent', dashed ? 'border-dashed' : 'border-signal/60')}>
      <StatusIcon status={r.status} />
      <b>v{n}</b>
      {r.grade && <span style={{ color: GRADE_COLOR[r.grade] }}>{Math.round(r.score!)} {r.grade}</span>}
      <span className="text-muted-foreground">{ago(r.created_at)}</span>
      {r.llm && <span className="text-[11px] text-muted-foreground">LLM</span>}
    </a>
  )
}

/** Score parts side by side: the old version as a ghost bar behind the new one. */
function Parts({ ra, rb, na, nb }: { ra?: Run; rb?: Run; na: number; nb: number }) {
  const pa = ra?.report?.score.parts ?? []
  const pb = rb?.report?.score.parts ?? []
  if (!pb.length) return null
  return (
    <section className="rounded-lg border bg-card">
      <header className="flex items-baseline gap-2 border-b px-4 py-2.5">
        <h2 className="text-[13px] font-semibold">Where the points moved</h2>
        <span className="text-[12px] text-muted-foreground">
          v{nb} solid, v{na} as the faint bar behind
        </span>
      </header>
      <div className="grid gap-x-8 gap-y-1.5 p-4 md:grid-cols-2">
        {pb.map((p) => {
          const o = pa.find((x) => x.name === p.name)
          return (
            <Tooltip key={p.name}>
              <TooltipTrigger asChild>
                <div className="grid grid-cols-[120px_1fr_92px] items-center gap-3 text-[12.5px]">
                  <span>{p.name}</span>
                  <span className="relative h-2 rounded-full bg-muted">
                    {o && <span className="absolute inset-y-0 left-0 rounded-full bg-foreground/20 transition-[width] duration-700" style={{ width: `${(o.points / o.weight) * 100}%` }} />}
                    <span className="absolute inset-y-[1px] left-0 rounded-full bg-chart-1 transition-[width] duration-700 ease-out" style={{ width: `${(p.points / p.weight) * 100}%` }} />
                  </span>
                  <span className="flex items-center justify-end gap-1.5 tabular-nums">
                    {p.points.toFixed(1)}/{p.weight}
                    {o && Math.abs(p.points - (o.points * p.weight) / o.weight) >= 0.05 && <Delta value={p.points - (o.points * p.weight) / o.weight} />}
                  </span>
                </div>
              </TooltipTrigger>
              <TooltipContent className="max-w-80">
                <p>
                  v{nb}: {p.detail}
                </p>
                {o && (
                  <p className="text-muted-foreground">
                    v{na}: {o.detail}
                  </p>
                )}
              </TooltipContent>
            </Tooltip>
          )
        })}
      </div>
    </section>
  )
}

/** What changed: the LLM-written files and the spec, as a unified diff. */
function Diffs({ A, B, ra, rb, na, nb }: { A: Summary; B: Summary; ra?: Run; rb?: Run; na: number; nb: number }) {
  const [file, setFile] = useState(OWNED[0])
  const [text, setText] = useState<Record<string, string>>({})
  useEffect(() => {
    for (const id of [A.id, B.id])
      for (const p of OWNED) {
        const k = `${id}:${p}`
        if (text[k] != null) continue
        api(`/api/runs/${encodeURIComponent(id)}/file?path=${encodeURIComponent(p)}`)
          .then((r) => r.text())
          .then((t) => setText((x) => ({ ...x, [k]: t })), () => setText((x) => ({ ...x, [k]: '' })))
      }
  }, [A.id, B.id, text])
  const before = file === 'spec' ? (ra?.spec ?? '') : (text[`${A.id}:${file}`] ?? '')
  const after = file === 'spec' ? (rb?.spec ?? '') : (text[`${B.id}:${file}`] ?? '')
  const lines = useMemo(() => diffLines(before, after), [before, after])
  const hs = useMemo(() => hunks(lines), [lines])
  const tabs = [...OWNED, 'spec']
  const counts = (f: string) => {
    const bf = f === 'spec' ? (ra?.spec ?? '') : (text[`${A.id}:${f}`] ?? '')
    const af = f === 'spec' ? (rb?.spec ?? '') : (text[`${B.id}:${f}`] ?? '')
    return changed(diffLines(bf, af))
  }
  return (
    <section className="rounded-lg border bg-card">
      <header className="flex flex-wrap items-center gap-1 border-b px-2 py-1.5">
        {tabs.map((t) => {
          const c = counts(t)
          return (
            <button
              key={t}
              onClick={() => setFile(t)}
              aria-pressed={file === t}
              className={cn('flex h-7 items-center gap-2 rounded-md px-2.5 font-mono text-[12px] transition-colors hover:bg-accent', file === t && 'bg-accent text-foreground', file !== t && 'text-muted-foreground')}
            >
              {t === 'spec' ? 'spec.yaml' : t.split('/').pop()}
              {c.added + c.removed > 0 ? (
                <span className="font-sans text-[11px]">
                  <span className="text-good">+{c.added}</span> <span className="text-destructive">−{c.removed}</span>
                </span>
              ) : (
                <span className="font-sans text-[11px] text-muted-foreground">same</span>
              )}
            </button>
          )
        })}
        <span className="ml-auto px-2 text-[12px] text-muted-foreground">
          v{na} → v{nb}
        </span>
      </header>
      <div key={file} className="max-h-[520px] animate-in overflow-auto font-mono text-[12px] leading-[1.6] duration-200 fade-in-0">
        {hs.length === 0 ? (
          <p className="p-6 text-center font-sans text-[13px] text-muted-foreground">No changes in this file between v{na} and v{nb}.</p>
        ) : (
          hs.map((h, i) => (
            <div key={i}>
              {h.skipped > 0 && <div className="border-y bg-muted/50 px-3 py-0.5 font-sans text-[11.5px] text-muted-foreground">{h.skipped} unchanged lines</div>}
              {h.lines.map((l, j) => (
                <div key={j} className={cn('grid grid-cols-[44px_44px_16px_1fr]', l.kind === '+' && 'bg-good/12', l.kind === '-' && 'bg-destructive/12')}>
                  <span className="pr-2 text-right text-muted-foreground/60 select-none">{l.a ?? ''}</span>
                  <span className="pr-2 text-right text-muted-foreground/60 select-none">{l.b ?? ''}</span>
                  <span className={cn('select-none', l.kind === '+' ? 'text-good' : l.kind === '-' ? 'text-destructive' : 'text-transparent')}>{l.kind}</span>
                  <span className="pr-4 whitespace-pre">{l.text}</span>
                </div>
              ))}
            </div>
          ))
        )}
      </div>
    </section>
  )
}
