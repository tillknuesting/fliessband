import { useState } from 'react'
import { Bar, BarChart, CartesianGrid, LabelList, XAxis } from 'recharts'
import { CircleAlert, CircleCheck, CircleX, Download, Trophy } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { Checkbox } from '@/components/ui/checkbox'
import { Label } from '@/components/ui/label'
import { Progress } from '@/components/ui/progress'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { mb, ms, num, num1, pct } from '@/lib/format'
import { cn } from '@/lib/utils'
import type { Bucket, Candidate, Func, PassCount, Report, Run } from '@/lib/types'
import { Empty, Origin } from './Views'

type Open = (path: string, line?: number) => void

function Section({ title, description, children, className }: { title: string; description?: React.ReactNode; children: React.ReactNode; className?: string }) {
  return (
    <Card className={cn('gap-4', className)}>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        {description && <CardDescription>{description}</CardDescription>}
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  )
}

function Tile({ label, value, note, tone }: { label: string; value: string; note: string; tone?: 'good' | 'bad' }) {
  return (
    <div className="rounded-lg border bg-card p-3">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-0.5 text-2xl font-semibold tracking-tight tabular-nums">{value}</div>
      <div className={cn('mt-0.5 text-xs text-muted-foreground', tone === 'good' && 'text-good', tone === 'bad' && 'text-destructive')}>{note}</div>
    </div>
  )
}

function Meter({ label, value, sub, title }: { label: React.ReactNode; value: number | undefined; sub: string; title: string }) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <div className="grid grid-cols-[minmax(0,1fr)_minmax(80px,1.2fr)_52px] items-center gap-3 py-1 text-sm">
          <span className="truncate font-mono text-xs">{label}</span>
          <Progress value={value ?? 0} className="h-2" />
          <span className="text-right text-xs tabular-nums">{sub}</span>
        </div>
      </TooltipTrigger>
      <TooltipContent>{title}</TooltipContent>
    </Tooltip>
  )
}

const histConfig = { count: { label: 'Functions', color: 'var(--chart-1)' } } satisfies ChartConfig

function Histogram({ title, description, data }: { title: string; description: string; data: Bucket[] }) {
  return (
    <Section title={title} description={description}>
      <ChartContainer config={histConfig} className="aspect-auto h-44 w-full">
        <BarChart data={data} margin={{ top: 18, left: 4, right: 4 }} accessibilityLayer>
          <CartesianGrid vertical={false} />
          <XAxis dataKey="label" tickLine={false} axisLine={false} tickMargin={6} fontSize={11} />
          <ChartTooltip cursor={{ fill: 'var(--muted)' }} content={<ChartTooltipContent hideIndicator />} />
          <Bar isAnimationActive={false} dataKey="count" fill="var(--color-count)" radius={[4, 4, 0, 0]} maxBarSize={28}>
            <LabelList dataKey="count" position="top" fontSize={11} className="fill-muted-foreground" />
          </Bar>
        </BarChart>
      </ChartContainer>
    </Section>
  )
}

function ScoreCard({ rep }: { rep: Report }) {
  const sc = rep.score
  return (
    <Card>
      <CardContent className="grid items-center gap-6 md:grid-cols-[220px_1fr]">
        <div>
          <div className="text-sm text-muted-foreground">Quality score</div>
          <div className="mt-1 flex items-end gap-3">
            <span className="text-6xl leading-none font-bold tracking-tight tabular-nums">{Math.round(sc.total)}</span>
            <span className="pb-1 text-sm text-muted-foreground">of 100</span>
            <span aria-label={`grade ${sc.grade}`} className="grid size-11 -rotate-3 place-items-center rounded-md border-2 border-foreground text-2xl font-bold">
              {sc.grade}
            </span>
          </div>
          {sc.capped && <p className="mt-2 text-xs text-destructive">Capped at 40: a gate failed.</p>}
        </div>
        <div className="space-y-1">
          {(sc.parts ?? []).map((p) => {
            const measured = p.measured !== false
            return (
              <Tooltip key={p.name}>
                <TooltipTrigger asChild>
                  <div className={cn('grid grid-cols-[130px_1fr_64px] items-center gap-3 text-sm', !measured && 'opacity-50')}>
                    <span>{p.name}</span>
                    <Progress value={measured ? (p.points / p.weight) * 100 : 0} className="h-2" />
                    <span className="text-right text-xs tabular-nums">
                      {measured ? <b className="text-sm">{num1(p.points)}</b> : '–'} / {p.weight}
                    </span>
                  </div>
                </TooltipTrigger>
                <TooltipContent className="max-w-80">
                  {p.detail}
                  {!measured && <p>Left out of the total: the score is scaled over what was measured.</p>}
                </TooltipContent>
              </Tooltip>
            )
          })}
        </div>
      </CardContent>
    </Card>
  )
}

