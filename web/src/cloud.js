// Shared helpers for the cloud autoscaler pages.

export const serverStatus = {
  requested: { label: 'Requested', dot: 'bg-ink-400', text: 'text-ink-300', bar: '#7a849b', hint: 'the hub ordered the server' },
  starting: { label: 'Starting', dot: 'bg-sky-400', text: 'text-sky-300', bar: '#38bdf8', hint: 'booting, tunnel and worker connecting' },
  working: { label: 'Working', dot: 'bg-emerald-400', text: 'text-emerald-300', bar: '#34d399', hint: 'the worker takes tests' },
  idle: { label: 'Idle', dot: 'bg-amber-400', text: 'text-amber-300', bar: '#fbbf24', hint: 'no work, waiting to be shut down' },
  draining: { label: 'Shutting down', dot: 'bg-orange-400', text: 'text-orange-300', bar: '#fb923c', hint: 'the last tests finish' },
  deleted: { label: 'Deleted', dot: 'bg-ink-500', text: 'text-ink-400', bar: '#4a5368', hint: 'the server was returned' },
  failed: { label: 'Failed', dot: 'bg-red-400', text: 'text-red-300', bar: '#f87171', hint: 'see the reason' },
}

export const providerLabel = { hetzner: 'Hetzner', aws: 'AWS' }

export function money(v, currency) {
  if (v == null || Number.isNaN(v)) return '—'
  const sym = currency === 'USD' ? '$' : '€'
  const digits = Math.abs(v) < 10 ? 2 : Math.abs(v) < 100 ? 1 : 0
  return `${sym}${v.toFixed(digits)}`
}

/** "1:05" style offset from a start timestamp. */
export function offset(from, to) {
  if (!from || !to || to < from) return ''
  const s = Math.round((to - from) / 1000)
  if (s < 3600) return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, '0')}`
  return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}

/** "4 min" style countdown to ts. */
export function countdown(ts, now) {
  if (!ts) return ''
  const s = Math.max(0, Math.round((ts - now) / 1000))
  if (s < 60) return `${s} s`
  if (s < 3600) return `${Math.ceil(s / 60)} min`
  return `${Math.floor(s / 3600)} h ${Math.round((s % 3600) / 60)} min`
}

export function minutes(sec) {
  if (sec == null) return '—'
  if (sec < 60) return `${sec} s`
  if (sec % 3600 === 0) return `${sec / 3600} h`
  return `${Math.round(sec / 60)} min`
}

/**
 * Run-time and cost model from the specification:
 * test time / (parallel tests × servers) + server start-up, never shorter than the longest test.
 */
export function estimateRun({ provider, servers, concurrency, testTimeMs, longestMs, startupMs, hourly }) {
  if (!servers || !testTimeMs) return null
  const runMs = Math.max(testTimeMs / (concurrency * servers), longestMs || 0) + (startupMs || 0)
  const hours = runMs / 3_600_000
  const billed = provider === 'hetzner' ? Math.max(1, Math.ceil(hours)) : hours
  const cost = hourly ? servers * billed * hourly : null
  return { runMs, cost }
}
