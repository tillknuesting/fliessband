import { useRef, useState } from 'react'
import { CircleAlert, CircleCheck, CircleX, Play } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { postJSON } from '@/lib/api'
import type { Example, FactoryStatus, Options, Validation } from '@/lib/types'

const OPTIONS: { key: keyof Omit<Options, 'model' | 'models' | 'reviewer' | 'llm'>; label: string }[] = [
  { key: 'mutation', label: 'Mutation testing: inject faults, see which tests catch them' },
  { key: 'fuzz', label: 'Fuzz each target for 3 seconds' },
  { key: 'integration', label: 'Test the PostgreSQL store in Docker' },
  { key: 'smoke', label: 'Start the binary and load-test it' },
  { key: 'vuln', label: 'Check for known vulnerabilities' },
]

interface Props {
  spec: string
  onSpec: (s: string) => void
  examples: Example[]
  example: string
  onExample: (name: string) => void
  validation: Validation | null
  status: FactoryStatus | null
  running: boolean
  onStarted: (id: string) => void
}

/** A plain textarea with line numbers; Tab inserts spaces, Cmd/Ctrl+Enter runs. */
function Editor({ value, onChange, onRun }: { value: string; onChange: (v: string) => void; onRun: () => void }) {
  const gutter = useRef<HTMLPreElement>(null)
  const lines = value.split('\n').length
  return (
    <div className="flex h-[min(56vh,560px)] overflow-hidden rounded-md border bg-muted/30 font-mono text-[12.5px] leading-5 focus-within:ring-2 focus-within:ring-ring/50">
      <pre ref={gutter} aria-hidden className="m-0 shrink-0 overflow-hidden border-r bg-muted/60 px-2 py-2 text-right text-muted-foreground select-none">
        {Array.from({ length: lines }, (_, i) => i + 1).join('\n')}
      </pre>
      <textarea
        aria-label="Service spec in YAML"
        spellCheck={false}
        autoCapitalize="off"
        autoComplete="off"
        className="min-w-0 flex-1 resize-none bg-transparent px-3 py-2 whitespace-pre outline-none"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        onScroll={(e) => {
          if (gutter.current) gutter.current.scrollTop = e.currentTarget.scrollTop
        }}
        onKeyDown={(e) => {
          if (e.key === 'Tab' && !e.shiftKey) {
            e.preventDefault()
            e.currentTarget.setRangeText('  ', e.currentTarget.selectionStart, e.currentTarget.selectionEnd, 'end')
            onChange(e.currentTarget.value)
          }
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault()
            onRun()
          }
        }}
      />
    </div>
  )
}

