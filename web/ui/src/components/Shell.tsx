import { useEffect, useState } from 'react'
import { ArrowDownRight, ArrowUpRight, Ban, Boxes, CircleCheck, CircleX, Factory, History as HistoryIcon, LoaderCircle, Moon, Play, Search, Sun, Workflow } from 'lucide-react'
import { CommandDialog, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList, CommandShortcut } from '@/components/ui/command'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { cn } from '@/lib/utils'
import type { Example, FactoryStatus, Status, Summary } from '@/lib/types'

export function Logo({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 28 28" aria-hidden className={cn('size-5', className)}>
      <rect x="2.75" y="16.75" width="22.5" height="7.5" rx="3.75" fill="none" stroke="currentColor" strokeWidth="1.5" />
      <circle cx="6.5" cy="20.5" r="1.75" fill="currentColor" />
      <circle cx="14" cy="20.5" r="1.75" fill="currentColor" />
      <circle cx="21.5" cy="20.5" r="1.75" fill="currentColor" />
      <rect x="5" y="9" width="7" height="6.5" rx="1" fill="currentColor" />
      <rect x="15" y="6" width="8" height="9.5" rx="1" fill="var(--signal)" />
    </svg>
  )
}

export function StatusIcon({ status, className }: { status: Status; className?: string }) {
  const c = cn('size-3.5 shrink-0', className)
  switch (status) {
    case 'passed':
      return <CircleCheck className={cn(c, 'text-good')} aria-label="passed" />
    case 'failed':
      return <CircleX className={cn(c, 'text-destructive')} aria-label="failed" />
    case 'running':
      return <LoaderCircle className={cn(c, 'animate-spin text-signal')} aria-label="running" />
    default:
      return <Ban className={cn(c, 'text-muted-foreground')} aria-label="stopped" />
  }
}

export function ago(iso: string) {
  const s = (Date.now() - new Date(iso).getTime()) / 1000
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)} min ago`
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`
  return new Date(iso).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

export function Kbd({ children }: { children: React.ReactNode }) {
  return <kbd className="key">{children}</kbd>
}

export const MOD = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform) ? '⌘' : 'Ctrl'

/** Fires the Run action wherever the spec editor lives. */
export const runLine = () => window.dispatchEvent(new Event('fliessband:run'))

export function useTheme() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains('dark'))
  useEffect(() => {
    document.documentElement.classList.toggle('dark', dark)
    localStorage.setItem('theme', dark ? 'dark' : 'light')
  }, [dark])
  // An explicit value, not an updater: a command that fires twice must not flip it back.
  return [dark, () => setDark(!dark)] as const
}

/** Runs grouped by spec name: each name is a product, each run a version. */
export function productsOf(runs: Summary[]) {
  const by = new Map<string, Summary[]>()
  for (const r of runs) by.set(r.name, [...(by.get(r.name) ?? []), r])
  return [...by.entries()]
    .map(([name, rs]) => {
      const scored = rs.filter((r) => r.score != null) // newest first
      return { name, versions: rs.length, running: rs.filter((r) => r.status === 'running').length, latest: rs[0], trend: scored.length > 1 ? scored[0].score! - scored[1].score! : 0 }
    })
    .sort((a, b) => b.latest.created_at.localeCompare(a.latest.created_at))
}

function NavItem({ href, active, icon: Icon, label, keys }: { href: string; active: boolean; icon: typeof Workflow; label: string; keys: string }) {
  return (
    <a
      href={href}
      aria-current={active ? 'page' : undefined}
      className="group flex h-7 items-center gap-2 rounded-md px-2 text-[13px] text-sidebar-foreground hover:bg-sidebar-accent aria-[current=page]:bg-sidebar-accent aria-[current=page]:text-sidebar-accent-foreground"
    >
      <Icon className="size-4 opacity-70" />
      {label}
      <span className="ml-auto hidden gap-0.5 group-hover:flex">
        {keys.split(' ').map((k) => (
          <Kbd key={k}>{k}</Kbd>
        ))}
      </span>
    </a>
  )
}

