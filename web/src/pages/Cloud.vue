<script setup>
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { Cloud, Pause, Play, Plus, Settings, ArrowUp, ArrowDown, ArrowRight, Square, Zap, Clock, Check, Loader2, Server, ChevronDown, ChevronRight, Undo2 } from 'lucide-vue-next'
import { api } from '../api'
import { refreshOn } from '../live'
import { dateTime, duration } from '../format'
import { serverStatus, providerLabel, money, offset, countdown } from '../cloud'
import PageHeader from '../components/PageHeader.vue'
import Spinner from '../components/Spinner.vue'
import Modal from '../components/Modal.vue'
import CodeBlock from '../components/CodeBlock.vue'
import { toast, toastError } from '../components/toast'

const data = ref(null) // { state, servers }
const timeline = ref(null)
const costs = ref(null)
const events = ref([])
const eventsDone = ref(false)
const evServer = ref('')
const evKind = ref('')
const launching = ref(null) // { count }
const confirmStop = ref(null)
const showEnded = ref(false)
const now = ref(Date.now())
const clock = setInterval(() => (now.value = Date.now()), 1000)
onBeforeUnmount(() => clearInterval(clock))

async function loadMain() { data.value = await api.get('/api/cloud') }
async function loadTimeline() { timeline.value = await api.get('/api/cloud/timeline?hours=24') }
async function loadCosts() { costs.value = await api.get('/api/cloud/costs?days=30') }
async function loadEvents(more = false) {
  const q = new URLSearchParams({ limit: '50' })
  if (evServer.value) q.set('server', evServer.value)
  if (evKind.value) q.set('kind', evKind.value)
  if (more && events.value.length) q.set('before', events.value[events.value.length - 1].id)
  const page = await api.get('/api/cloud/events?' + q)
  events.value = more ? events.value.concat(page) : page
  eventsDone.value = page.length < 50
}
function loadAll() { loadMain(); loadTimeline(); loadCosts(); loadEvents() }
onMounted(loadAll)
refreshOn(['cloud', 'workers', 'queue'], () => { loadMain(); loadEvents() }, 400)
refreshOn(['cloud'], () => { loadTimeline(); loadCosts() }, 5000)

const state = computed(() => data.value?.state)
const active = computed(() => (data.value?.servers || []).filter((s) => s.status !== 'deleted' && s.status !== 'failed'))
const ended = computed(() => (data.value?.servers || []).filter((s) => s.status === 'deleted' || s.status === 'failed'))
const notConfigured = computed(() => state.value && !state.value.configured.hetzner && !state.value.configured.aws)

const statusLine = computed(() => {
  const s = state.value
  if (!s) return {}
  return {
    running: { text: 'running', cls: 'bg-emerald-400', ping: true },
    paused: { text: 'paused', cls: 'bg-amber-400' },
    budget: { text: 'budget used up', cls: 'bg-red-400' },
    price_unknown: { text: 'price unknown', cls: 'bg-amber-400' },
    backoff: { text: 'waiting after an error', cls: 'bg-amber-400' },
    not_configured: { text: 'not configured', cls: 'bg-ink-500' },
  }[s.status] || { text: s.status, cls: 'bg-ink-500' }
})

const budgetPct = computed(() => {
  const s = state.value
  // Spent plus the running servers' current hours: what the budget can no longer give out.
  return s?.budget > 0 ? Math.min(100, Math.max(0, Math.round(((s.budget - s.budget_left) / s.budget) * 100))) : 0
})

async function setEnabled(enabled) {
  try {
    await api.post('/api/cloud/pause', { enabled })
    toast(enabled ? 'Autoscaler resumed' : 'Autoscaler paused: no new servers')
    loadMain()
  } catch (e) { toastError(e) }
}