const STATUS_BADGE: Record<string, string> = {
  passed: 'border-green-300 bg-green-50 text-green-800 dark:bg-green-950 dark:text-green-200',
  failed: 'border-red-300 bg-red-50 text-red-800 dark:bg-red-950 dark:text-red-200',
  pending: 'text-muted-foreground',
}

function Acceptance({ acc }: { acc: Report['acceptance'] }) {
  const description = 'Executable examples from the spec, run as tests the implementer cannot change. Holdout examples are also hidden from the LLM, so they show whether the code generalises.'
  if (!acc)
    return (
      <Section title="Spec examples" description={description}>
        <p className="text-sm text-muted-foreground">
          The spec has no examples. Add <code className="font-mono">examples:</code> to a method to pin its behaviour down.
        </p>
      </Section>
    )
  return (
    <Section title="Spec examples" description={description}>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Method</TableHead>
            <TableHead>Example</TableHead>
            <TableHead>Result</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(acc.examples ?? []).map((e, i) => (
            <TableRow key={i}>
              <TableCell className="font-mono text-xs">{e.method}</TableCell>
              <TableCell className="whitespace-normal">
                {e.name}{' '}
                {e.holdout && (
                  <Badge variant="outline" className="h-4 border-violet-300 bg-violet-50 px-1.5 text-[10.5px] text-violet-800 dark:bg-violet-950 dark:text-violet-200">
                    holdout
                  </Badge>
                )}
                {e.message && <div className="text-xs text-muted-foreground">{e.message.split('\n')[0]}</div>}
              </TableCell>
              <TableCell>
                <Badge variant="outline" className={STATUS_BADGE[e.status]}>
                  {e.status}
                </Badge>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Section>
  )
}

function Mutation({ rep, open }: { rep: Report; open: Open }) {
  const mut = rep.mutation
  const hard = rep.factory.hardening
  const description = 'Small faults injected into the code, one at a time. A caught fault means some test checks that behaviour; a surviving one marks behaviour no test checks.'
  if (!mut)
    return (
      <Section title="Mutation testing" description={description}>
        <p className="text-sm text-muted-foreground">Not run for this run.</p>
      </Section>
    )
  const survivors = mut.survivors ?? []
  return (
    <Section title="Mutation testing" description={`${description} ${mut.not_covered} on lines no test runs${mut.sampled ? '; a sample spread over files and operators' : ''}.`}>
      <div>
        {(mut.by_file ?? [])
          .filter((f) => f.total)
          .map((f) => (
            <Meter
              key={f.file}
              label={
                <span className="flex items-center gap-1.5">
                  {f.file.replace('internal/', '')} {f.origin === 'llm' && <Origin origin="llm" />}
                </span>
              }
              value={(f.score ?? 0) * 100}
              sub={f.score == null ? '–' : pct(f.score * 100)}
              title={`${f.file}: ${f.killed} of ${f.total} faults caught`}
            />
          ))}
      </div>
      {hard && (
        <p className="mt-3 text-sm">
          LLM hardening: {hard.kept ? `new tests killed ${hard.killed} of ${hard.survivors} survivors in its code` : (hard.note ?? 'no tests kept')}
          {hard.equivalent ? `, ${hard.equivalent} judged equivalent` : ''}.
        </p>
      )}
      <h4 className="mt-5 mb-2 text-sm font-semibold">Surviving mutants</h4>
      {survivors.length === 0 ? (
        <p className="flex items-center gap-1.5 text-sm text-good">
          <CircleCheck className="size-4" /> No surviving mutants.
        </p>
      ) : (
        <ul className="space-y-2">
          {survivors.slice(0, 12).map((m) => (
            <li key={m.id} className="rounded-md bg-muted p-2.5 text-sm">
              <div className="text-xs text-muted-foreground">
                <button className="font-mono text-primary underline-offset-2 hover:underline" onClick={() => open(m.file, m.line)}>
                  {m.file}:{m.line}
                </button>
                , {m.operator}
              </div>
              <code className="block font-mono text-xs break-all whitespace-pre-wrap text-muted-foreground">{m.before}</code>
              <code className="block font-mono text-xs break-all whitespace-pre-wrap text-destructive">→ {m.after}</code>
            </li>
          ))}
          {survivors.length > 12 && <p className="text-xs text-muted-foreground">{survivors.length - 12} more in the Log tab.</p>}
        </ul>
      )}
    </Section>
  )
}

const KINDS: [string, string, string][] = [
  ['code', 'Service code', 'var(--chart-1)'],
  ['test', 'Tests', 'var(--chart-2)'],
  ['generated', 'Generated by buf', 'var(--chart-3)'],
  ['proto', 'Proto', 'var(--chart-4)'],
  ['config', 'Config and docs', 'var(--chart-5)'],
]

function Size({ rep }: { rep: Report }) {
  const kinds = KINDS.map(([k, name, color]) => ({ k, name, color, ...(rep.size.by_kind?.[k] ?? { code: 0, files: 0 }) })).filter((x) => x.files)
  const total = kinds.reduce((a, x) => a + x.code, 0) || 1
  return (
    <Section title="What the factory wrote" description={`${num(rep.size.total.code)} code lines in ${rep.size.total.files} files, not counting comments and blank lines.`}>
      <div className="flex h-7 gap-0.5 overflow-hidden rounded-md" role="img" aria-label="Code lines by kind">
        {kinds.map((x) => (
          <Tooltip key={x.k}>
            <TooltipTrigger asChild>
              <span className="grid place-items-center text-[11px] font-medium text-white" style={{ flex: `${x.code} 1 0`, background: x.color }}>
                {x.code / total > 0.09 ? `${Math.round((x.code / total) * 100)}%` : ''}
              </span>
            </TooltipTrigger>
            <TooltipContent>
              {x.name}: {num(x.code)} code lines in {x.files} files
            </TooltipContent>
          </Tooltip>
        ))}
      </div>
      <ul className="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-xs">
        {kinds.map((x) => (
          <li key={x.k} className="flex items-center gap-1.5">
            <i className="size-2.5 rounded-sm" style={{ background: x.color }} />
            {x.name} <b>{num(x.code)}</b>
          </li>
        ))}
      </ul>
    </Section>
  )
}

function Origins({ rep }: { rep: Report }) {
  const names: Record<string, string> = { template: 'Templates', buf: 'buf generate', llm: 'LLM', tool: 'Tools (go mod tidy)' }
  const rows = Object.entries(rep.size.by_origin ?? {}).sort((a, b) => b[1].code - a[1].code)
  const total = rows.reduce((a, [, v]) => a + v.code, 0) || 1
  return (
    <Section title="Who wrote it" description="Origin of every file, from the factory's manifest.">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Origin</TableHead>
            <TableHead className="text-right">Files</TableHead>
            <TableHead className="text-right">Code lines</TableHead>
            <TableHead className="text-right">Share</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map(([k, v]) => (
            <TableRow key={k}>
              <TableCell>
                <Origin origin={k} /> {names[k] ?? k}
              </TableCell>
              <TableCell className="text-right tabular-nums">{v.files}</TableCell>
              <TableCell className="text-right tabular-nums">{num(v.code)}</TableCell>
              <TableCell className="text-right tabular-nums">{Math.round((v.code / total) * 100)}%</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Section>
  )
}

function Coverage({ rep }: { rep: Report }) {
  const cov = rep.coverage
  if (!cov)
    return (
      <Section title="Coverage by package">
        <p className="text-sm text-muted-foreground">The tests did not run, so there is no coverage profile.</p>
      </Section>
    )
  return (
    <Section title="Coverage by package" description="Statements run by go test -race and the PostgreSQL tests, generated code excluded.">
      {(cov.packages ?? []).map((p) => (
        <Meter key={p.package} label={p.package} value={p.percent} sub={pct(p.percent)} title={`${p.covered} of ${p.statements} statements covered`} />
      ))}
    </Section>
  )
}

function Timeline({ run }: { run: Run }) {
  const started = run.stages.filter((s) => s.started_at)
  if (!started.length) return null
  const t0 = Math.min(...started.map((s) => Date.parse(s.started_at!)))
  const end = Math.max(...started.map((s) => Date.parse(s.started_at!) + s.duration_ms), t0 + 1)
  return (
    <Section title="Time on the line" description="When each station started and how long it took. Amber marks a check with warnings, red a failed gate.">
      <div className="space-y-1">
        {run.stages.map((s) => (
          <div key={s.id} className="grid grid-cols-[110px_1fr_56px] items-center gap-2 text-xs">
            <span className="truncate text-muted-foreground">{s.name}</span>
            <span className="relative h-2.5 rounded-sm bg-muted">
              {s.started_at && (
                <span
                  className={cn('absolute inset-y-0 rounded-sm bg-chart-1', s.status === 'failed' && 'bg-destructive', s.status === 'warning' && 'bg-warn')}
                  style={{ left: `${((Date.parse(s.started_at) - t0) / (end - t0)) * 100}%`, width: `${Math.max(0.4, (s.duration_ms / (end - t0)) * 100)}%` }}
                />
              )}
            </span>
            <span className="text-right tabular-nums">{s.started_at ? ms(s.duration_ms) : s.status === 'skipped' ? 'skipped' : ''}</span>
          </div>
        ))}
      </div>
    </Section>
  )
}

function Runtime({ rep }: { rep: Report }) {
  const rt = rep.runtime
  if (!rt) return null
  const store = rt.store === 'postgres' ? ', backed by PostgreSQL' : rt.store === 'memory' ? ', with the in-memory store' : ''
  return (
    <Section
      title="Running the binary"
      description={`The smoke station started the server${store}, checked health and reflection, sent ${num(rt.load_calls)} calls to ${rt.load_method.split('/').pop()} from ${rt.load_concurrency} workers, then sent SIGTERM.`}
    >
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 xl:grid-cols-6">
        <Tile label="Ready" value={ms(rt.startup_ms)} note={`health ${rt.health_status || 'n/a'}; first exec ${ms(rt.first_exec_ms)}`} />
        <Tile label="Latency p50" value={ms(rt.p50_ms)} note={`p90 ${ms(rt.p90_ms)}`} />
        <Tile label="Latency p99" value={ms(rt.p99_ms)} note={`${rt.load_errors} errors`} tone={rt.load_errors ? 'bad' : undefined} />
        <Tile label="Throughput" value={`${num(Math.round(rt.rps))}/s`} note="calls per second" />
        <Tile label="Memory" value={rt.rss_bytes ? mb(rt.rss_bytes) : '–'} note="resident after load" />
        <Tile label="Shutdown" value={ms(rt.shutdown_ms)} note={rt.clean_exit ? 'clean exit' : 'exited with an error'} tone={rt.clean_exit ? 'good' : 'bad'} />
      </div>
    </Section>
  )
}

function Robustness({ rep }: { rep: Report }) {
  const fz = rep.fuzz
  const db = rep.integration
  return (
    <Section title="Fuzzing and integration" description="Properties checked against generated inputs, and the store contract against a real PostgreSQL.">
      {fz ? (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Fuzz target</TableHead>
              <TableHead className="text-right">Inputs</TableHead>
              <TableHead className="text-right">New paths</TableHead>
              <TableHead>Property</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {(fz.targets ?? []).map((t) => (
              <TableRow key={t.name}>
                <TableCell className="font-mono text-xs">{t.name}</TableCell>
                <TableCell className="text-right tabular-nums">{num(t.execs)}</TableCell>
                <TableCell className="text-right tabular-nums">{t.interesting}</TableCell>
                <TableCell>
                  <Badge variant="outline" className={t.failed ? STATUS_BADGE.failed : STATUS_BADGE.passed}>
                    {t.failed ? 'input found' : 'holds'}
                  </Badge>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      ) : (
        <p className="text-sm text-muted-foreground">Fuzzing was not run.</p>
      )}
      <p className="mt-3 flex items-center gap-1.5 text-sm">
        {db ? (
          <>
            {db.failed ? <CircleX className="size-4 text-destructive" /> : <CircleCheck className="size-4 text-good" />}
            {db.failed ? `${db.failed} of ${db.tests}` : db.tests} store contract tests on {db.database}, started in {ms(db.start_ms)}.
          </>
        ) : (
          <span className="text-muted-foreground">The PostgreSQL store was not tested against a database in this run.</span>
        )}
      </p>
    </Section>
  )
}

function Supply({ rep, run }: { rep: Report; run: Run }) {
  const sup = rep.supply
  const ct = rep.contract
  const id = encodeURIComponent(run.id)
  const compat = !ct
    ? 'not checked'
    : ct.baseline
      ? ct.changes?.length
        ? `${ct.changes.length} breaking changes against run ${ct.baseline}`
        : `compatible with run ${ct.baseline}`
      : (ct.note ?? 'no baseline')
  const rows: [string, React.ReactNode][] = [['Compatibility', compat]]
  if (sup)
    rows.push(
      ['Components', `${sup.components} in the SBOM (CycloneDX 1.6)`],
      ['Toolchain', sup.go_version],
      ['Binary', <code className="font-mono text-xs">sha256:{sup.binary_sha256.slice(0, 16)}…</code>],
      ['Provenance', 'in-toto statement, SLSA provenance v1, unsigned: sign it in your release pipeline'],
    )
  return (
    <Section title="Contract and supply chain" description="API compatibility with the last passing run, and what went into the binary.">
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-sm">
        {rows.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground">{k}</dt>
            <dd className="break-words">{v}</dd>
          </div>
        ))}
      </dl>
      {(ct?.changes ?? []).length > 0 && (
        <ul className="mt-3 space-y-1 text-sm">
          {ct!.changes!.map((c, i) => (
            <li key={i}>
              {c.message} <span className="text-xs text-muted-foreground">{c.code}</span>
            </li>
          ))}
        </ul>
      )}
      {sup ? (
        <div className="mt-4 flex flex-wrap gap-2">
          <Button variant="outline" size="sm" asChild>
            <a href={`/api/runs/${id}/sbom`}>
              <Download /> SBOM
            </a>
          </Button>
          <Button variant="outline" size="sm" asChild>
            <a href={`/api/runs/${id}/provenance`}>
              <Download /> Provenance
            </a>
          </Button>
        </div>
      ) : (
        <p className="mt-3 text-sm text-muted-foreground">No SBOM: the line stopped before the supply-chain station.</p>
      )}
    </Section>
  )
}

const FN_COLS: [keyof Func, string, boolean][] = [
  ['name', 'Function', false],
  ['file', 'Where', false],
  ['cyclomatic', 'Cyclomatic', true],
  ['cognitive', 'Cognitive', true],
  ['mi', 'Maintainability', true],
  ['loc', 'Lines', true],
  ['coverage', 'Coverage', true],
]

function Functions({ rep, open }: { rep: Report; open: Open }) {
  const [sort, setSort] = useState<{ key: keyof Func; dir: number }>({ key: 'cyclomatic', dir: -1 })
  const [all, setAll] = useState(false)
  const [tests, setTests] = useState(false)
  const fns = (rep.functions ?? [])
    .filter((f) => tests || !f.test)
    .sort((a, b) => {
      const av = a[sort.key] ?? -1
      const bv = b[sort.key] ?? -1
      return (typeof av === 'string' ? av.localeCompare(String(bv)) : Number(av) - Number(bv)) * sort.dir
    })
  const shown = all ? fns : fns.slice(0, 15)
  const hot = 'font-semibold text-destructive'
  return (
    <Section title="Functions" description="Every function the factory wrote, measured. Red marks a value past its threshold.">
      <div className="mb-3 flex flex-wrap items-center gap-4 text-sm">
        <div className="flex items-center gap-2">
          <Checkbox id="fn-tests" checked={tests} onCheckedChange={(c) => setTests(c === true)} />
          <Label htmlFor="fn-tests" className="font-normal">
            Include tests
          </Label>
        </div>
        <span className="text-muted-foreground">
          Showing {shown.length} of {fns.length}
        </span>
        {fns.length > 15 && (
          <Button variant="outline" size="sm" onClick={() => setAll(!all)}>
            {all ? 'Show the top 15' : `Show all ${fns.length}`}
          </Button>
        )}
      </div>
      <Table>
        <TableHeader>
          <TableRow>
            {FN_COLS.map(([k, label, numeric]) => (
              <TableHead key={k} className={numeric ? 'text-right' : ''} aria-sort={sort.key === k ? (sort.dir < 0 ? 'descending' : 'ascending') : 'none'}>
                <button
                  className="font-medium hover:text-foreground"
                  onClick={() => setSort(sort.key === k ? { key: k, dir: -sort.dir } : { key: k, dir: ['mi', 'coverage', 'name', 'file'].includes(k) ? 1 : -1 })}
                >
                  {label}
                  {sort.key === k ? (sort.dir < 0 ? ' ▾' : ' ▴') : ''}
                </button>
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {shown.map((f) => (
            <TableRow key={`${f.file}:${f.line}`}>
              <TableCell className="font-mono text-xs">
                {f.name} {f.test && <Origin origin="test" />} {f.origin === 'llm' && <Origin origin="llm" />}
              </TableCell>
              <TableCell>
                <button className="font-mono text-xs text-primary underline-offset-2 hover:underline" onClick={() => open(f.file, f.line)}>
                  {f.file}:{f.line}
                </button>
              </TableCell>
              <TableCell className={cn('text-right tabular-nums', f.cyclomatic > 10 && hot)}>{f.cyclomatic}</TableCell>
              <TableCell className={cn('text-right tabular-nums', f.cognitive > 15 && hot)}>{f.cognitive}</TableCell>
              <TableCell className={cn('text-right tabular-nums', f.mi < 20 && hot)}>{num1(f.mi)}</TableCell>
              <TableCell className="text-right tabular-nums">{f.loc}</TableCell>
              <TableCell className={cn('text-right tabular-nums', f.coverage != null && f.coverage < 50 && hot)}>{f.coverage == null ? '–' : pct(f.coverage)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Section>
  )
}

function Findings({ rep }: { rep: Report }) {
  const f = rep.findings
  const groups: [string, keyof Report['findings']][] = [
    ['policy', 'policy'], ['buf lint', 'buf_lint'], ['go vet', 'vet'], ['golangci-lint', 'lint'], ['staticcheck', 'staticcheck'],
    ['go fuzz', 'fuzz'], ['go fix', 'modernize'], ['govulncheck', 'vulns'],
  ]
  const items = groups.flatMap(([tool, key]) => (f[key] ?? []).map((x) => ({ ...x, tool })))
  const from = 'From the LLM policy, buf lint, go vet, golangci-lint (staticcheck, gosec, errcheck and more), fuzzing, the go fix modernizers and govulncheck.'
  return (
    <Section title="Findings" description={items.length ? `${items.length} findings. ${from}` : from}>
      {items.length === 0 ? (
        <p className="flex items-center gap-1.5 text-sm text-good">
          <CircleCheck className="size-4" /> No findings.
        </p>
      ) : (
        <ul className="space-y-2 text-sm">
          {items.map((x, i) => (
            <li key={i} className="flex gap-2">
              {x.tool === 'govulncheck' ? <CircleX className="mt-0.5 size-4 shrink-0 text-destructive" /> : <CircleAlert className="mt-0.5 size-4 shrink-0 text-warn" />}
              <span>
                {x.message}
                <span className="block text-xs text-muted-foreground">
                  {x.tool}
                  {x.code ? ` ${x.code}` : ''}, {x.file}
                  {x.line ? `:${x.line}` : ''}
                </span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

const usd = (v?: number) => (v ? `$${v < 0.01 ? v.toFixed(4) : v.toFixed(3)}` : '–')
const pc = (p: PassCount) => (p.total ? `${p.passed} of ${p.total}` : '–')

const STATUS_LABEL: Record<Candidate['status'], string> = {
  ran: 'ran', compile: 'does not compile', policy: 'policy violation', incomplete: 'incomplete answer', 'no answer': 'no answer',
}

function Tournament({ rep }: { rep: Report }) {
  const t = rep.factory.tournament
  if (!t) return null
  const rows = [...t.candidates].sort((a, b) => b.score - a.score)
  return (
    <Section
      title="Model tournament"
      description={`${t.candidates.length} models wrote the custom RPCs and their tests. Each implementation ran against the visible examples and against the tests the other models wrote without seeing it, and the best-supported one was kept. Holdout examples took no part in the choice, so they still judge the winner fairly.`}
    >
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Model</TableHead>
            <TableHead>Result</TableHead>
            <TableHead className="text-right">Examples</TableHead>
            <TableHead className="text-right">Others' tests</TableHead>
            <TableHead className="text-right">Own tests</TableHead>
            <TableHead className="text-right">Selection</TableHead>
            <TableHead className="text-right">Tokens</TableHead>
            <TableHead className="text-right">Cost</TableHead>
            <TableHead className="text-right">Time</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((c) => (
            <TableRow key={c.model} className={cn(c.chosen && 'bg-signal/15 hover:bg-signal/20')}>
              <TableCell className="font-mono text-xs">
                <span className="flex items-center gap-1.5">
                  {c.chosen && <Trophy className="size-3.5" aria-label="chosen" />}
                  {c.model}
                </span>
              </TableCell>
              <TableCell className="text-xs">
                {c.note ? (
                  <Tooltip>
                    <TooltipTrigger className={cn('underline decoration-dotted underline-offset-2', c.status !== 'ran' && 'text-destructive')}>{STATUS_LABEL[c.status]}</TooltipTrigger>
                    <TooltipContent className="max-w-96 font-mono text-[11px]">{c.note}</TooltipContent>
                  </Tooltip>
                ) : (
                  <span className={cn(c.status !== 'ran' && 'text-destructive')}>{STATUS_LABEL[c.status]}</span>
                )}
              </TableCell>
              <TableCell className="text-right tabular-nums">{pc(c.visible)}</TableCell>
              <TableCell className="text-right tabular-nums">
                {c.cross && Object.keys(c.cross).length ? (
                  <Tooltip>
                    <TooltipTrigger className="underline decoration-dotted underline-offset-2">{pc(c.foreign)}</TooltipTrigger>
                    <TooltipContent>
                      {Object.entries(c.cross).map(([author, p]) => (
                        <div key={author}>
                          tests by {author}: {pc(p)}
                        </div>
                      ))}
                      {c.failed_tests?.length ? (
                        <div className="mt-1.5 border-t border-background/20 pt-1.5">
                          Fails:
                          {c.failed_tests.map((t) => (
                            <div key={t} className="font-mono">
                              {t}
                            </div>
                          ))}
                        </div>
                      ) : null}
                    </TooltipContent>
                  </Tooltip>
                ) : (
                  pc(c.foreign)
                )}
              </TableCell>
              <TableCell className="text-right tabular-nums">{pc(c.own)}</TableCell>
              <TableCell className="text-right tabular-nums">{c.score >= 0 ? c.score.toFixed(2) : '–'}</TableCell>
              <TableCell className="text-right tabular-nums">{num(c.tokens_in + c.tokens_out)}</TableCell>
              <TableCell className="text-right tabular-nums">{usd(c.cost_usd)}</TableCell>
              <TableCell className="text-right tabular-nums">{ms(c.ms)}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      <p className="mt-3 text-xs text-muted-foreground">Selection weighs visible examples 50%, other models' tests 35% and the model's own tests 15%.</p>
    </Section>
  )
}

const SEVERITY: Record<string, string> = { bug: 'bg-destructive text-white', risk: 'bg-warn text-foreground', nit: 'bg-muted text-muted-foreground' }

function Review({ rep, open }: { rep: Report; open: Open }) {
  const r = rep.factory.review
  if (!r) return null
  const items = r.findings ?? []
  return (
    <Section title="Second-model review" description={`${r.model} reviewed the LLM-written code against the method docs. It reports; it never stops the line.`}>
      {r.note && <p className="mb-3 text-xs text-muted-foreground">{r.note}</p>}
      {items.length === 0 ? (
        <p className="flex items-center gap-1.5 text-sm text-good">
          <CircleCheck className="size-4" /> No defects reported.
        </p>
      ) : (
        <ul className="space-y-2 text-sm">
          {items.map((f, i) => (
            <li key={i} className="flex items-start gap-2">
              <Badge className={cn('mt-0.5 h-4 w-10 justify-center px-1 text-[10.5px]', SEVERITY[f.severity])}>{f.severity}</Badge>
              <span>
                {f.message}
                <button className="block font-mono text-xs text-muted-foreground hover:text-foreground hover:underline" onClick={() => open(f.file, f.line)}>
                  {f.file}:{f.line}
                </button>
              </span>
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

function Factory({ rep, run }: { rep: Report; run: Run }) {
  const fx = rep.factory
  return (
    <Section title="The factory itself" description="Measures of the line, so changes to templates or models can be compared run against run.">
      <div className="grid grid-cols-2 gap-3">
        <Tile label="Total time" value={ms(fx.total_ms)} note="spec to last gate" />
        <Tile label="First pass" value={fx.first_pass ? 'Yes' : 'No'} note={fx.repair_attempts ? `${fx.repair_attempts} repair attempts` : 'no repairs needed'} tone={fx.first_pass ? 'good' : undefined} />
        <Tile
          label="LLM"
          value={run.options?.llm ? (fx.llm_model ?? 'not configured') : 'off'}
          note={`${fx.llm_calls} calls${fx.rejected_writes?.length ? `, ${fx.rejected_writes.length} writes rejected` : ''}`}
        />
        <Tile label="Tokens" value={num(fx.tokens_in + fx.tokens_out)} note={`${num(fx.tokens_in)} in (${num(fx.tokens_cached ?? 0)} cached), ${num(fx.tokens_out)} out`} />
        {fx.cost_usd ? <Tile label="LLM cost" value={usd(fx.cost_usd)} note="as reported by the provider" /> : null}
      </div>
    </Section>
  )
}

export function Measurements({ run, open }: { run: Run; open: Open }) {
  const rep = run.report
  if (!rep?.score?.parts || !rep.complexity) {
    if (run.status === 'running') return <Empty title="The line is running">Measurements appear when the last station finishes. Open the Log tab to follow each tool's output live.</Empty>
    if (run.status === 'canceled') return <Empty title="No measurements for this run">The run was stopped before it was measured.</Empty>
    return <Empty title="No measurements for this run">The line stopped before any code existed. The Log tab shows why.</Empty>
  }
  const c = rep.complexity
  const cov = rep.coverage
  const svc = cov?.packages?.filter((p) => p.package.startsWith('internal/'))
  const svcCov = svc?.length ? (100 * svc.reduce((a, p) => a + p.covered, 0)) / svc.reduce((a, p) => a + p.statements, 0) : undefined
  const t = rep.tests
  const f = rep.findings
  const codeFind = (['buf_lint', 'vet', 'lint', 'staticcheck', 'policy', 'fuzz'] as const).reduce((a, k) => a + (f[k]?.length ?? 0), 0)
  const mut = rep.mutation
  const acc = rep.acceptance
  const comp = rep.completeness
  const judged = mut ? mut.total - mut.invalid - mut.skipped - (mut.equivalent ?? 0) : 0
  return (
    <div className="space-y-4">
      <ScoreCard rep={rep} />
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4 2xl:grid-cols-8">
        <Tile
          label="Mutation score"
          value={mut?.score != null ? pct(mut.score * 100) : '–'}
          note={mut ? `${mut.killed + mut.timed_out} of ${judged} faults caught, ${mut.survived} survived` : 'not run'}
          tone={mut?.score != null ? (mut.score < 0.8 ? 'bad' : 'good') : undefined}
        />
        <Tile
          label="Spec examples"
          value={acc ? `${acc.passed} of ${acc.examples?.length ?? 0}` : '–'}
          note={acc ? (acc.pending ? `${acc.pending} pending: methods are stubs` : `${acc.holdout} holdout, hidden from the LLM`) : 'the spec has none'}
          tone={acc?.failed ? 'bad' : acc && !acc.pending ? 'good' : undefined}
        />
        <Tile label="Service coverage" value={pct(svcCov == null ? null : Math.round(svcCov * 10) / 10)} note={cov ? `${pct(cov.percent)} including main` : 'tests did not run'} />
        <Tile label="Tests" value={t ? num(t.passed) : '–'} note={t ? (t.failed ? `${t.failed} failed` : `all passing, ${t.top_level} top-level`) : 'did not run'} tone={t?.failed ? 'bad' : 'good'} />
        <Tile label="Cyclomatic, mean" value={num1(c.mean_cyclomatic)} note={`max ${c.max_cyclomatic}, ${c.over_cyclomatic} above 10`} />
        <Tile label="Findings" value={num(codeFind)} note={`${f.vulns?.length ?? 0} vulnerabilities, ${f.modernize?.length ?? 0} modernizations`} tone={codeFind ? 'bad' : undefined} />
        <Tile label="RPCs implemented" value={`${comp.implemented} of ${comp.total}`} note={comp.stubbed?.length ? `stubs: ${comp.stubbed.join(', ')}` : 'no stubs left'} />
        <Tile label="Server binary" value={rep.build.binary_bytes ? mb(rep.build.binary_bytes) : '–'} note={`${rep.build.direct_deps} direct, ${rep.build.indirect_deps} indirect deps`} />
      </div>
      <div className="grid gap-4 xl:grid-cols-2">
        <Acceptance acc={acc} />
        <Mutation rep={rep} open={open} />
      </div>
      <Tournament rep={rep} />
      <div className="grid gap-4 xl:grid-cols-2">
        <Size rep={rep} />
        <Origins rep={rep} />
      </div>
      <div className="grid gap-4 xl:grid-cols-3">
        <Histogram title="Cyclomatic complexity" description="Decision points per function. Above 10 is hard to test." data={c.cyclomatic_hist ?? []} />
        <Histogram title="Cognitive complexity" description="How hard each function is to read; nesting costs more." data={c.cognitive_hist ?? []} />
        <Histogram title="Maintainability Index" description="Per function, 0–100 from Halstead volume, complexity and length." data={c.mi_hist ?? []} />
      </div>
      <div className="grid gap-4 xl:grid-cols-2">
        <Coverage rep={rep} />
        <Timeline run={run} />
      </div>
      <Runtime rep={rep} />
      <div className="grid gap-4 xl:grid-cols-2">
        <Robustness rep={rep} />
        <Supply rep={rep} run={run} />
      </div>
      <Functions rep={rep} open={open} />
      <div className="grid gap-4 xl:grid-cols-2">
        <Findings rep={rep} />
        {rep.factory.review ? <Review rep={rep} open={open} /> : <Factory rep={rep} run={run} />}
      </div>
      {rep.factory.review && <Factory rep={rep} run={run} />}
    </div>
  )
}

