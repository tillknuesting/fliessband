import { useEffect, useMemo, useRef, useState } from 'react'
import { Ban, Check, ChevronDown, CircleCheck, CircleX, Database, LoaderCircle, Rocket, Trash2, Undo2, X } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { removeService, startDeploy, useDeployment, useDeployState } from '@/lib/deploy'
import { cn } from '@/lib/utils'
import type { DeployState, DeployStatus, DeployStep, DeploySummary, Run, Summary, TargetState, Workload } from '@/lib/types'
import { versionsOf } from './Product'
import { ago } from './Shell'

const secs = (ms: number) => (ms < 1000 ? `${Math.round(ms)} ms` : ms < 60_000 ? `${(ms / 1000).toFixed(1)} s` : `${Math.floor(ms / 60_000)} min ${Math.round((ms % 60_000) / 1000)} s`)

export const DEPLOY_WORD: Record<DeployStatus, { word: string; cls: string; icon: typeof Check }> = {
  running: { word: 'Deploying', cls: 'text-signal', icon: LoaderCircle },
  live: { word: 'Live', cls: 'text-good', icon: CircleCheck },
  failed: { word: 'Failed', cls: 'text-destructive', icon: CircleX },
  rolled_back: { word: 'Rolled back', cls: 'text-warn', icon: Undo2 },
  replaced: { word: 'Replaced', cls: 'text-muted-foreground', icon: Check },
  removed: { word: 'Removed', cls: 'text-muted-foreground', icon: Ban },
}

const STRATEGY: Record<TargetState['strategy'], string> = {
  k3d: 'k3d image import',
  kind: 'kind load',
  minikube: 'minikube image load',
  shared: 'shared image store',
  registry: 'registry push',
}

/** One capsule per replica: filled when it serves, a breathing ring while it starts. */
export function PodDots({ w, className }: { w: Pick<Workload, 'ready' | 'desired' | 'updated'>; className?: string }) {
  const n = Math.max(w.desired, w.ready, 1)
  return (
    <span className={cn('flex items-center gap-1', className)} aria-label={`${w.ready} of ${w.desired} replicas ready`}>
      {Array.from({ length: n }, (_, i) => {
        const ready = i < w.ready
        const extra = i >= w.desired
        return <span key={i} className={cn('pod', ready ? 'pod-ready' : 'pod-wait', extra && 'opacity-40')} />
      })}
    </span>
  )
}

/** The steps of a deployment on a short track; a puck rides to the step at work. */
export function StepTrack({ steps, status }: { steps: DeployStep[]; status: DeployStatus }) {
  const at = steps.findIndex((s) => s.status === 'running' || s.status === 'failed')
  const last = steps.reduce((a, s, i) => (s.status === 'passed' ? i : a), -1)
  const pos = at >= 0 ? at : status === 'running' ? last + 1 : steps.length - 1
  const pct = steps.length > 1 ? (Math.min(pos, steps.length - 1) / (steps.length - 1)) * 100 : 0
  const done = status === 'live'
  return (
    <div className="relative px-3 pt-2 pb-1">
      <div className="absolute top-[17px] right-[calc(100%/14)] left-[calc(100%/14)] h-px bg-border">
        <div className={cn('h-px transition-[width] duration-700 ease-out', status === 'rolled_back' ? 'bg-warn' : status === 'failed' ? 'bg-destructive' : 'bg-signal')} style={{ width: `${pct}%` }} />
        {status === 'running' && <span className="deploy-puck" style={{ left: `${pct}%` }} />}
      </div>
      <ol className="relative grid" style={{ gridTemplateColumns: `repeat(${steps.length}, minmax(0, 1fr))` }}>
        {steps.map((s) => (
          <li key={s.id} className="flex min-w-0 flex-col items-center gap-1.5 text-center" title={s.summary}>
            <span
              className={cn(
                'grid size-[18px] place-items-center rounded-full border bg-card text-[10px]',
                s.status === 'passed' && 'border-signal bg-signal text-signal-foreground',
                s.status === 'running' && 'border-signal text-signal',
                s.status === 'failed' && (status === 'rolled_back' ? 'border-warn bg-warn text-background' : 'border-destructive bg-destructive text-white'),
                s.status === 'skipped' && 'border-dashed text-muted-foreground',
                done && s.status === 'passed' && 'shadow-[0_0_10px_-2px_var(--signal)]',
              )}
            >
              {s.status === 'passed' ? <Check className="size-3" strokeWidth={3} /> : s.status === 'running' ? <LoaderCircle className="size-3 animate-spin" /> : s.status === 'failed' ? <X className="size-3" strokeWidth={3} /> : null}
            </span>
            <span className={cn('truncate text-[11.5px]', s.status === 'pending' || s.status === 'skipped' ? 'text-muted-foreground' : 'text-foreground')}>{s.name}</span>
            <span className="h-3.5 text-[10.5px] text-muted-foreground tabular-nums">{s.duration_ms > 0 && s.status !== 'running' ? secs(s.duration_ms) : ''}</span>
          </li>
        ))}
      </ol>
    </div>
  )
}

