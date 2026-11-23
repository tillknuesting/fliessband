// A small tokenizer for Go, proto and YAML. It returns tokens per line, so
// the code is rendered as React text (never as HTML).

export type Tok = { t: string; c?: 'k' | 't' | 's' | 'n' | 'c' | 'f' | 'y' }
export type Lang = 'go' | 'proto' | 'yaml' | 'plain'

const GO_KW = new Set('break case chan const continue default defer else fallthrough for func go goto if import interface map package range return select struct switch type var'.split(' '))
const GO_TY = new Set('any bool byte comparable error float32 float64 int int8 int16 int32 int64 rune string uint uint8 uint16 uint32 uint64 uintptr true false nil iota'.split(' '))
const PB_KW = new Set('syntax package import option message service rpc returns repeated enum oneof map reserved optional stream'.split(' '))
const PB_TY = new Set('string bool bytes int32 int64 uint32 uint64 float double sint32 sint64 fixed32 fixed64'.split(' '))

export function langOf(path: string): Lang {
  if (path.endsWith('.go')) return 'go'
  if (path.endsWith('.proto')) return 'proto'
  if (/\.(ya?ml|json)$/.test(path)) return 'yaml'
  return 'plain'
}

/** Splits tokens that span lines (block comments, raw strings) into lines. */
function toLines(toks: Tok[]): Tok[][] {
  const lines: Tok[][] = [[]]
  for (const tok of toks) {
    tok.t.split('\n').forEach((part, i) => {
      if (i > 0) lines.push([])
      if (part) lines[lines.length - 1].push({ t: part, c: tok.c })
    })
  }
  return lines
}

export function highlight(src: string, lang: Lang): Tok[][] {
  if (lang === 'yaml') {
    return src.split('\n').map((l) => {
      if (/^\s*#/.test(l)) return [{ t: l, c: 'c' }]
      const m = l.match(/^(\s*-?\s*)("?[\w.-]+"?)(\s*:)(.*)$/)
      if (!m) return [{ t: l }]
      return [{ t: m[1] }, { t: m[2], c: 'y' }, { t: m[3] }, { t: m[4], c: /^\s*"/.test(m[4]) ? 's' : undefined }]
    })
  }
  if (lang === 'plain') return src.split('\n').map((l) => [{ t: l, c: /^\s*#/.test(l) ? 'c' : undefined }])
  const kw = lang === 'go' ? GO_KW : PB_KW
  const ty = lang === 'go' ? GO_TY : PB_TY
  const re = /(\/\/[^\n]*|\/\*[\s\S]*?\*\/)|("(?:[^"\\\n]|\\.)*"|`[^`]*`|'(?:[^'\\\n]|\\.)*')|(\b(?:0x[0-9a-fA-F_]+|\d[\d_]*(?:\.\d+)?(?:[eE][+-]?\d+)?)\b)|([A-Za-z_][A-Za-z0-9_]*)/g
  const toks: Tok[] = []
  let last = 0
  for (const m of src.matchAll(re)) {
    const i = m.index ?? 0
    if (i > last) toks.push({ t: src.slice(last, i) })
    if (m[1]) toks.push({ t: m[1], c: 'c' })
    else if (m[2]) toks.push({ t: m[2], c: 's' })
    else if (m[3]) toks.push({ t: m[3], c: 'n' })
    else {
      const w = m[4]
      if (kw.has(w)) toks.push({ t: w, c: 'k' })
      else if (ty.has(w) || (lang === 'proto' && /^[A-Z]/.test(w))) toks.push({ t: w, c: 't' })
      else if (src[i + w.length] === '(') toks.push({ t: w, c: 'f' })
      else toks.push({ t: w })
    }
    last = i + m[0].length
  }
  if (last < src.length) toks.push({ t: src.slice(last) })
  return toLines(toks)
}

export const TOKEN_CLASS: Record<NonNullable<Tok['c']>, string> = {
  k: 'text-violet-700 dark:text-violet-300',
  t: 'text-sky-700 dark:text-sky-300',
  s: 'text-emerald-700 dark:text-emerald-300',
  n: 'text-amber-700 dark:text-amber-300',
  c: 'text-muted-foreground italic',
  f: 'text-blue-700 dark:text-blue-300',
  y: 'text-sky-700 dark:text-sky-300',
}
