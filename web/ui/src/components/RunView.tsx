import { useEffect, useState } from 'react'
import { Ban, CircleCheck, CircleX, Download, LoaderCircle, Square } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { api } from '@/lib/api'
import { ms } from '@/lib/format'
import { LINE } from '@/lib/stages'
import { cn } from '@/lib/utils'
import type { Run, Stage, Validation } from '@/lib/types'
import { Conveyor, StationDetail, remaining } from './Conveyor'
import { Blueprint } from './Blueprint'
import { Measurements } from './Measurements'
import { CodeView, ContractView, LogView } from './Views'
import { ErrorBoundary } from './ErrorBoundary'

const PILL = {
  running: { word: 'Running', icon: LoaderCircle, cls: 'bg-signal/15 text-signal border-signal/30', spin: true },
  passed: { word: 'All gates passed', icon: CircleCheck, cls: 'text-good border-good/30 bg-good/10', spin: false },
  failed: { word: 'Stopped at a gate', icon: CircleX, cls: 'text-destructive border-destructive/30 bg-destructive/10', spin: false },
  canceled: { word: 'Stopped', icon: Ban, cls: 'text-muted-foreground', spin: false },
} as const

const dur = (ms: number) => {
  const s = Math.round(ms / 1000)
  return s < 60 ? `${s} s` : `${Math.floor(s / 60)} min ${String(s % 60).padStart(2, '0')} s`
}

/** A running line's clock: stations done, time so far, a rough estimate of what is left. */
function LineClock({ stages, started }: { stages: Stage[]; started: string }) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 250)
    return () => clearInterval(t)
  }, [])
  const done = stages.filter((s) => s.status !== 'pending' && s.status !== 'running').length
  const left = remaining(stages, now)
  return (
    <span className="text-sm text-muted-foreground tabular-nums">
      Station {Math.min(done + 1, stages.length)} of {stages.length}, {dur(now - new Date(started).getTime())} so far
      {left > 1000 && <>, about {dur(left)} left</>}
    </span>
  )
}

export function RunView({ run, error, validation }: { run: Run | null; error: string | null; validation: Validation | null }) {
  const [tab, setTab] = useState('service')
  const [selected, setSelected] = useState<string | undefined>()
  const [open, setOpen] = useState<{ path: string; line?: number } | null>(null)
  const [stopping, setStopping] = useState(false)
  const stages = run?.stages ?? LINE
  const current =
    stages.find((s) => s.id === selected) ??
    stages.find((s) => s.status === 'running') ??
    stages.find((s) => s.status === 'failed') ??
    [...stages].reverse().find((s) => s.status !== 'pending' && s.status !== 'skipped') ??
    null

  const openFile = (path: string, line?: number) => {
    setOpen({ path, line })
    setTab('code')
  }

  const pill = run ? PILL[run.status as keyof typeof PILL] : undefined
  const total = run?.stages.reduce((a, s) => a + (s.duration_ms || 0), 0) ?? 0
  const id = run ? encodeURIComponent(run.id) : ''

  return (
    <section className="min-w-0 space-y-4 p-5">
        <div className="flex flex-wrap items-center gap-3">
          {run ? (
            <>
              <h2 className="text-[17px] font-semibold tracking-[-0.015em]">{run.name}</h2>
              {pill && (
                <Badge variant="outline" className={cn('gap-1', pill.cls)}>
                  <pill.icon className={cn('size-3.5', pill.spin && 'animate-spin')} />
                  {pill.word}
                </Badge>
              )}
              {run.status === 'running' ? (
                <LineClock stages={run.stages} started={run.created_at} />
              ) : (
                <span className="text-sm text-muted-foreground">
                  Started {new Date(run.created_at).toLocaleTimeString()}, {ms(total)} on the line{run.options?.llm ? ', LLM on' : ''}
                </span>
              )}
              <div className="ml-auto flex flex-wrap gap-2">
                {run.status === 'running' ? (
                  <Button
                    variant="outline"
                    size="sm"
                    className="border-destructive/50 text-destructive hover:bg-destructive/10"
                    disabled={stopping}
                    onClick={async () => {
                      setStopping(true)
                      try {
                        await api(`/api/runs/${id}/cancel`, { method: 'POST' })
                      } finally {
                        setStopping(false)
                      }
                    }}
                  >
                    <Square /> {stopping ? 'Stopping…' : 'Stop the run'}
                  </Button>
                ) : (
                  <>
                    <Button variant="outline" size="sm" asChild>
                      <a href={`/api/runs/${id}/zip`}>
                        <Download /> Download .zip
                      </a>
                    </Button>
                    {run.report?.supply && (
                      <>
                        <Button variant="outline" size="sm" asChild>
                          <a href={`/api/runs/${id}/sbom`}>SBOM</a>
                        </Button>
                        <Button variant="outline" size="sm" asChild>
                          <a href={`/api/runs/${id}/provenance`}>Provenance</a>
                        </Button>
                      </>
                    )}
                  </>
                )}
              </div>
            </>
          ) : (
            <>
              <h2 className="text-[17px] font-semibold tracking-[-0.015em]">{error ? 'Run not found' : 'No run yet'}</h2>
              <span className="text-sm text-muted-foreground">{error ?? 'Pick an example or write a spec, then run the line.'}</span>
            </>
          )}
        </div>
        <Conveyor stages={stages} running={run?.status === 'running'} runStatus={run?.status} grade={run?.report?.score.grade} selected={current?.id} onSelect={setSelected} />
        <StationDetail stage={current} />
        <Tabs value={tab} onValueChange={setTab}>
          <TabsList>
            <TabsTrigger value="service">Service</TabsTrigger>
            <TabsTrigger value="measure">Measurements</TabsTrigger>
            <TabsTrigger value="code">Code</TabsTrigger>
            <TabsTrigger value="log">Log</TabsTrigger>
            <TabsTrigger value="contract">Contract</TabsTrigger>
          </TabsList>
          <TabsContent value="service" className="pt-2">
            <ErrorBoundary label="service blueprint">
              <Blueprint run={run} validation={validation} open={openFile} />
            </ErrorBoundary>
          </TabsContent>
          <TabsContent value="measure" className="pt-2">
            {run ? (
              <ErrorBoundary label="measurements" key={run.id + run.status}>
                <Measurements run={run} open={openFile} />
              </ErrorBoundary>
            ) : (
              <p className="py-16 text-center text-sm text-muted-foreground">
                Run the line and every station's output is measured here: mutation score, spec examples, complexity, coverage, findings, runtime and a quality score.
              </p>
            )}
          </TabsContent>
          <TabsContent value="code" className="pt-2">
            {run ? <CodeView run={run} open={open} /> : <p className="py-16 text-center text-sm text-muted-foreground">Run the line to generate a service, then browse every file here with its origin.</p>}
          </TabsContent>
          <TabsContent value="log" className="pt-2">
            <LogView stages={stages} selected={current?.id} onSelect={setSelected} />
          </TabsContent>
          <TabsContent value="contract" className="pt-2">
            <ContractView v={validation} />
          </TabsContent>
        </Tabs>
    </section>
  )
}
