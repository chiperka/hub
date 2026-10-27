export function duration(ms) {
  if (ms == null || ms < 0) return '—'
  if (ms < 1000) return `${Math.round(ms)}ms`
  const s = ms / 1000
  if (s < 60) return `${s.toFixed(s < 10 ? 2 : 1)}s`
  const m = Math.floor(s / 60)
  const rs = Math.round(s % 60)
  if (m < 60) return `${m}m ${rs}s`
  return `${Math.floor(m / 60)}h ${m % 60}m`
}

export function ago(ts) {
  if (!ts) return '—'
  const d = (Date.now() - ts) / 1000
  if (d < 5) return 'just now'
  if (d < 60) return `${Math.floor(d)}s ago`
  if (d < 3600) return `${Math.floor(d / 60)}m ago`
  if (d < 86400) return `${Math.floor(d / 3600)}h ago`
  return `${Math.floor(d / 86400)}d ago`
}

export function dateTime(ts) {
  if (!ts) return '—'
  return new Date(ts).toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

export function bytes(n) {
  if (n == null) return '—'
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(1)} MB`
}

export function runDuration(run) {
  if (!run) return null
  return (run.completed_at || Date.now()) - run.started_at
}

/** Pretty-prints JSON strings; returns the input unchanged otherwise. */
export function pretty(s) {
  if (s == null) return ''
  if (typeof s !== 'string') return JSON.stringify(s, null, 2)
  const t = s.trim()
  if (!(t.startsWith('{') || t.startsWith('['))) return s
  try { return JSON.stringify(JSON.parse(t), null, 2) } catch { return s }
}

export function pct(a, b) {
  return b > 0 ? Math.round((a / b) * 100) : 0
}