async function launch() {
  try {
    await api.post('/api/cloud/servers', { count: Number(launching.value.count) || 1 })
    launching.value = null
    toast('Server requested')
    loadMain()
  } catch (e) { toastError(e) }
}
async function stop(s, immediately) {
  try {
    await api.post(`/api/cloud/servers/${s.id}/terminate`, { now: immediately })
    confirmStop.value = null
    toast(immediately ? `${s.id} is being deleted` : `${s.id} finishes its tests and shuts down`)
    loadMain()
  } catch (e) { toastError(e) }
}
async function extend(s) {
  try { await api.post(`/api/cloud/servers/${s.id}/extend`); toast(`${s.id} stays for another 30 minutes`); loadMain() } catch (e) { toastError(e) }
}
async function revert(ev) {
  try { await api.post(`/api/cloud/settings/revert/${ev.id}`); toast('Settings reverted'); loadAll() } catch (e) { toastError(e) }
}

// Start-up steps with offsets from the moment the server was requested.
function steps(s) {
  const base = s.created_at
  const list = [
    { label: 'created', at: s.provider_created_at },
    { label: 'booted', at: s.boot_at },
    { label: 'tunnel', at: s.tunnel_at },
    { label: 'worker', at: s.worker_at },
    { label: 'first test', at: s.first_test_at },
  ]
  let waiting = false
  return list.map((st) => {
    const done = !!st.at
    const current = !done && !waiting && s.status !== 'failed' && s.status !== 'deleted'
    if (!done) waiting = true
    return { ...st, done, current, off: done ? offset(base, st.at) : '' }
  })
}

// Human-readable "why" for each server.
function story(s) {
  const lines = []
  if (s.reason) lines.push(s.reason)
  if (s.status === 'idle' && s.planned_stop_at) {
    const idleMin = Math.round((now.value - s.idle_since) / 60000)
    const tail = s.waits_for_hour ? ' (end of the paid hour)' : ''
    lines.push(`idle ${idleMin} min, shutting down in ${countdown(s.planned_stop_at, now.value)}${tail}`)
  }
  if (s.hold_until > now.value) lines.push(`kept until ${new Date(s.hold_until).toLocaleTimeString()}`)
  if (s.status === 'draining') lines.push(`shutting down: ${s.end_reason}${s.running_jobs ? `, ${s.running_jobs} test(s) finishing` : ''}`)
  if ((s.status === 'deleted' || s.status === 'failed') && s.end_reason) lines.push(s.end_reason)
  return lines
}

function money_(v) { return money(v, state.value?.currency) }
function serverMoney(s) { return s.price_unknown ? 'price unknown' : money(s.cost, s.currency) }

// ── timeline (SVG) ──────────────────────────────────────────────────────────
const W = 1000
const ROW = 14
const tl = computed(() => {
  const t = timeline.value
  if (!t) return null
  const span = t.to - t.from
  const x = (ts) => Math.max(0, Math.min(W, ((ts - t.from) / span) * W))
  const maxQ = Math.max(10, ...t.samples.map((s) => s.queue))
  const H = 40
  let path = ''
  t.samples.forEach((s, i) => { path += `${i ? 'L' : 'M'}${x(s.ts).toFixed(1)},${(H - (s.queue / maxQ) * H).toFixed(1)}` })
  const area = t.samples.length ? `${path}L${x(t.samples[t.samples.length - 1].ts).toFixed(1)},${H}L${x(t.samples[0].ts).toFixed(1)},${H}Z` : ''
  const hours = []
  for (let h = Math.ceil(t.from / 3_600_000) * 3_600_000; h < t.to; h += 3 * 3_600_000) hours.push({ x: x(h), label: new Date(h).getHours() + ':00' })
  const rows = t.servers.map((r) => ({ ...r, segs: r.segments.map((s) => ({ x: x(s.from), w: Math.max(1.5, x(s.to) - x(s.from)), color: serverStatus[s.status]?.bar || '#4a5368', status: s.status })) }))
  return { path, area, maxQ, H, hours, rows }
})

// ── costs ───────────────────────────────────────────────────────────────────
const costMax = computed(() => Math.max(0.01, ...(costs.value?.daily || []).map((d) => d.cost)))
</script>