export function Sidebar(props: {
  view: 'line' | 'history' | 'floor' | 'product'
  product?: string
  runId: string | null
  runs: Summary[]
  status: FactoryStatus | null
  dark: boolean
  onTheme: () => void
  onSearch: () => void
}) {
  const { view, product, runId, runs, status, dark, onTheme, onSearch } = props
  const products = productsOf(runs)
  const found = status?.tools.filter((t) => t.found).length ?? 0
  return (
    <aside className="flex h-screen w-60 shrink-0 flex-col border-r border-sidebar-border bg-sidebar">
      <div className="flex h-12 items-center gap-2 px-4">
        <Logo className="text-sidebar-accent-foreground" />
        <span className="text-[14px] font-semibold tracking-[-0.01em] text-sidebar-accent-foreground">Fliessband</span>
        {status && <span className="ml-auto text-[11px] text-muted-foreground">v{status.version}</span>}
      </div>
      <div className="flex gap-1.5 px-3 pb-3">
        <button
          onClick={() => {
            location.hash = '#/'
            setTimeout(runLine, 0)
          }}
          title={`Run the line (${MOD}+Enter)`}
          className="flex h-8 flex-1 items-center gap-2 rounded-md border border-sidebar-border bg-card px-2.5 text-[13px] font-medium whitespace-nowrap shadow-xs hover:bg-accent"
        >
          <Play className="size-3.5 fill-signal text-signal" /> Run the line
        </button>
        <Tooltip>
          <TooltipTrigger asChild>
            <button onClick={onSearch} aria-label="Search and commands" className="grid size-8 place-items-center rounded-md border border-sidebar-border bg-card shadow-xs hover:bg-accent">
              <Search className="size-3.5" />
            </button>
          </TooltipTrigger>
          <TooltipContent>
            Search and commands <Kbd>{MOD}</Kbd> <Kbd>K</Kbd>
          </TooltipContent>
        </Tooltip>
      </div>
      <nav aria-label="Views" className="space-y-px px-3">
        <NavItem href="#/floor" active={view === 'floor'} icon={Factory} label="Floor" keys="G F" />
        <NavItem href={runId ? `#/run/${runId}` : '#/'} active={view === 'line'} icon={Workflow} label="Line" keys="G L" />
        <NavItem href="#/history" active={view === 'history'} icon={HistoryIcon} label="History" keys="G H" />
      </nav>
      <div className="mt-5 flex items-center px-5 pb-1 text-[11.5px] font-medium text-muted-foreground">Products</div>
      <ul className="space-y-px px-3">
        {products.map((p) => (
          <li key={p.name}>
            <a
              href={`#/product/${encodeURIComponent(p.name)}`}
              aria-current={view === 'product' && product === p.name ? 'page' : undefined}
              className="flex h-7 items-center gap-2 rounded-md px-2 text-[13px] text-sidebar-foreground transition-colors hover:bg-sidebar-accent aria-[current=page]:bg-sidebar-accent aria-[current=page]:text-sidebar-accent-foreground"
            >
              <Boxes className="size-3.5 opacity-60" />
              <span className="truncate">{p.name}</span>
              <span className="ml-auto flex shrink-0 items-center gap-1 text-[11.5px] text-muted-foreground tabular-nums">
                {p.running > 0 && <LoaderCircle className="size-3 animate-spin text-signal" />}
                {p.trend > 0.5 && <ArrowUpRight className="size-3 text-good" />}
                {p.trend < -0.5 && <ArrowDownRight className="size-3 text-destructive" />}
                v{p.versions}
              </span>
            </a>
          </li>
        ))}
      </ul>
      <div className="mt-4 flex items-center px-5 pb-1 text-[11.5px] font-medium text-muted-foreground">Recent runs</div>
      <ol className="min-h-0 flex-1 space-y-px overflow-y-auto px-3 pb-3">
        {runs.slice(0, 40).map((r) => (
          <li key={r.id} className="animate-in duration-300 fade-in-0 slide-in-from-left-1">
            <a
              href={`#/run/${r.id}`}
              aria-current={view === 'line' && r.id === runId ? 'page' : undefined}
              className="flex h-8 items-center gap-2 rounded-md px-2 text-[13px] text-sidebar-foreground hover:bg-sidebar-accent aria-[current=page]:bg-sidebar-accent aria-[current=page]:text-sidebar-accent-foreground"
            >
              <StatusIcon status={r.status} />
              <span className="truncate">{r.name}</span>
              <span className="ml-auto shrink-0 text-[11.5px] text-muted-foreground tabular-nums">
                {r.grade ? `${Math.round(r.score!)} ${r.grade}` : ago(r.created_at)}
              </span>
            </a>
          </li>
        ))}
        {runs.length === 0 && <li className="px-2 py-1 text-[12.5px] text-muted-foreground">Runs appear here.</li>}
      </ol>
      <div className="space-y-1.5 border-t border-sidebar-border px-4 py-3 text-[12px] text-muted-foreground">
        <Tooltip>
          <TooltipTrigger className="flex w-full items-center gap-2">
            <span className={cn('size-1.5 rounded-full', status && found === status.tools.length ? 'bg-good' : 'bg-warn')} />
            Toolchain {status ? `${found} of ${status.tools.length}` : 'unreachable'}
          </TooltipTrigger>
          <TooltipContent side="right">
            {status?.tools.map((t) => (
              <div key={t.name}>
                {t.found ? '✓' : '✕'} {t.name}: {t.found ? t.version : 'not found'}
              </div>
            ))}
          </TooltipContent>
        </Tooltip>
        <Tooltip>
          <TooltipTrigger className="flex w-full min-w-0 items-center gap-2">
            <span className={cn('size-1.5 shrink-0 rounded-full', status?.llm ? 'bg-good' : 'bg-muted-foreground/50')} />
            <span className="truncate">{status?.llm ? (status.llm.models.length > 1 ? `${status.llm.models.length} models compete` : status.llm.model) : 'LLM off'}</span>
          </TooltipTrigger>
          <TooltipContent side="right" className="max-w-80">
            {status?.llm ? (
              <>
                <div>{new URL(status.llm.base_url).host}</div>
                {status.llm.models.map((m) => (
                  <div key={m} className="font-mono">
                    {m}
                  </div>
                ))}
                {status.llm.reviewer && <div>Reviewer: {status.llm.reviewer}</div>}
              </>
            ) : (
              'Start fliessband with FLIESSBAND_LLM_API_KEY (or OPENAI_API_KEY) to let models implement custom RPCs.'
            )}
          </TooltipContent>
        </Tooltip>
        <button onClick={onTheme} className="flex items-center gap-2 hover:text-foreground">
          {dark ? <Sun className="size-3.5" /> : <Moon className="size-3.5" />}
          {dark ? 'Light theme' : 'Dark theme'}
        </button>
      </div>
    </aside>
  )
}

