// A line diff (longest common subsequence), grouped into hunks with context.

export type DiffLine = { kind: ' ' | '+' | '-'; text: string; a?: number; b?: number }
export type Hunk = { lines: DiffLine[]; skipped: number }

export function diffLines(before: string, after: string): DiffLine[] {
  const a = before.split('\n')
  const b = after.split('\n')
  if (a.length * b.length > 4_000_000) return [...a.map((t, i) => ({ kind: '-' as const, text: t, a: i + 1 })), ...b.map((t, i) => ({ kind: '+' as const, text: t, b: i + 1 }))]
  const n = a.length
  const m = b.length
  const dp = Array.from({ length: n + 1 }, () => new Uint32Array(m + 1))
  for (let i = n - 1; i >= 0; i--) for (let j = m - 1; j >= 0; j--) dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
  const out: DiffLine[] = []
  let i = 0
  let j = 0
  while (i < n && j < m) {
    if (a[i] === b[j]) out.push({ kind: ' ', text: a[i], a: ++i, b: ++j })
    else if (dp[i + 1][j] >= dp[i][j + 1]) out.push({ kind: '-', text: a[i], a: ++i })
    else out.push({ kind: '+', text: b[j], b: ++j })
  }
  while (i < n) out.push({ kind: '-', text: a[i], a: ++i })
  while (j < m) out.push({ kind: '+', text: b[j], b: ++j })
  return out
}

/** Keeps changed lines and `context` lines around them; the rest is counted as skipped. */
export function hunks(lines: DiffLine[], context = 3): Hunk[] {
  const keep = lines.map(() => false)
  lines.forEach((l, i) => {
    if (l.kind !== ' ') for (let k = Math.max(0, i - context); k <= Math.min(lines.length - 1, i + context); k++) keep[k] = true
  })
  const out: Hunk[] = []
  let skipped = 0
  let cur: DiffLine[] = []
  lines.forEach((l, i) => {
    if (keep[i]) {
      if (!cur.length) out.push({ lines: (cur = []), skipped })
      cur.push(l)
      skipped = 0
    } else {
      skipped++
      cur = []
    }
  })
  if (skipped && out.length) out.push({ lines: [], skipped })
  return out
}

export const changed = (lines: DiffLine[]) => ({ added: lines.filter((l) => l.kind === '+').length, removed: lines.filter((l) => l.kind === '-').length })