/** A deployment as it happens: its steps, what the current one says, and its log. */
export function DeployProgress({ id, version, onClose }: { id: string; version?: number; onClose?: () => void }) {
  const dep = useDeployment(id)
  const [view, setView] = useState<'none' | 'log' | 'manifests'>('none')
  const logRef = useRef<HTMLPreElement>(null)
  useEffect(() => {
    if (view === 'log' && logRef.current) logRef.current.scrollTop = logRef.current.scrollHeight
  }, [dep?.log.length, view])
  if (!dep) return <div className="h-[132px] animate-pulse rounded-lg border bg-card" />
  const w = DEPLOY_WORD[dep.status]
  const current = dep.steps.find((s) => s.status === 'running' || s.status === 'failed') ?? [...dep.steps].reverse().find((s) => s.status === 'passed')
  const total = dep.finished_at ? new Date(dep.finished_at).getTime() - new Date(dep.created_at).getTime() : 0
  return (
    <section
      className={cn(
        'overflow-hidden rounded-lg border bg-card',
        dep.status === 'live' && 'border-good/40',
        dep.status === 'failed' && 'border-destructive/40',
        dep.status === 'rolled_back' && 'border-warn/50',
        dep.status === 'running' && 'border-signal/40',
      )}
    >
      <header className="flex flex-wrap items-center gap-x-2 gap-y-1 border-b px-4 py-2.5 text-[13px]">
        <w.icon className={cn('size-4', w.cls, dep.status === 'running' && 'animate-spin')} />
        <span className="font-semibold">
          {dep.action === 'remove' ? `${{ running: 'Removing', removed: 'Removed', failed: 'Could not remove' }[dep.status as string] ?? 'Removed'} ${dep.service}` : `${w.word === 'Deploying' ? 'Deploying' : w.word}: ${dep.service}${version ? ` v${version}` : ''}`}
        </span>
        <span className="text-muted-foreground">
          to <span className="font-mono text-[12px] text-foreground">{dep.target}</span>
          <span className="font-mono text-[12px]">/{dep.namespace}</span>
        </span>
        <span className="ml-auto flex items-center gap-1 text-[12px] text-muted-foreground tabular-nums">
          {total > 0 ? `took ${secs(total)}` : ago(dep.created_at)}
          {onClose && dep.status !== 'running' && (
            <button onClick={onClose} aria-label="Close" className="ml-1 grid size-6 place-items-center rounded hover:bg-accent hover:text-foreground">
              <X className="size-3.5" />
            </button>
          )}
        </span>
      </header>
      {dep.action === 'deploy' && <StepTrack steps={dep.steps} status={dep.status} />}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-t px-4 py-2 text-[12.5px]">
        {dep.status === 'live' ? (
          <span>
            Serving with {dep.replicas ?? '?'} replicas ready
            {dep.health && <>, health <span className="font-mono text-[11.5px] text-good">{dep.health}</span></>}
            {dep.previous && <span className="text-muted-foreground">. Replaced {dep.previous.slice(-4)}</span>}
          </span>
        ) : dep.status === 'running' ? (
          <span className="text-muted-foreground">
            <span className="text-foreground">{current?.name}</span>
            {current?.summary ? `: ${current.summary}` : '…'}
          </span>
        ) : (
          <span className={cn(dep.status === 'failed' && 'text-destructive', dep.status === 'rolled_back' && 'text-warn')}>{dep.error || current?.summary}</span>
        )}
        <span className="ml-auto flex gap-1">
          {(['log', 'manifests'] as const).map((v) =>
            v === 'manifests' && !dep.manifests ? null : (
              <button
                key={v}
                onClick={() => setView(view === v ? 'none' : v)}
                aria-pressed={view === v}
                className="rounded px-1.5 py-0.5 text-[12px] text-muted-foreground hover:bg-accent hover:text-foreground aria-pressed:bg-accent aria-pressed:text-foreground"
              >
                {v === 'log' ? `Log (${dep.log.length})` : 'Manifests'}
              </button>
            ),
          )}
        </span>
      </div>
      {view !== 'none' && (
        <pre ref={logRef} className="max-h-72 overflow-auto border-t bg-muted/40 px-4 py-2 font-mono text-[11.5px] leading-[1.55] whitespace-pre-wrap text-muted-foreground">
          {view === 'log'
            ? dep.log.map((l, i) => (
                <div key={i} className={cn(l.startsWith('$ ') && 'text-foreground', /failed|error|BackOff/i.test(l) && !l.startsWith('$ ') && 'text-destructive')}>
                  {l}
                </div>
              ))
            : dep.manifests}
        </pre>
      )}
    </section>
  )
}