<template>
  <PageHeader title="Cloud" subtitle="Servers rented from the cloud for big runs, returned when the queue is empty.">
    <template v-if="state && !notConfigured">
      <button class="btn" @click="setEnabled(!state.enabled)">
        <component :is="state.enabled ? Pause : Play" class="h-4 w-4" />{{ state.enabled ? 'Pause' : 'Resume' }}
      </button>
      <button class="btn" :disabled="state.status === 'not_configured'" @click="launching = { count: 1 }"><Plus class="h-4 w-4" />Start server</button>
    </template>
    <router-link to="/cloud/settings" class="btn btn-primary"><Settings class="h-4 w-4" />Settings</router-link>
  </PageHeader>

  <Spinner v-if="!data" />
  <template v-else>
    <!-- Not configured -->
    <div v-if="notConfigured" class="card mb-6 p-5">
      <div class="flex items-start gap-3">
        <div class="grid h-10 w-10 shrink-0 place-items-center rounded-lg bg-ink-800 text-ink-400"><Cloud class="h-5 w-5" /></div>
        <div class="min-w-0 flex-1 space-y-3 text-sm">
          <div class="font-semibold">Cloud workers are not configured on this hub</div>
          <p class="text-ink-400">Without provider credentials the hub works exactly as before. To rent workers on demand, set the credentials in the hub's environment and restart it:</p>
          <CodeBlock :code="'# Hetzner Cloud: API token of a project used only by Chiperka (read & write)\nCHIPERKA_CLOUD_HETZNER_TOKEN=...\n\n# AWS: an IAM user limited to Chiperka servers (contrib/cloud-image/aws-iam-policy.json)\nCHIPERKA_CLOUD_AWS_ACCESS_KEY_ID=...\nCHIPERKA_CLOUD_AWS_SECRET_ACCESS_KEY=...\nCHIPERKA_CLOUD_AWS_REGION=eu-central-1'" />
          <p class="text-ink-400">The hub connects to its cloud servers over SSH (outbound only, nothing opens on this machine). All other settings are edited under Settings.</p>
        </div>
      </div>
    </div>

    <!-- Status strip -->
    <div v-else class="card mb-6">
      <div class="grid gap-px overflow-hidden rounded-xl bg-ink-800 sm:grid-cols-2 lg:grid-cols-5">
        <div class="bg-ink-900 p-4">
          <div class="label">Autoscaler</div>
          <div class="flex items-center gap-2 text-sm font-semibold">
            <span class="relative flex h-2.5 w-2.5">
              <span v-if="statusLine.ping" class="absolute inline-flex h-full w-full animate-ping rounded-full opacity-60" :class="statusLine.cls" />
              <span class="relative inline-flex h-2.5 w-2.5 rounded-full" :class="statusLine.cls" />
            </span>
            {{ statusLine.text }} <span class="font-normal text-ink-400">({{ providerLabel[state.provider] }})</span>
          </div>
        </div>
        <div class="bg-ink-900 p-4">
          <div class="label">Queue</div>
          <div class="flex items-center gap-1.5 font-mono text-lg font-semibold">
            {{ state.queue }}
            <ArrowUp v-if="state.queue_trend > 0" class="h-4 w-4 text-amber-300" />
            <ArrowDown v-else-if="state.queue_trend < 0" class="h-4 w-4 text-emerald-300" />
            <span class="text-xs font-normal text-ink-400">tests waiting</span>
          </div>
        </div>
        <div class="bg-ink-900 p-4">
          <div class="label">Servers</div>
          <div class="font-mono text-lg font-semibold">{{ state.servers }} <span class="text-sm text-ink-400">/ {{ state.max_servers }}</span></div>
        </div>
        <div class="bg-ink-900 p-4">
          <div class="label">Today</div>
          <div class="font-mono text-lg font-semibold">{{ money_(state.spend_today) }}</div>
        </div>
        <div class="bg-ink-900 p-4">
          <div class="label">This month</div>
          <div class="font-mono text-lg font-semibold">
            {{ money_(state.spend_month) }}
            <span v-if="state.budget > 0" class="text-sm text-ink-400">of {{ money_(state.budget) }}</span>
          </div>
          <div v-if="state.price_unknown" class="mt-0.5 text-xs text-amber-300">without {{ state.price_unknown }} server{{ state.price_unknown > 1 ? 's' : '' }} of unknown price</div>
          <div v-if="state.budget > 0" class="mt-0.5 text-xs text-ink-400" title="The budget minus the spend and the hour each running server is in. New servers start only while their first hour fits; running ones stop before an hour that does not.">
            {{ money_(Math.max(0, state.budget_left)) }} left for new hours
          </div>
          <div v-for="(v, cur) in state.spend_other" :key="cur" class="mt-0.5 text-xs text-ink-400" title="Spent at the other provider, in its currency; not part of the budget.">
            + {{ money(v, cur) }} at the other provider
          </div>
          <div v-if="state.budget > 0" class="mt-1.5 h-1.5 overflow-hidden rounded-full bg-ink-800">
            <div class="h-full rounded-full" :class="budgetPct >= 100 ? 'bg-red-400' : budgetPct > 80 ? 'bg-amber-400' : 'bg-emerald-400'" :style="{ width: budgetPct + '%' }" />
          </div>
        </div>
      </div>
      <div v-if="state.status_message" class="border-t border-ink-800 px-4 py-2.5 text-xs text-amber-200">{{ state.status_message }}</div>
    </div>

    <!-- Active servers -->
    <div v-if="!notConfigured" class="mb-6">
      <h2 class="mb-3 text-sm font-semibold text-ink-200">Servers</h2>
      <div v-if="!active.length" class="card px-4 py-8 text-center text-sm text-ink-400">
        No cloud servers right now. They start when the queue grows past the first rule, or with “Start server”.
      </div>
      <div v-else class="grid gap-4 xl:grid-cols-2">
        <div v-for="s in active" :key="s.id" class="card flex flex-col">
          <div class="flex items-start gap-3 p-4">
            <div class="grid h-10 w-10 shrink-0 place-items-center rounded-lg bg-ink-800" :class="serverStatus[s.status]?.text"><Server class="h-5 w-5" /></div>
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <span class="font-mono font-semibold">{{ s.id }}</span>
                <span class="text-xs text-ink-400">{{ providerLabel[s.provider] }} · {{ s.type || '…' }}<template v-if="s.location"> · {{ s.location }}</template></span>
                <span v-if="s.provider === 'aws' && s.type" class="rounded px-1.5 py-0.5 text-[10px] font-medium uppercase" :class="s.spot ? 'bg-emerald-500/15 text-emerald-300' : 'bg-ink-700 text-ink-300'">{{ s.spot ? 'spot' : 'on-demand' }}</span>
                <span v-if="s.manual" class="rounded bg-indigo-500/15 px-1.5 py-0.5 text-[10px] font-medium uppercase text-indigo-300">manual</span>
              </div>
              <div class="font-mono text-xs text-ink-500">{{ s.public_ip || 'no IP yet' }}<template v-if="s.worker_id"> · <router-link to="/workers" class="link">{{ s.worker_id }}</router-link></template></div>
            </div>
            <div class="flex items-center gap-1.5 text-xs font-medium" :class="serverStatus[s.status]?.text" :title="serverStatus[s.status]?.hint">
              <Loader2 v-if="s.status === 'starting' || s.status === 'requested' || s.status === 'draining'" class="h-3.5 w-3.5 animate-spin" />
              <span v-else class="h-2 w-2 rounded-full" :class="serverStatus[s.status]?.dot" />
              {{ serverStatus[s.status]?.label }}
            </div>
          </div>

          <!-- Start-up steps -->
          <div class="flex flex-wrap items-center gap-x-1.5 gap-y-1 border-t border-ink-800 px-4 py-2.5 text-xs">
            <template v-for="(st, i) in steps(s)" :key="st.label">
              <ArrowRight v-if="i" class="h-3 w-3 text-ink-600" />
              <span :class="st.done ? 'text-ink-200' : st.current ? 'text-sky-300' : 'text-ink-500'" class="inline-flex items-center gap-1">
                <Check v-if="st.done" class="h-3 w-3 text-emerald-400" />
                <Loader2 v-else-if="st.current" class="h-3 w-3 animate-spin" />
                {{ st.label }}<span v-if="st.off" class="font-mono text-ink-400">{{ st.off }}</span>
              </span>
            </template>
          </div>

          <div class="grid grid-cols-2 gap-x-4 gap-y-3 border-t border-ink-800 px-4 py-3 text-xs">
            <div>
              <div class="mb-1 text-ink-400">Tests running</div>
              <div class="flex items-center gap-2">
                <div class="h-1.5 flex-1 overflow-hidden rounded-full bg-ink-800">
                  <div class="h-full rounded-full bg-emerald-400" :style="{ width: (s.concurrency ? Math.min(100, (s.running_jobs / s.concurrency) * 100) : 0) + '%' }" />
                </div>
                <span class="font-mono text-ink-200">{{ s.running_jobs }}/{{ s.concurrency || '—' }}</span>
              </div>
              <div class="mt-1 text-ink-500">{{ s.completed_jobs }} done in total</div>
            </div>
            <div>
              <div class="mb-1 text-ink-400">Time &amp; money</div>
              <div v-if="s.provider === 'hetzner' && s.paid_until" class="font-mono text-ink-200">paid for {{ countdown(s.paid_until, now) }} more</div>
              <div v-else-if="s.provider_created_at" class="font-mono text-ink-200">running {{ offset(s.provider_created_at, now) }}</div>
              <div v-if="s.price_unknown" class="text-amber-300">price unknown</div>
              <div v-else class="text-ink-500">~{{ serverMoney(s) }} so far<template v-if="s.hourly_price"> · {{ money(s.hourly_price, s.currency) }}/h</template></div>
            </div>
            <div class="col-span-2 space-y-0.5 text-ink-300">
              <div v-for="(l, i) in story(s)" :key="i" class="first-letter:uppercase">{{ l }}</div>
            </div>
          </div>
          <div class="mt-auto flex items-center gap-1 border-t border-ink-800 px-3 py-2">
            <button class="btn btn-ghost btn-sm" :disabled="s.status === 'draining'" @click="stop(s, false)"><Square class="h-3.5 w-3.5" />Stop</button>
            <button class="btn btn-ghost btn-sm text-red-300" @click="confirmStop = s"><Zap class="h-3.5 w-3.5" />Stop now</button>
            <button class="btn btn-ghost btn-sm ml-auto" :disabled="s.status === 'draining'" @click="extend(s)"><Clock class="h-3.5 w-3.5" />Keep 30 min</button>
          </div>
        </div>
      </div>

      <button v-if="ended.length" class="mt-3 flex items-center gap-1 text-xs text-ink-400 hover:text-ink-200" @click="showEnded = !showEnded">
        <component :is="showEnded ? ChevronDown : ChevronRight" class="h-3.5 w-3.5" />{{ ended.length }} server(s) ended in the last 24 hours
      </button>
      <div v-if="showEnded" class="card mt-2 overflow-x-auto">
        <table class="table">
          <thead><tr><th>Server</th><th>Type</th><th>Start → worker</th><th>Lived</th><th>Tests</th><th>Cost</th><th>Ended</th></tr></thead>
          <tbody>
            <tr v-for="s in ended" :key="s.id">
              <td class="font-mono text-xs">{{ s.id }} <span class="ml-1" :class="serverStatus[s.status].text">{{ serverStatus[s.status].label }}</span></td>
              <td class="text-xs text-ink-300">{{ providerLabel[s.provider] }} · {{ s.type || '—' }}<template v-if="s.spot"> · spot</template></td>
              <td class="font-mono text-xs">{{ s.worker_at ? offset(s.provider_created_at, s.worker_at) : '—' }}</td>
              <td class="font-mono text-xs">{{ s.provider_created_at ? offset(s.provider_created_at, s.deleted_at) : '—' }}</td>
              <td class="font-mono text-xs">{{ s.completed_jobs || '—' }}</td>
              <td class="font-mono text-xs">{{ serverMoney(s) }}</td>
              <td class="max-w-[24rem] truncate text-xs text-ink-400" :title="s.end_reason">{{ dateTime(s.deleted_at) }} · {{ s.end_reason }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Timeline -->
    <div v-if="!notConfigured && tl" class="card mb-6">
      <div class="card-header">
        <h3 class="card-title">Last 24 hours</h3>
        <div class="flex flex-wrap gap-3 text-[11px] text-ink-400">
          <span v-for="k in ['starting', 'working', 'idle', 'draining']" :key="k" class="flex items-center gap-1"><span class="h-2 w-3 rounded-sm" :style="{ background: serverStatus[k].bar }" />{{ serverStatus[k].label }}</span>
          <span class="flex items-center gap-1"><span class="h-2 w-3 rounded-sm bg-indigo-400/50" />Queue (max {{ tl.maxQ }})</span>
        </div>
      </div>
      <div class="p-4">
        <svg :viewBox="`0 0 ${W} ${tl.H + 16 + tl.rows.length * ROW}`" class="w-full" preserveAspectRatio="none" :style="{ height: tl.H + 16 + tl.rows.length * ROW + 'px' }">
          <path v-if="tl.area" :d="tl.area" fill="rgb(129 140 248 / 0.25)" />
          <path v-if="tl.path" :d="tl.path" fill="none" stroke="rgb(129 140 248)" stroke-width="1.5" vector-effect="non-scaling-stroke" />
          <g v-for="h in tl.hours" :key="h.x">
            <line :x1="h.x" :x2="h.x" y1="0" :y2="tl.H + 16 + tl.rows.length * ROW" stroke="#222836" vector-effect="non-scaling-stroke" />
          </g>
          <g v-for="(r, i) in tl.rows" :key="r.id">
            <rect v-for="(s, j) in r.segs" :key="j" :x="s.x" :y="tl.H + 16 + i * ROW" :width="s.w" :height="ROW - 4" :fill="s.color" rx="2">
              <title>{{ r.id }} · {{ serverStatus[s.status]?.label }}</title>
            </rect>
          </g>
        </svg>
        <div class="relative mt-1 h-4 text-[10px] text-ink-500">
          <span v-for="h in tl.hours" :key="h.x" class="absolute -translate-x-1/2 font-mono" :style="{ left: (h.x / W) * 100 + '%' }">{{ h.label }}</span>
        </div>
        <p v-if="!tl.rows.length" class="mt-2 text-xs text-ink-500">No servers in the last 24 hours.</p>
      </div>
    </div>

    <div v-if="!notConfigured" class="grid gap-6 xl:grid-cols-3">
      <!-- Events -->
      <div class="card xl:col-span-2">
        <div class="card-header">
          <h3 class="card-title">Events</h3>
          <div class="flex gap-2">
            <select v-model="evServer" class="input w-36 py-1 text-xs" @change="loadEvents()">
              <option value="">All servers</option>
              <option v-for="s in data.servers" :key="s.id" :value="s.id">{{ s.id }}</option>
            </select>
            <select v-model="evKind" class="input w-36 py-1 text-xs" @change="loadEvents()">
              <option value="">All events</option>
              <option value="decision">Decisions</option>
              <option value="server">Server steps</option>
              <option value="provider">Provider</option>
              <option value="spot">Spot</option>
              <option value="settings">Settings</option>
              <option value="error">Errors</option>
            </select>
          </div>
        </div>
        <div class="max-h-[32rem] divide-y divide-ink-800 overflow-y-auto">
          <div v-for="e in events" :key="e.id" class="flex items-start gap-3 px-4 py-2 text-xs">
            <span class="shrink-0 font-mono text-ink-500" :title="dateTime(e.ts)">{{ new Date(e.ts).toLocaleTimeString() }}</span>
            <span class="w-16 shrink-0 rounded px-1.5 py-0.5 text-center text-[10px] font-medium uppercase"
              :class="{ decision: 'bg-indigo-500/15 text-indigo-300', server: 'bg-ink-700 text-ink-300', provider: 'bg-amber-500/15 text-amber-300', spot: 'bg-orange-500/15 text-orange-300', settings: 'bg-sky-500/15 text-sky-300', error: 'bg-red-500/15 text-red-300' }[e.kind]">{{ e.kind }}</span>
            <button v-if="e.server_id" class="shrink-0 font-mono text-indigo-300 hover:underline" @click="evServer = e.server_id; loadEvents()">{{ e.server_id }}</button>
            <span class="min-w-0 flex-1 break-words text-ink-200">{{ e.message }}</span>
            <button v-if="e.kind === 'settings'" class="btn btn-ghost btn-sm shrink-0" title="Restore the settings from before this change" @click="revert(e)"><Undo2 class="h-3 w-3" />Revert</button>
          </div>
          <div v-if="!events.length" class="px-4 py-6 text-center text-xs text-ink-500">No events.</div>
        </div>
        <div v-if="!eventsDone" class="border-t border-ink-800 px-4 py-2 text-center"><button class="btn btn-ghost btn-sm" @click="loadEvents(true)">Load more</button></div>
      </div>

      <!-- Costs -->
      <div class="card">
        <div class="card-header"><h3 class="card-title">Costs</h3><span class="text-xs text-ink-400">estimate, excl. VAT</span></div>
        <div v-if="costs" class="p-4">
          <div class="flex h-24 items-end gap-[2px]">
            <div v-for="d in costs.daily" :key="d.day" class="flex-1 rounded-t bg-indigo-400/70 hover:bg-indigo-300" :style="{ height: Math.max(d.cost > 0 ? 4 : 1, (d.cost / costMax) * 100) + '%' }"
              :title="`${d.day}: ${money(d.cost, costs.currency)} · ${d.servers} server(s) · ${d.hours.toFixed(1)} h`" />
          </div>
          <div class="mt-1 flex justify-between font-mono text-[10px] text-ink-500"><span>{{ costs.daily[0]?.day.slice(5) }}</span><span>today</span></div>
          <table class="mt-4 w-full text-xs">
            <tr v-for="m in costs.monthly.slice().reverse()" :key="m.month" class="border-b border-ink-800 last:border-0">
              <td class="py-1.5 text-ink-400">{{ m.month }}</td>
              <td class="py-1.5 text-right font-mono text-ink-200">{{ money(m.cost, costs.currency) }}</td>
            </tr>
          </table>
        </div>
      </div>
    </div>
  </template>

  <Modal v-if="launching" title="Start cloud server" @close="launching = null">
    <div class="space-y-3">
      <div>
        <label class="label">Number of servers</label>
        <input v-model="launching.count" type="number" min="1" :max="state.max_servers" class="input" @keyup.enter="launch" autofocus />
      </div>
      <p class="text-xs text-ink-400">Useful before a demo or a big run. The servers take tests like any worker and shut down on their own after the idle time from the settings.</p>
    </div>
    <template #footer>
      <button class="btn" @click="launching = null">Cancel</button>
      <button class="btn btn-primary" @click="launch">Start</button>
    </template>
  </Modal>

  <Modal v-if="confirmStop" title="Stop server now" @close="confirmStop = null">
    <p class="text-sm text-ink-300">Delete <b class="font-mono">{{ confirmStop.id }}</b> right away? Its {{ confirmStop.running_jobs || 'running' }} test(s) are re-queued and run again elsewhere. Use “Stop” to let them finish first.</p>
    <template #footer>
      <button class="btn" @click="confirmStop = null">Cancel</button>
      <button class="btn btn-danger" @click="stop(confirmStop, true)">Delete now</button>
    </template>
  </Modal>
</template>
