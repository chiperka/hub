import { onBeforeUnmount, ref, watch } from 'vue'

// Global live stream: one EventSource on /api/events/ui, re-dispatched to listeners.
const listeners = new Map() // event name -> Set<fn>
export const connected = ref(false)
let es = null
let currentRun = ''
let retry = 1000

function dispatch(name, data) {
  for (const fn of listeners.get(name) || []) fn(data)
  for (const fn of listeners.get('*') || []) fn(name, data)
}

const EVENTS = ['hello', 'runs', 'workers', 'queue', 'test_update', 'step_update', 'test_log', 'run_update', 'run_completed', 'run_cancelled', 'artifact']

function connect() {
  if (es) es.close()
  es = new EventSource('/api/events/ui' + (currentRun ? '?run=' + encodeURIComponent(currentRun) : ''))
  es.onopen = () => { connected.value = true; retry = 1000 }
  es.onerror = () => {
    connected.value = false
    es.close()
    es = null
    setTimeout(connect, retry)
    retry = Math.min(retry * 2, 15000)
  }
  for (const name of EVENTS) {
    es.addEventListener(name, (e) => {
      let data = null
      try { data = JSON.parse(e.data) } catch { /* ignore */ }
      dispatch(name, data)
    })
  }
}

export function startLive() {
  if (!es) connect()
}

/** Focus the stream on one run so its fine-grained events are delivered. */
export function watchRun(runIdRef) {
  const stop = watch(runIdRef, (id) => {
    if ((id || '') === currentRun) return
    currentRun = id || ''
    connect()
  }, { immediate: true })
  onBeforeUnmount(() => {
    stop()
    if (currentRun) { currentRun = ''; connect() }
  })
}

/** Subscribe to an event for the lifetime of the calling component. */
export function onLive(name, fn) {
  if (!listeners.has(name)) listeners.set(name, new Set())
  listeners.get(name).add(fn)
  onBeforeUnmount(() => listeners.get(name).delete(fn))
}

/** Debounced refresh triggered by any of the given events. */
export function refreshOn(names, fn, wait = 400) {
  let t = null
  const trigger = () => { clearTimeout(t); t = setTimeout(fn, wait) }
  for (const n of [].concat(names)) onLive(n, trigger)
  onBeforeUnmount(() => clearTimeout(t))
}