/** Deploy a passed run: one button per target, or a short menu when there are several. */
export function DeployButton({ run, state, onStarted }: { run: Run; state: DeployState | null; onStarted: (id: string) => void }) {
  const [open, setOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    if (!open) return
    const close = (e: MouseEvent) => !ref.current?.contains(e.target as Node) && setOpen(false)
    window.addEventListener('mousedown', close)
    return () => window.removeEventListener('mousedown', close)
  }, [open])
  if (!state?.enabled) return null
  const targets = state.targets
  const passed = run.status === 'passed'
  const go = async (target: string) => {
    setOpen(false)
    setBusy(true)
    setErr(null)
    try {
      onStarted((await startDeploy(run.id, target)).id)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }
  const title = !targets.length
    ? 'No cluster to deploy to: create one with k3d cluster create fliessband, or set FLIESSBAND_DEPLOY_TARGETS'
    : !passed
      ? 'Only a run that passed every gate can be deployed'
      : targets.length === 1
        ? `Build the image and roll it out to ${targets[0].context}/${targets[0].namespace}`
        : 'Pick a cluster to deploy to'
  return (
    <div ref={ref} className="relative">
      <Button
        variant="outline"
        size="sm"
        disabled={!targets.length || !passed || busy}
        title={err ?? title}
        className={cn(err && 'border-destructive/50 text-destructive')}
        onClick={() => (targets.length === 1 ? go(targets[0].name) : setOpen(!open))}
      >
        <Rocket className={cn(busy && 'animate-pulse')} /> Deploy
        {targets.length > 1 && <ChevronDown className="opacity-60" />}
      </Button>
      {open && (
        <div role="menu" className="absolute right-0 z-20 mt-1 w-72 overflow-hidden rounded-md border bg-popover p-1 text-popover-foreground shadow-md">
          {targets.map((t) => {
            const w = t.workloads.find((x) => x.service === run.name)
            return (
              <button key={t.name} role="menuitem" onClick={() => go(t.name)} disabled={!t.reachable} className="flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[13px] hover:bg-accent disabled:opacity-50">
                <span className={cn('size-1.5 rounded-full', t.reachable ? 'bg-good' : 'bg-destructive')} />
                <span className="min-w-0 flex-1">
                  <span className="block truncate">{t.name}</span>
                  <span className="block truncate text-[11.5px] text-muted-foreground">
                    {t.namespace}
                    {w ? `, runs ${w.run.slice(-4)} now` : ''}
                  </span>
                </span>
              </button>
            )
          })}
        </div>
      )}
    </div>
  )
}

