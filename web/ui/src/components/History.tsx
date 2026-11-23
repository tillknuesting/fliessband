import { CartesianGrid, Line, LineChart, XAxis, YAxis } from 'recharts'
import { Ban, CircleCheck, CircleX, LoaderCircle } from 'lucide-react'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from '@/components/ui/chart'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { ms, num, num1, pct } from '@/lib/format'
import type { Summary } from '@/lib/types'
import { Empty } from './Views'

const config = { score: { label: 'Quality score', color: 'var(--chart-1)' } } satisfies ChartConfig

const ICON = {
  passed: <CircleCheck className="size-4 text-good" />,
  failed: <CircleX className="size-4 text-destructive" />,
  canceled: <Ban className="size-4 text-muted-foreground" />,
}

export function History({ runs }: { runs: Summary[] }) {
  if (!runs.length) return <Empty title="No runs yet">Runs you start on the Line view are listed here with their key measurements.</Empty>
  const points = [...runs]
    .reverse()
    .filter((r) => r.score != null && r.grade)
    .map((r) => ({ id: r.id, label: `${r.name}, ${new Date(r.created_at).toLocaleString()}`, score: Math.round(r.score!) }))
  return (
    <Card>
      <CardHeader>
        <CardTitle>Run history</CardTitle>
        <CardDescription>Every run is kept with its spec, code and measurements. Compare runs to see how a template or model change moves the numbers.</CardDescription>
      </CardHeader>
      <CardContent className="space-y-6">
        {points.length > 1 && (
          <div>
            <h3 className="text-sm font-medium">Quality score per run</h3>
            <p className="mb-2 text-xs text-muted-foreground">Oldest on the left. Version 0.2 scores more strictly than earlier runs.</p>
            <ChartContainer config={config} className="aspect-auto h-44 w-full">
              <LineChart data={points} margin={{ left: 0, right: 12, top: 8 }} accessibilityLayer>
                <CartesianGrid vertical={false} />
                <XAxis dataKey="id" hide />
                <YAxis domain={[0, 100]} ticks={[0, 50, 100]} width={30} tickLine={false} axisLine={false} fontSize={11} />
                <ChartTooltip content={<ChartTooltipContent labelFormatter={(_, p) => p?.[0]?.payload?.label} />} />
                <Line dataKey="score" stroke="var(--color-score)" strokeWidth={2} dot={{ r: 4, fill: 'var(--color-score)', strokeWidth: 2, stroke: 'var(--card)' }} activeDot={{ r: 6 }} />
              </LineChart>
            </ChartContainer>
          </div>
        )}
        <Table>
          <TableHeader>
            <TableRow>
              {['Started', 'Service', 'Result'].map((h) => (
                <TableHead key={h}>{h}</TableHead>
              ))}
              {['Score', 'Mutation', 'Examples', 'Coverage', 'Cyclomatic', 'Code lines', 'Tests', 'Findings', 'RPCs done', 'p99', 'Time'].map((h) => (
                <TableHead key={h} className="text-right">
                  {h}
                </TableHead>
              ))}
              <TableHead>LLM</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {runs.map((r) => (
              <TableRow
                key={r.id}
                tabIndex={0}
                className="cursor-pointer"
                onClick={() => (location.hash = `#/run/${r.id}`)}
                onKeyDown={(e) => e.key === 'Enter' && (location.hash = `#/run/${r.id}`)}
              >
                <TableCell>{new Date(r.created_at).toLocaleString()}</TableCell>
                <TableCell className="font-medium">{r.name}</TableCell>
                <TableCell>
                  <span className="flex items-center gap-1.5">
                    {ICON[r.status as keyof typeof ICON] ?? <LoaderCircle className="size-4 animate-spin" />}
                    {r.status}
                    {r.failed_at ? ` at ${r.failed_at}` : ''}
                  </span>
                </TableCell>
                <TableCell className="text-right tabular-nums">{r.score != null && r.grade ? `${Math.round(r.score)} ${r.grade}` : '–'}</TableCell>
                <TableCell className="text-right tabular-nums">{r.mutation_score != null ? pct(r.mutation_score * 100) : '–'}</TableCell>
                <TableCell className="text-right tabular-nums">{r.examples ? `${r.examples_passed} of ${r.examples}` : '–'}</TableCell>
                <TableCell className="text-right tabular-nums">{pct(r.coverage)}</TableCell>
                <TableCell className="text-right tabular-nums">{r.mean_cyclomatic != null ? num1(r.mean_cyclomatic) : '–'}</TableCell>
                <TableCell className="text-right tabular-nums">{num(r.code_lines)}</TableCell>
                <TableCell className="text-right tabular-nums">{r.tests || '–'}</TableCell>
                <TableCell className="text-right tabular-nums">{r.findings}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {r.implemented} of {r.rpcs}
                </TableCell>
                <TableCell className="text-right tabular-nums">{r.p99_ms != null ? ms(r.p99_ms) : '–'}</TableCell>
                <TableCell className="text-right tabular-nums">{ms(r.duration_ms)}</TableCell>
                <TableCell>{r.llm ? 'on' : 'off'}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  )
}
