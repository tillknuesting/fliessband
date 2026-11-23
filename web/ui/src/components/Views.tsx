import { useEffect, useMemo, useRef, useState } from 'react'
import { Badge } from '@/components/ui/badge'
import { ScrollArea } from '@/components/ui/scroll-area'
import { api, getJSON } from '@/lib/api'
import { highlight, langOf, TOKEN_CLASS, type Lang } from '@/lib/highlight'
import { num } from '@/lib/format'
import { cn } from '@/lib/utils'
import type { FileEntry, Run, Stage, Validation } from '@/lib/types'

export function Empty({ title, children }: { title: string; children?: React.ReactNode }) {
  return (
    <div className="mx-auto max-w-lg py-16 text-center">
      <h3 className="font-semibold">{title}</h3>
      {children && <p className="mt-1 text-sm text-muted-foreground">{children}</p>}
    </div>
  )
}

const ORIGIN_STYLE: Record<string, string> = {
  template: '',
  buf: 'border-sky-300 bg-sky-50 text-sky-800 dark:bg-sky-950 dark:text-sky-200',
  llm: 'border-violet-300 bg-violet-50 text-violet-800 dark:bg-violet-950 dark:text-violet-200',
  tool: 'border-amber-300 bg-amber-50 text-amber-800 dark:bg-amber-950 dark:text-amber-200',
}

export function Origin({ origin }: { origin: string }) {
  return (
    <Badge variant="outline" className={cn('h-4 px-1.5 text-[10.5px] font-medium', ORIGIN_STYLE[origin])}>
      {origin}
    </Badge>
  )
}

/** Highlighted source with line numbers; marks stub lines and an optional target line. */
export function CodeBlock({ text, lang, mark, className }: { text: string; lang: Lang; mark?: number; className?: string }) {
  const lines = useMemo(() => text.replace(/\n$/, '').split('\n'), [text])
  const toks = useMemo(() => highlight(lines.join('\n'), lang), [lines, lang])
  const target = useRef<HTMLDivElement>(null)
  useEffect(() => {
    target.current?.scrollIntoView({ block: 'center' })
  }, [mark, text])
  return (
    <pre className={cn('m-0 overflow-auto bg-muted/30 py-2 font-mono text-[12.5px] leading-5', className)}>
      {toks.map((line, i) => {
        const marked = lines[i]?.includes('fliessband:implement') || mark === i + 1
        return (
          <div key={i} ref={mark === i + 1 ? target : undefined} className={cn('flex', marked && 'bg-signal/25')}>
            <span className="w-12 shrink-0 pr-3 text-right text-muted-foreground/70 select-none">{i + 1}</span>
            <span className="whitespace-pre pr-4">
              {line.length === 0 ? ' ' : line.map((t, j) => (t.c ? <span key={j} className={TOKEN_CLASS[t.c]}>{t.t}</span> : t.t))}
            </span>
          </div>
        )
      })}
    </pre>
  )
}

export function CodeView({ run, open }: { run: Run; open: { path: string; line?: number } | null }) {
  const [files, setFiles] = useState<FileEntry[]>([])
  const [sel, setSel] = useState<string | null>(null)
  const [text, setText] = useState('')
  const [line, setLine] = useState<number | undefined>()

  useEffect(() => {
    if (run.status === 'running') return
    getJSON<FileEntry[]>(`/api/runs/${encodeURIComponent(run.id)}/files`).then(setFiles, () => setFiles([]))
  }, [run.id, run.status])

  useEffect(() => {
    if (open) {
      setSel(open.path)
      setLine(open.line)
    }
  }, [open])

  const current = sel && files.some((f) => f.path === sel) ? sel : (files.find((f) => f.path.endsWith('.proto')) ?? files[0])?.path
  useEffect(() => {
    if (!current) return
    let stale = false
    api(`/api/runs/${encodeURIComponent(run.id)}/file?path=${encodeURIComponent(current)}`)
      .then((r) => r.text())
      .then((t) => !stale && setText(t), (err: Error) => !stale && setText(`Could not load: ${err.message}`))
    return () => {
      stale = true
    }
  }, [run.id, current])

  if (run.status === 'running') return <Empty title="Generating">Files appear here when the line finishes.</Empty>
  if (!files.length) return <Empty title="No code for this run">The line stopped before any code existed.</Empty>

  const dirs = new Map<string, FileEntry[]>()
  for (const f of files) {
    const d = f.path.includes('/') ? f.path.slice(0, f.path.lastIndexOf('/')) : ''
    dirs.set(d, [...(dirs.get(d) ?? []), f])
  }
  const meta = files.find((f) => f.path === current)
  const fns = (run.report?.functions ?? []).filter((f) => f.file === current)
  const stubs = text.split('\n').filter((l) => l.includes('fliessband:implement')).length

  return (
    <div className="grid min-h-[560px] overflow-hidden rounded-md border md:grid-cols-[280px_1fr]">
      <ScrollArea className="max-h-[70vh] border-b md:border-r md:border-b-0">
        <nav aria-label="Files" className="py-2">
          {[...dirs.keys()].sort().map((d) => (
            <div key={d}>
              <div className="px-3 pt-2 pb-1 text-xs font-medium text-muted-foreground">{d || 'service root'}</div>
              {dirs.get(d)!.map((f) => (
                <button
                  key={f.path}
                  aria-current={f.path === current}
                  onClick={() => {
                    setSel(f.path)
                    setLine(undefined)
                  }}
                  className="flex w-full items-center justify-between gap-2 py-1 pr-3 pl-5 text-left font-mono text-xs hover:bg-muted aria-[current=true]:bg-muted aria-[current=true]:font-semibold"
                >
                  <span className="truncate">{f.path.slice(f.path.lastIndexOf('/') + 1)}</span>
                  <Origin origin={f.origin} />
                </button>
              ))}
            </div>
          ))}
        </nav>
      </ScrollArea>
      <div className="flex min-w-0 flex-col">
        <div className="flex flex-wrap items-center gap-2 border-b px-3 py-2 text-xs text-muted-foreground">
          <b className="font-mono text-foreground">{current}</b>
          {meta && <Origin origin={meta.origin} />}
          <span>{num(text.split('\n').length)} lines</span>
          {fns.length > 0 && <span>{fns.length} functions, max cyclomatic {Math.max(...fns.map((f) => f.cyclomatic))}</span>}
          {stubs > 0 && <span className="text-destructive">{stubs} stubs waiting for an implementation</span>}
        </div>
        <CodeBlock text={text} lang={langOf(current ?? '')} mark={line} className="max-h-[70vh] flex-1" />
      </div>
    </div>
  )
}