/** Where a run is live, if anywhere. */
export function liveTargets(state: DeployState | null, run: Run | Summary) {
  // The cluster reports a version as soon as it is applied; it is live once its deployment says so.
  const live = new Set((state?.deployments ?? []).filter((d) => d.run === run.id && d.status === 'live').map((d) => d.target))
  return (state?.targets ?? []).filter((t) => live.has(t.name) && t.workloads.some((w) => w.service === run.name && w.run === run.id))
}

/**
 * The clusters the factory ships to: what runs on each, replica by replica,
 * with rollback and removal, and the deployments that put it there.
 */
export function Deployments({ runs }: { runs: Summary[] }) {
  const state = useDeployState()
  const [open, setOpen] = useState<string | null>(null)
  const vnum = useMemo(() => {
    const m = new Map<string, number>()
    for (const name of new Set(runs.map((r) => r.name))) versionsOf(runs, name).forEach((v, i) => m.set(v.id, i + 1))
    return m
  }, [runs])
  const running = state?.deployments.find((d) => d.status === 'running')
  useEffect(() => {
    if (running && !open) setOpen(running.id)
  }, [running, open])

  if (!state) return <div className="m-5 h-40 animate-pulse rounded-lg border bg-card" />
  if (!state.enabled) return <Empty title="Deployment is off" body="The factory was started with FLIESSBAND_DEPLOY=off." />
  if (!state.targets.length)
    return (
      <Empty
        title="No cluster to deploy to"
        body={
          <>
            Fliessband deploys to local clusters it finds in your kubeconfig (k3d, kind, minikube, Docker Desktop, OrbStack, Colima, Rancher Desktop). Create one and restart the factory:
            <code className="mt-3 block rounded-md border bg-muted/50 px-3 py-2 text-left font-mono text-[12px] text-foreground">k3d cluster create fliessband</code>
            <span className="mt-3 block">
              For any other cluster, name it with a registry its nodes pull from: <code className="font-mono text-[12px]">FLIESSBAND_DEPLOY_TARGETS=staging=my-context/shop@ghcr.io/acme</code>
            </span>
          </>
        }
      />
    )
  const deps = state.deployments
  return (
    <div className="grid items-start gap-4 p-5 xl:grid-cols-[minmax(0,1fr)_420px]">
      <div className="space-y-4">
        {state.targets.map((t) => (
          <TargetCard key={t.name} t={t} deps={deps} vnum={vnum} onOpen={setOpen} />
        ))}
      </div>
      <div className="space-y-4">
        {open && <DeployProgress key={open} id={open} version={vnum.get(deps.find((d) => d.id === open)?.run ?? '')} onClose={() => setOpen(null)} />}
        <section className="rounded-lg border bg-card">
          <header className="flex items-baseline gap-2 border-b px-4 py-2.5">
            <h2 className="text-[13px] font-semibold">Deployments</h2>
            <span className="text-[12px] text-muted-foreground">{deps.length ? `${deps.filter((d) => d.status === 'live').length} live` : 'none yet'}</span>
          </header>
          {deps.length === 0 ? (
            <p className="px-4 py-8 text-center text-[13px] text-muted-foreground">Open a run that passed every gate and press Deploy.</p>
          ) : (
            <ul className="max-h-[60vh] divide-y overflow-y-auto">
              {deps.map((d) => (
                <HistoryRow key={d.id} d={d} v={vnum.get(d.run)} active={open === d.id} onOpen={() => setOpen(d.id)} />
              ))}
            </ul>
          )}
        </section>
      </div>
    </div>
  )
}