export function CommandMenu(props: {
  open: boolean
  onOpenChange: (open: boolean) => void
  runs: Summary[]
  examples: Example[]
  onExample: (name: string) => void
  onTheme: () => void
  onToggleSpec: () => void
}) {
  const { open, onOpenChange, runs, examples, onExample, onTheme, onToggleSpec } = props
  const go = (fn: () => void) => () => {
    onOpenChange(false)
    fn()
  }
  return (
    <CommandDialog open={open} onOpenChange={onOpenChange} title="Search and commands" description="Jump to a run or run a command">
      <CommandInput placeholder="Type a command or search runs…" />
      <CommandList>
        <CommandEmpty>Nothing matches.</CommandEmpty>
        <CommandGroup heading="Actions">
          <CommandItem
            onSelect={go(() => {
              location.hash = '#/'
              setTimeout(runLine, 0)
            })}
          >
            <Play /> Run the line
            <CommandShortcut>
              <Kbd>{MOD}</Kbd>
              <Kbd>↵</Kbd>
            </CommandShortcut>
          </CommandItem>
          <CommandItem onSelect={go(() => (location.hash = '#/floor'))}>
            <Factory /> Go to the floor
            <CommandShortcut>
              <Kbd>G</Kbd>
              <Kbd>F</Kbd>
            </CommandShortcut>
          </CommandItem>
          <CommandItem onSelect={go(() => (location.hash = '#/history'))}>
            <HistoryIcon /> Go to history
            <CommandShortcut>
              <Kbd>G</Kbd>
              <Kbd>H</Kbd>
            </CommandShortcut>
          </CommandItem>
          <CommandItem onSelect={go(onToggleSpec)}>
            <Workflow /> Show or hide the spec
            <CommandShortcut>
              <Kbd>[</Kbd>
            </CommandShortcut>
          </CommandItem>
          <CommandItem onSelect={go(onTheme)}>
            <Moon /> Switch theme
          </CommandItem>
        </CommandGroup>
        <CommandGroup heading="Start from an example">
          {examples.map((e) => (
            <CommandItem key={e.name} value={`example ${e.file}`} onSelect={go(() => onExample(e.name))}>
              <span className="font-mono text-[12.5px]">{e.file}</span>
            </CommandItem>
          ))}
        </CommandGroup>
        <CommandGroup heading="Runs">
          {runs.slice(0, 60).map((r) => (
            <CommandItem key={r.id} value={`${r.name} ${r.id} ${r.status}`} onSelect={go(() => (location.hash = `#/run/${r.id}`))}>
              <StatusIcon status={r.status} />
              <span>{r.name}</span>
              <span className="font-mono text-[11.5px] text-muted-foreground">{r.id.slice(-4)}</span>
              <CommandShortcut className="text-[12px] text-muted-foreground tabular-nums">
                {r.grade ? `${Math.round(r.score!)} ${r.grade}, ` : ''}
                {ago(r.created_at)}
              </CommandShortcut>
            </CommandItem>
          ))}
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  )
}