function logClass(line: string) {
  if (line.startsWith('$ ')) return 'text-sky-300'
  if (/✗|FAIL|error:|panic:|survived #/.test(line)) return 'text-red-300'
  if (/^(ok\s|✓|wrote )/.test(line)) return 'text-emerald-300'
  return ''
}

export function LogView({ stages, selected, onSelect }: { stages: Stage[]; selected?: string; onSelect: (id: string) => void }) {
  const box = useRef<HTMLPreElement>(null)
  const stage = stages.find((s) => s.id === selected) ?? stages.find((s) => s.status === 'running') ?? stages.find((s) => s.status === 'failed') ?? stages[0]
  useEffect(() => {
    const el = box.current
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight < 80) el.scrollTop = el.scrollHeight
  }, [stage?.log.length, stage?.id])
  return (
    <div className="grid overflow-hidden rounded-md border md:grid-cols-[220px_1fr]">
      <nav aria-label="Stations" className="max-h-[70vh] overflow-auto border-b py-1 md:border-r md:border-b-0">
        {stages.map((s) => (
          <button
            key={s.id}
            aria-current={s.id === stage?.id}
            onClick={() => onSelect(s.id)}
            className="flex w-full items-center justify-between px-3 py-1.5 text-left text-sm hover:bg-muted aria-[current=true]:bg-muted aria-[current=true]:font-medium"
          >
            <span className="flex items-center gap-2">
              <span className={cn('size-2 rounded-full', { passed: 'bg-good', failed: 'bg-destructive', warning: 'bg-warn', running: 'bg-signal', pending: 'bg-border', skipped: 'bg-border', canceled: 'bg-foreground/40' }[s.status])} />
              {s.name}
            </span>
            <span className="text-xs text-muted-foreground tabular-nums">{s.log?.length || 0}</span>
          </button>
        ))}
      </nav>
      <pre ref={box} tabIndex={0} className="m-0 max-h-[70vh] min-h-[420px] overflow-auto bg-zinc-950 p-3 font-mono text-[12px] leading-5 whitespace-pre-wrap text-zinc-200">
        {!stage
          ? 'Pick a station.'
          : stage.log.length
            ? stage.log.map((l, i) => (
                <div key={i} className={logClass(l)}>
                  {l}
                </div>
              ))
            : stage.status === 'pending'
              ? 'This station has not started yet.'
              : stage.status === 'skipped'
                ? `Skipped: ${stage.summary}`
                : 'No output.'}
      </pre>
    </div>
  )
}

export function ContractView({ v }: { v: Validation | null }) {
  if (!v?.ok || !v.proto) return <Empty title="No contract to show">Fix the spec errors listed under the editor and the API contract appears here as you type.</Empty>
  return (
    <div className="space-y-3">
      <div>
        <h3 className="font-semibold">{v.service}</h3>
        <p className="text-sm text-muted-foreground">Live preview of the proto the editor's spec produces. {v.rpcs?.length} RPCs:</p>
      </div>
      <div className="flex flex-wrap gap-1.5">
        {v.rpcs?.map((r) => (
          <Badge key={r} variant="secondary" className="font-mono">
            {r}
          </Badge>
        ))}
      </div>
      <CodeBlock text={v.proto} lang="proto" className="max-h-[70vh] rounded-md border" />
    </div>
  )
}