function Check({ v }: { v: Validation | null }) {
  if (!v) return null
  const problems = [...v.problems.filter((p) => !p.warning), ...v.problems.filter((p) => p.warning)]
  return (
    <div className="space-y-1.5 text-[13px]" aria-live="polite">
      {v.error && (
        <p className="flex gap-1.5 text-destructive">
          <CircleX className="mt-0.5 size-3.5 shrink-0" />
          {v.error}
        </p>
      )}
      {v.ok && (
        <p className="flex gap-1.5 text-good">
          <CircleCheck className="mt-0.5 size-3.5 shrink-0" />
          <span>
            {v.service}: {v.rpcs?.length} RPCs, {v.files} files, {v.examples} {v.examples === 1 ? 'example' : 'examples'}
            {v.holdout ? ` (${v.holdout} holdout)` : ''}
          </span>
        </p>
      )}
      {problems.length > 0 && (
        <ul className="max-h-28 space-y-1 overflow-auto">
          {problems.map((p, i) => (
            <li key={i} className="flex gap-1.5">
              {p.warning ? <CircleAlert className="mt-0.5 size-3.5 shrink-0 text-warn" /> : <CircleX className="mt-0.5 size-3.5 shrink-0 text-destructive" />}
              <span>
                <code className="font-mono text-xs text-muted-foreground">{p.path}</code> {p.message}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

export function SpecPanel(props: Props) {
  const { spec, onSpec, examples, example, onExample, validation, status, running, onStarted } = props
  const [opts, setOpts] = useState<Options>({ mutation: true, fuzz: true, integration: true, smoke: true, vuln: true, llm: false, model: '', reviewer: '' })
  const [modelText, setModelText] = useState('')
  const [starting, setStarting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const canRun = !!validation?.ok && !running && !starting

  const start = async () => {
    if (!canRun) return
    setStarting(true)
    setError(null)
    try {
      const { id } = await postJSON<{ id: string }>('/api/runs', {
        spec,
        options: {
          ...opts,
          model: undefined,
          models: modelText.split(',').map((m) => m.trim()).filter(Boolean),
          reviewer: opts.reviewer?.trim() || undefined,
        },
      })
      onStarted(id)
    } catch (err) {
      setError(`Could not start the run: ${(err as Error).message}`)
    } finally {
      setStarting(false)
    }
  }

  return (
    <Card className="h-fit lg:sticky lg:top-4">
      <CardHeader className="flex-row items-center justify-between gap-2">
        <CardTitle>Service spec</CardTitle>
        <div className="flex items-center gap-2">
          <Label htmlFor="example" className="text-xs text-muted-foreground">
            Start from
          </Label>
          <Select value={example} onValueChange={onExample}>
            <SelectTrigger id="example" size="sm" className="w-36">
              <SelectValue placeholder="Example" />
            </SelectTrigger>
            <SelectContent>
              {examples.map((e) => (
                <SelectItem key={e.name} value={e.name}>
                  {e.file}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </CardHeader>
      <CardContent className="space-y-4">
        <Editor value={spec} onChange={onSpec} onRun={start} />
        <Check v={validation} />
        <fieldset className="space-y-2.5">
          <legend className="mb-2 text-sm font-medium">On this run</legend>
          {OPTIONS.map((o) => (
            <div key={o.key} className="flex items-start gap-2">
              <Checkbox id={`opt-${o.key}`} checked={opts[o.key]} onCheckedChange={(c) => setOpts({ ...opts, [o.key]: c === true })} className="mt-0.5" />
              <Label htmlFor={`opt-${o.key}`} className="text-[13px] leading-snug font-normal">
                {o.label}
              </Label>
            </div>
          ))}
          <div className="flex items-start gap-2">
            <Checkbox id="opt-llm" disabled={!status?.llm} checked={opts.llm} onCheckedChange={(c) => setOpts({ ...opts, llm: c === true })} className="mt-0.5" />
            <Label htmlFor="opt-llm" className="text-[13px] leading-snug font-normal">
              Let the LLM implement custom RPCs
            </Label>
          </div>
          <div className="grid grid-cols-[64px_1fr] items-center gap-x-2 gap-y-1.5 pl-6">
            <Label htmlFor="opt-models" className="text-xs text-muted-foreground">
              Models
            </Label>
            <Input
              id="opt-models"
              disabled={!status?.llm}
              placeholder={status?.llm ? status.llm.models.join(', ') : 'default'}
              value={modelText}
              onChange={(e) => setModelText(e.target.value)}
              className="h-7 font-mono text-xs"
            />
            <Label htmlFor="opt-reviewer" className="text-xs text-muted-foreground">
              Reviewer
            </Label>
            <Input
              id="opt-reviewer"
              disabled={!status?.llm}
              placeholder={status?.llm?.reviewer || 'none'}
              value={opts.reviewer}
              onChange={(e) => setOpts({ ...opts, reviewer: e.target.value })}
              className="h-7 font-mono text-xs"
            />
          </div>
          <p className="pl-6 text-xs text-muted-foreground">
            {status?.llm
              ? `Through ${new URL(status.llm.base_url).host}. List several models, separated by commas, and they compete: each implementation is tested against the others' tests and the best one is kept. Models may only write custom.go and its test.`
              : 'Not configured: start fliessband with FLIESSBAND_LLM_API_KEY or OPENAI_API_KEY set.'}
          </p>
        </fieldset>
        {error && <p className="text-[13px] text-destructive">{error}</p>}
        <Button size="lg" className="w-full bg-signal font-semibold text-foreground hover:bg-signal/85" disabled={!canRun} onClick={start}>
          <Play className="size-4" />
          {running ? 'The line is running…' : 'Run the line'}
        </Button>
      </CardContent>
    </Card>
  )
}