function Empty({ title, body }: { title: string; body: React.ReactNode }) {
  return (
    <div className="mx-auto max-w-xl p-10 text-center">
      <Rocket className="mx-auto size-6 text-signal" />
      <h2 className="mt-3 text-[15px] font-semibold">{title}</h2>
      <div className="mt-2 text-[13px] leading-relaxed text-muted-foreground">{body}</div>
    </div>
  )
}

function HistoryRow({ d, v, active, onOpen }: { d: DeploySummary; v?: number; active: boolean; onOpen: () => void }) {
  const w = DEPLOY_WORD[d.status]
  const took = d.finished_at ? new Date(d.finished_at).getTime() - new Date(d.created_at).getTime() : 0
  return (
    <li>
      <button onClick={onOpen} aria-current={active || undefined} className="flex w-full items-start gap-2.5 px-4 py-2 text-left hover:bg-accent/60 aria-[current]:bg-accent">
        <w.icon className={cn('mt-0.5 size-3.5 shrink-0', w.cls, d.status === 'running' && 'animate-spin')} />
        <span className="min-w-0 flex-1">
          <span className="flex items-baseline gap-1.5 text-[13px]">
            <span className="font-medium">{d.action === 'remove' ? `Removed ${d.service}` : `${d.service}${v ? ` v${v}` : ''}`}</span>
            <span className="truncate text-[12px] text-muted-foreground">to {d.target}</span>
          </span>
          <span className={cn('block truncate text-[12px]', d.status === 'failed' ? 'text-destructive' : d.status === 'rolled_back' ? 'text-warn' : 'text-muted-foreground')}>
            {d.status === 'running' ? `${d.step ?? 'Starting'}…` : d.error || w.word}
          </span>
        </span>
        <span className="shrink-0 text-right text-[11.5px] text-muted-foreground tabular-nums">
          {ago(d.created_at)}
          {took > 0 && <span className="block">{secs(took)}</span>}
        </span>
      </button>
    </li>
  )
}

