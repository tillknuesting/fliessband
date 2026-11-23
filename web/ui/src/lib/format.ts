const nf0 = new Intl.NumberFormat('en')
const nf1 = new Intl.NumberFormat('en', { maximumFractionDigits: 1 })

export const num = (v: number) => nf0.format(v)
export const num1 = (v: number) => nf1.format(v)
export const pct = (v?: number | null) => (v == null ? '–' : `${nf1.format(v)}%`)
export const mb = (b: number) => `${nf1.format(b / 1048576)} MB`
export const ms = (v?: number | null) =>
  v == null ? '' : v < 1 ? `${v.toFixed(2)} ms` : v < 1000 ? `${Math.round(v)} ms` : `${(v / 1000).toFixed(v < 10000 ? 1 : 0)} s`
export const plural = (n: number, one: string, many = one + 's') => `${n} ${n === 1 ? one : many}`
