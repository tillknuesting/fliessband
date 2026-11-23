// Types of the factory's JSON API (internal/pipeline, internal/metrics).

export type Status = 'pending' | 'running' | 'passed' | 'failed' | 'warning' | 'skipped' | 'canceled'

export interface Stage {
  id: string
  name: string
  tool: string
  gate: boolean
  phase?: string
  status: Status
  summary: string
  started_at?: string
  duration_ms: number
  attempt?: number
  log: string[]
}

export interface Finding { tool: string; file: string; line: number; code?: string; message: string }
export interface Bucket { label: string; count: number }
export interface ScorePart { name: string; weight: number; points: number; detail: string; measured?: boolean }
export interface LineCount { files: number; lines: number; code: number; comment: number; blank: number }

export interface Func {
  name: string; file: string; line: number; end_line: number; loc: number; params: number
  cyclomatic: number; cognitive: number; mi: number; test: boolean; origin: string; coverage?: number
}

export interface Mutant {
  id: number; file: string; line: number; operator: string; original: string; mutated: string
  before: string; after: string; origin: string; status: string
}

export interface Report {
  size: { files: unknown[]; by_origin: Record<string, LineCount>; by_kind: Record<string, LineCount>; total: LineCount }
  complexity: {
    funcs: number; test_funcs: number; mean_cyclomatic: number; p90_cyclomatic: number; max_cyclomatic: number
    mean_cognitive: number; max_cognitive: number; over_cyclomatic: number; over_cognitive: number
    mean_mi: number; min_mi: number; cyclomatic_hist: Bucket[]; cognitive_hist: Bucket[]; mi_hist: Bucket[]
  }
  functions: Func[]
  coverage?: { percent: number; statements: number; covered: number; packages: { package: string; statements: number; covered: number; percent: number }[] }
  tests?: { total: number; top_level: number; passed: number; failed: number; skipped: number }
  findings: Partial<Record<'buf_lint' | 'vet' | 'lint' | 'staticcheck' | 'modernize' | 'vulns' | 'policy' | 'fuzz', Finding[] | null>>
  build: { binary_bytes: number; direct_deps: number; indirect_deps: number; go_version: string }
  runtime?: {
    store?: string; first_exec_ms: number; startup_ms: number; health_status: string; load_method: string
    load_calls: number; load_concurrency: number; load_errors: number; rps: number; p50_ms: number; p90_ms: number
    p99_ms: number; rss_bytes: number; shutdown_ms: number; clean_exit: boolean
  }
  completeness: { total: number; implemented: number; stubbed?: string[] | null }
  acceptance?: { passed: number; failed: number; pending: number; holdout: number; examples: { method: string; name: string; holdout: boolean; status: string; message?: string }[] }
  mutation?: {
    total: number; killed: number; survived: number; timed_out: number; not_covered: number; invalid: number
    equivalent: number; skipped: number; score?: number; strength?: number; sampled: boolean; duration_ms: number
    by_operator: { operator: string; total: number; killed: number }[]
    by_file: { file: string; origin: string; total: number; killed: number; score?: number }[]
    survivors: Mutant[] | null
  }
  fuzz?: { targets: { name: string; execs: number; interesting: number; failed: boolean; input?: string }[]; execs: number; failures: number }
  contract?: { baseline?: string; package: string; changes: Finding[] | null; note?: string }
  integration?: { database: string; tests: number; failed: number; start_ms: number }
  supply?: { components: number; go_version: string; binary_sha256: string }
  factory: {
    total_ms: number; llm_model?: string; llm_calls: number; tokens_in: number; tokens_out: number; tokens_cached?: number
    repair_attempts: number; first_pass: boolean; rejected_writes?: string[] | null
    hardening?: { survivors: number; killed: number; equivalent: number; kept: boolean; note?: string }
    cost_usd?: number
    tournament?: Tournament
    review?: { model: string; findings: ReviewFinding[] | null; note?: string }
  }
  score: { total: number; grade: string; capped: boolean; parts: ScorePart[] }
}

export interface Options {
  llm: boolean; model?: string; models?: string[]; reviewer?: string; smoke: boolean; vuln: boolean; mutation: boolean; fuzz: boolean; integration: boolean
}

export interface Run {
  id: string; name: string; created_at: string; finished_at?: string; status: Status
  options: Options; spec: string; stages: Stage[]; report?: Report
}

export interface Summary {
  id: string; name: string; created_at: string; status: Status; llm: boolean; score?: number; grade?: string
  coverage?: number; mean_cyclomatic?: number; mean_mi?: number; code_lines: number; tests: number; findings: number
  rpcs: number; implemented: number; duration_ms: number; p99_ms?: number; failed_at?: string
  mutation_score?: number; examples: number; examples_passed: number; cost_usd?: number; stages?: Brief[]
}

export interface Brief { id: string; status: Status; ms: number; started_at?: string; summary?: string }

export interface Validation {
  ok: boolean; error?: string; problems: { path: string; message: string; warning: boolean }[]
  service?: string; rpcs?: string[]; examples: number; holdout: number; files?: number; proto?: string; outline?: Outline
}

export interface OutlineField { name: string; type: string; repeated?: boolean; required?: boolean; unique?: boolean }
export interface Outline {
  entities: { name: string; doc?: string; fields: OutlineField[]; rpcs: string[] }[]
  methods: { name: string; doc?: string; request: OutlineField[]; response: OutlineField[]; touches: string[]; examples: number; holdout: number }[]
  features: string[]
}

export interface Tool { name: string; version: string; found: boolean }
export interface FactoryStatus {
  tools: Tool[]; llm: { model: string; base_url: string; models: string[]; reviewer: string } | null; version: string; auth: boolean
  capacity?: { lines: number; active: number; queued: number }
}
export interface Example { name: string; file: string; source: string }
export interface FileEntry { path: string; bytes: number; origin: string }

export interface PassCount { passed: number; total: number }
export interface Candidate {
  model: string; status: 'ran' | 'compile' | 'policy' | 'incomplete' | 'no answer'; note?: string
  own: PassCount; visible: PassCount; foreign: PassCount; cross?: Record<string, PassCount> | null; failed_tests?: string[] | null
  score: number; tokens_in: number; tokens_out: number; cost_usd?: number; ms: number; chosen: boolean
}
export interface Tournament { winner: string; candidates: Candidate[] }
export interface ReviewFinding { file: string; line: number; severity: 'bug' | 'risk' | 'nit'; message: string }