function TargetCard({ t, deps, vnum, onOpen }: { t: TargetState; deps: DeploySummary[]; vnum: Map<string, number>; onOpen: (id: string) => void }) {
  const mine = deps.filter((d) => d.target === t.name)
  const ghost = mine.filter((d) => d.status === 'running' && d.action === 'deploy' && !t.workloads.some((w) => w.service === d.service))
  return (
    <section className="rounded-lg border bg-card">
      <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1 border-b px-4 py-2.5">
        <span className={cn('size-2 rounded-full', t.reachable ? 'bg-good shadow-[0_0_8px_-1px_var(--good)]' : 'bg-destructive')} aria-label={t.reachable ? 'reachable' : 'unreachable'} />
        <h2 className="text-[13px] font-semibold">{t.name}</h2>
        <span className="font-mono text-[12px] text-muted-foreground">
          {t.context}/{t.namespace}
        </span>
        <span className="ml-auto flex items-center gap-2 text-[12px] text-muted-foreground">
          {t.server && <span>Kubernetes {t.server.replace(/^v/, '').replace(/\+.*$/, '')}</span>}
          <span className="rounded border px-1.5 py-px">{STRATEGY[t.strategy]}</span>
          {!t.local && <span className="rounded border border-warn/50 px-1.5 py-px text-warn">remote</span>}
        </span>
      </header>
      {!t.reachable ? (
        <p className="px-4 py-6 text-[13px] text-destructive">{t.error}. Is the cluster running?</p>
      ) : t.workloads.length === 0 && ghost.length === 0 ? (
        <p className="px-4 py-6 text-center text-[13px] text-muted-foreground">Nothing deployed here yet.</p>
      ) : (
        <ul className="divide-y">
          {t.workloads.map((w) => (
            <WorkloadRow key={w.service} t={t} w={w} deps={mine.filter((d) => d.service === w.service)} vnum={vnum} onOpen={onOpen} />
          ))}
          {ghost.map((d) => (
            <li key={d.id} className="flex items-center gap-3 px-4 py-3 text-[13px] text-muted-foreground">
              <LoaderCircle className="size-3.5 animate-spin text-signal" /> {d.service} v{vnum.get(d.run)}: {d.step ?? 'starting'}…
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function WorkloadRow({ t, w, deps, vnum, onOpen }: { t: TargetState; w: Workload; deps: DeploySummary[]; vnum: Map<string, number>; onOpen: (id: string) => void }) {
  const [confirm, setConfirm] = useState(false)
  const [busy, setBusy] = useState<string | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const v = vnum.get(w.run)
  const deploying = deps.find((d) => d.status === 'running')
  // The version to go back to: the newest one that was live here before this one.
  const back = deps.find((d) => d.action === 'deploy' && d.status === 'replaced' && d.run !== w.run)
  const settling = w.ready < w.desired || w.updated < w.desired
  const act = async (what: string, fn: () => Promise<{ id: string }>) => {
    setBusy(what)
    setErr(null)
    try {
      onOpen((await fn()).id)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(null)
      setConfirm(false)
    }
  }
  return (
    <li className="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-1 px-4 py-3">
      <div className="flex min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1">
        <a href={`#/product/${encodeURIComponent(w.service)}`} className="text-[13.5px] font-medium hover:underline">
          {w.service}
        </a>
        <a href={`#/run/${w.run}`} className="rounded-md border px-1.5 py-px text-[12px] text-muted-foreground hover:bg-accent hover:text-foreground" title={`Run ${w.run}`}>
          {v ? `v${v}` : w.run.slice(-4)}
        </a>
        <PodDots w={w} />
        <span className={cn('text-[12px] tabular-nums', settling ? 'text-signal' : 'text-muted-foreground')}>
          {w.ready}/{w.desired} ready
        </span>
        {w.database && (
          <span className="flex items-center gap-1 rounded border px-1.5 py-px text-[11.5px] text-muted-foreground" title="A throwaway PostgreSQL runs next to it; its data is lost when its pod goes">
            <Database className="size-3" /> PostgreSQL
          </span>
        )}
        {deploying && (
          <button onClick={() => onOpen(deploying.id)} className="flex items-center gap-1 text-[12px] text-signal hover:underline">
            <LoaderCircle className="size-3 animate-spin" /> v{vnum.get(deploying.run)} {deploying.step?.toLowerCase() ?? 'starting'}
          </button>
        )}
        <span className="basis-full truncate font-mono text-[11px] text-muted-foreground">{w.image}</span>
        {err && <span className="basis-full text-[12px] text-destructive">{err}</span>}
      </div>
      <div className="flex gap-1.5">
        {back && (
          <Button variant="outline" size="sm" disabled={!!busy || !!deploying} onClick={() => act('back', () => startDeploy(back.run, t.name))} title={`Deploy v${vnum.get(back.run) ?? back.run.slice(-4)} again, the version that was live here before`}>
            <Undo2 className={cn(busy === 'back' && 'animate-pulse')} /> Roll back to v{vnum.get(back.run) ?? '?'}
          </Button>
        )}
        <Button
          variant="outline"
          size="sm"
          disabled={!!busy || !!deploying}
          onBlur={() => setConfirm(false)}
          className={cn(confirm && 'border-destructive/60 text-destructive hover:bg-destructive/10')}
          onClick={() => (confirm ? act('remove', () => removeService(t.name, w.service)) : setConfirm(true))}
          title="Delete the service's deployment, service, disruption budget and the factory's database from the cluster"
        >
          <Trash2 /> {confirm ? 'Click again to remove' : 'Remove'}
        </Button>
      </div>
    </li>
  )
}
