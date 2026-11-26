<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import { ArrowLeft, Plus, Trash2, Wifi, Save, RotateCcw, Undo2, Locate, CheckCircle2, XCircle, Loader2, ArrowUp, ArrowDown } from 'lucide-vue-next'
import { api } from '../api'
import { refreshOn } from '../live'
import { dateTime, duration } from '../format'
import { money, providerLabel, estimateRun } from '../cloud'
import PageHeader from '../components/PageHeader.vue'
import Spinner from '../components/Spinner.vue'
import CopyButton from '../components/CopyButton.vue'
import { toast, toastError } from '../components/toast'

const saved = ref(null)    // settings as stored
const version = ref('')    // of the stored settings, sent back on save
const form = ref(null)     // editable draft (times in minutes / hours)
const presets = ref([])
const state = ref(null)
const est = ref(null)
const errors = ref([])
const saving = ref(false)
const tests = ref({})      // provider -> { busy, ok, error }
const history = ref([])
const newType = ref('')
const newLocation = ref('')

// Seconds in storage, friendlier units in the form.
const units = {
  scale_up_delay_sec: 1, idle_timeout_sec: 60, hetzner_hour_reserve_sec: 60,
  max_lifetime_sec: 3600, drain_timeout_sec: 60, start_timeout_sec: 60,
}
function toForm(s) {
  const f = JSON.parse(JSON.stringify(s))
  for (const [k, div] of Object.entries(units)) f[k] = +(s[k] / div).toFixed(4)
  return f
}
function fromForm(f) {
  const s = JSON.parse(JSON.stringify(f))
  for (const [k, div] of Object.entries(units)) s[k] = Math.round(Number(f[k]) * div)
  for (const p of ['hetzner', 'aws']) {
    s[p].rules = s[p].rules.map((r) => ({ min_queue: Number(r.min_queue), servers: Number(r.servers) }))
    s[p].max_servers = Number(s[p].max_servers)
  }
  s.warm_pool = s.warm_pool.map((w) => ({ ...w, servers: Number(w.servers) }))
  for (const k of ['monthly_budget', 'disk_gb', 'worker_concurrency']) s[k] = Number(s[k])
  return s
}

async function load() {
  const r = await api.get('/api/cloud/settings')
  saved.value = r.settings
  version.value = r.version
  presets.value = r.presets
  form.value = toForm(r.settings)
  errors.value = []
}
async function loadSide() {
  state.value = (await api.get('/api/cloud')).state
  history.value = await api.get('/api/cloud/events?kind=settings&limit=20')
}
async function loadEstimate() { est.value = await api.get('/api/cloud/estimate') }
onMounted(() => { load(); loadSide(); loadEstimate() })
refreshOn(['cloud'], loadSide, 1000)

const dirty = computed(() => form.value && saved.value && JSON.stringify(fromForm(form.value)) !== JSON.stringify(saved.value))
const ps = computed(() => form.value?.[form.value.provider])
const currency = computed(() => (form.value?.provider === 'aws' ? 'USD' : 'EUR'))

async function save() {
  saving.value = true
  errors.value = []
  try {
    const r = await api.put('/api/cloud/settings', fromForm(form.value), { 'If-Match': version.value })
    saved.value = r.settings
    version.value = r.version
    form.value = toForm(r.settings)
    toast('Saved: applies within 5 seconds')
    loadSide()
    loadEstimate()
  } catch (e) {
    errors.value = e.status === 422 ? (e.body?.errors || [e.message]) : [e.message]
    if (e.status !== 422 && e.status !== 409) toastError(e)
  } finally {
    saving.value = false
  }
}
function reset() { form.value = toForm(saved.value); errors.value = [] }

async function applyPreset(name) {
  try {
    await api.post('/api/cloud/settings/preset', { name })
    await load()
    toast(`Preset “${name}” applied`)
    loadSide()
  } catch (e) { toastError(e) }
}
async function revert(ev) {
  try { await api.post(`/api/cloud/settings/revert/${ev.id}`); await load(); loadSide(); toast('Reverted') } catch (e) { toastError(e) }
}
async function testConn(p) {
  tests.value[p] = { busy: true }
  try {
    const r = await api.post(`/api/cloud/test/${p}`)
    tests.value[p] = r.ok ? { ok: true } : { error: r.error }
  } catch (e) { tests.value[p] = { error: e.message } }
}
async function detectIP() {
  try { const r = await api.post('/api/cloud/detect-ip'); form.value.ssh_allow_cidr = r.ip + '/32'; toast(`Detected ${r.ip}`) } catch (e) { toastError(e) }
}

function addRule() {
  const last = ps.value.rules[ps.value.rules.length - 1]
  ps.value.rules.push({ min_queue: last ? Number(last.min_queue) * 2 || 40 : 40, servers: last ? Number(last.servers) + 2 : 2 })
}
function addWarm() { form.value.warm_pool.push({ days: [1, 2, 3, 4, 5], from: '08:00', to: '17:00', servers: 1 }) }
function toggleDay(w, d) { w.days = w.days.includes(d) ? w.days.filter((x) => x !== d) : [...w.days, d].sort() }
function move(list, i, dir) { const j = i + dir; if (j < 0 || j >= list.length) return; [list[i], list[j]] = [list[j], list[i]] }
function addTo(list, v) { v = v.trim(); if (v && !list.includes(v)) list.push(v) }

// ── impact estimate ─────────────────────────────────────────────────────────
function price(type) { return est.value?.prices?.[form.value.provider]?.[type] }
const estimates = computed(() => {
  const e = est.value
  const f = form.value
  if (!e || !f || !e.test_time_ms) return []
  const p = f.provider
  const hourly = price(ps.value.types[0])
  const counts = [...new Set([...ps.value.rules.map((r) => Number(r.servers)), Number(ps.value.max_servers)].filter((n) => n > 0))].sort((a, b) => a - b)
  return counts.map((n) => {
    const r = estimateRun({ provider: p, servers: n, concurrency: Number(f.worker_concurrency), testTimeMs: e.test_time_ms, longestMs: e.longest_test_ms, startupMs: e.startup_ms[p], hourly })
    return { servers: n, ...r, monthly: r.cost != null ? r.cost * 40 : null }
  })
})
const dayNames = ['Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa', 'Su']
</script>

<template>
  <PageHeader title="Cloud settings" subtitle="Changes apply within 5 seconds, without a restart. Every change is logged and can be reverted.">
    <template #eyebrow><router-link to="/cloud" class="mb-1 inline-flex items-center gap-1 text-xs text-ink-400 hover:text-ink-200"><ArrowLeft class="h-3 w-3" />Cloud</router-link></template>
    <button class="btn" :disabled="!dirty" @click="reset"><RotateCcw class="h-4 w-4" />Discard</button>
    <button class="btn btn-primary" :disabled="!dirty || saving" @click="save"><Save class="h-4 w-4" />Save</button>
  </PageHeader>

  <Spinner v-if="!form" />
  <div v-else class="grid gap-6 xl:grid-cols-[1fr_22rem]">
    <div class="space-y-6">
      <div v-if="errors.length" class="rounded-lg border border-red-500/30 bg-red-500/5 px-4 py-3 text-sm text-red-200">
        <div class="mb-1 font-semibold">The settings were not saved:</div>
        <ul class="list-inside list-disc space-y-0.5 text-xs"><li v-for="e in errors" :key="e">{{ e }}</li></ul>
      </div>

      <!-- Presets -->
      <div class="card p-4">
        <div class="mb-3 text-sm font-semibold">Presets</div>
        <div class="grid gap-2 sm:grid-cols-3">
          <button v-for="p in presets" :key="p" class="rounded-lg border border-ink-700 bg-ink-850 p-3 text-left transition hover:border-indigo-500" @click="applyPreset(p)">
            <div class="text-sm font-semibold capitalize">{{ p }}</div>
            <div class="text-xs text-ink-400">{{ { economical: 'fewer servers, longer runs', balanced: 'the defaults', fast: 'more servers, shortest runs' }[p] }}</div>
          </button>
        </div>
        <p class="mt-2 text-xs text-ink-500">A preset fills the rules, server cap and idle time at once (and saves). You can fine-tune afterwards.</p>
      </div>

      <!-- General -->
      <section class="card">
        <div class="card-header"><h3 class="card-title">General</h3></div>
        <div class="grid gap-4 p-4 sm:grid-cols-2">
          <label class="flex items-start gap-3 sm:col-span-2">
            <input v-model="form.enabled" type="checkbox" class="mt-0.5 h-4 w-4 accent-indigo-500" />
            <span><span class="text-sm font-medium">Autoscaler enabled</span><br /><span class="text-xs text-ink-400">Off = no new servers; running ones finish and shut down as usual. Also the emergency brake.</span></span>
          </label>
          <div>
            <label class="label">Provider</label>
            <select v-model="form.provider" class="input">
              <option value="hetzner">Hetzner Cloud</option>
              <option value="aws">AWS EC2</option>
            </select>
            <p class="mt-1 text-xs text-ink-500">Applies to new servers; running ones live out their time.</p>
          </div>
          <div>
            <label class="label">Monthly budget ({{ currency }})</label>
            <input v-model="form.monthly_budget" type="number" min="0" step="1" class="input" />
            <p class="mt-1 text-xs text-ink-500">When the estimated spend reaches it, no new servers start. 0 = no limit.</p>
          </div>
          <div class="sm:col-span-2">
            <label class="label">Provider credentials</label>
            <div class="flex flex-wrap gap-2">
              <div v-for="p in ['hetzner', 'aws']" :key="p" class="flex items-center gap-2 rounded-lg border border-ink-700 bg-ink-850 px-3 py-2 text-xs">
                <span class="font-medium">{{ providerLabel[p] }}</span>
                <span :class="state?.configured[p] ? 'text-emerald-300' : 'text-ink-500'">{{ state?.configured[p] ? 'set' : 'not set' }}</span>
                <button v-if="state?.configured[p]" class="btn btn-ghost btn-sm" @click="testConn(p)"><Wifi class="h-3 w-3" />Test connection</button>
                <Loader2 v-if="tests[p]?.busy" class="h-3.5 w-3.5 animate-spin text-ink-400" />
                <CheckCircle2 v-else-if="tests[p]?.ok" class="h-3.5 w-3.5 text-emerald-400" />
                <span v-else-if="tests[p]?.error" class="flex items-center gap-1 text-red-300"><XCircle class="h-3.5 w-3.5" />{{ tests[p].error }}</span>
              </div>
            </div>
            <p class="mt-1 text-xs text-ink-500">Tokens stay in the hub's server configuration (CHIPERKA_CLOUD_HETZNER_TOKEN, CHIPERKA_CLOUD_AWS_*), never in the UI.</p>
          </div>
        </div>
      </section>

      <!-- When to start -->
      <section class="card">
        <div class="card-header"><h3 class="card-title">When to start servers <span class="font-normal text-ink-400">· {{ providerLabel[form.provider] }}</span></h3></div>
        <div class="space-y-4 p-4">
          <div>
            <label class="label">Rules by queue length</label>
            <div class="space-y-2">
              <div v-for="(r, i) in ps.rules" :key="i" class="flex items-center gap-2 text-sm">
                <span class="text-ink-400">at least</span>
                <input v-model="r.min_queue" type="number" min="0" class="input w-24" />
                <span class="text-ink-400">tests waiting →</span>
                <input v-model="r.servers" type="number" min="0" class="input w-20" />
                <span class="text-ink-400">servers</span>
                <button class="btn btn-ghost btn-sm ml-auto" @click="ps.rules.splice(i, 1)"><Trash2 class="h-3.5 w-3.5" /></button>
              </div>
            </div>
            <button class="btn btn-ghost btn-sm mt-2" @click="addRule"><Plus class="h-3.5 w-3.5" />Add rule</button>
            <p class="mt-1 text-xs text-ink-500">Below the first rule nothing starts: small runs stay on the permanent workers. Running servers count towards the target.</p>
          </div>
          <div class="grid gap-4 sm:grid-cols-2">
            <div>
              <label class="label">Delay before starting (s)</label>
              <input v-model="form.scale_up_delay_sec" type="number" min="0" class="input" />
              <p class="mt-1 text-xs text-ink-500">How long the queue must stay above a threshold, so short spikes start nothing.</p>
            </div>
            <div>
              <label class="label">Maximum servers</label>
              <input v-model="ps.max_servers" type="number" min="0" class="input" />
              <p class="mt-1 text-xs text-ink-500">Cap including servers started by hand.</p>
            </div>
          </div>
          <div>
            <label class="label">Warm pool</label>
            <div v-for="(w, i) in form.warm_pool" :key="i" class="mb-2 flex flex-wrap items-center gap-2 text-sm">
              <div class="flex gap-0.5">
                <button v-for="(d, j) in dayNames" :key="d" class="rounded px-1.5 py-1 text-xs" :class="w.days.includes(j + 1) ? 'bg-indigo-500 text-white' : 'bg-ink-800 text-ink-400'" @click="toggleDay(w, j + 1)">{{ d }}</button>
              </div>
              <input v-model="w.from" type="time" class="input w-28" />
              <span class="text-ink-400">–</span>
              <input v-model="w.to" type="time" class="input w-28" />
              <span class="text-ink-400">keep</span>
              <input v-model="w.servers" type="number" min="0" class="input w-16" />
              <span class="text-ink-400">server(s)</span>
              <button class="btn btn-ghost btn-sm ml-auto" @click="form.warm_pool.splice(i, 1)"><Trash2 class="h-3.5 w-3.5" /></button>
            </div>
            <button class="btn btn-ghost btn-sm" @click="addWarm"><Plus class="h-3.5 w-3.5" />Add time window</button>
            <p class="mt-1 text-xs text-ink-500">Optional: keep servers ready in working hours so the first run does not wait for start-up. Off by default.</p>
          </div>
        </div>
      </section>

      <!-- When to stop -->
      <section class="card">
        <div class="card-header"><h3 class="card-title">When to stop servers</h3></div>
        <div class="grid gap-4 p-4 sm:grid-cols-2">
          <div>
            <label class="label">Idle time before shutdown (min)</label>
            <input v-model="form.idle_timeout_sec" type="number" min="0" step="0.5" class="input" />
          </div>
          <div>
            <label class="label">Maximum server lifetime (h)</label>
            <input v-model="form.max_lifetime_sec" type="number" min="0" step="0.5" class="input" />
            <p class="mt-1 text-xs text-ink-500">Then the server shuts down gracefully, even when busy.</p>
          </div>
          <label class="flex items-start gap-3 sm:col-span-2">
            <input v-model="form.hetzner_hold_hour" type="checkbox" class="mt-0.5 h-4 w-4 accent-indigo-500" />
            <span><span class="text-sm font-medium">Hetzner: keep until the end of the paid hour</span><br /><span class="text-xs text-ink-400">Hetzner bills every started hour; an idle server stays until the hour it is paid for runs out, so another run can use it for free.</span></span>
          </label>
          <div>
            <label class="label">Hetzner: stop this long before the hour ends (min)</label>
            <input v-model="form.hetzner_hour_reserve_sec" type="number" min="0" max="59" class="input" :disabled="!form.hetzner_hold_hour" />
          </div>
          <div>
            <label class="label">Wait for running tests (min)</label>
            <input v-model="form.drain_timeout_sec" type="number" min="0" class="input" />
            <p class="mt-1 text-xs text-ink-500">When shutting down; tests still running then are re-queued.</p>
          </div>
          <div>
            <label class="label">Wait for start (min)</label>
            <input v-model="form.start_timeout_sec" type="number" min="0" class="input" />
            <p class="mt-1 text-xs text-ink-500">A server whose worker does not sign in by then is deleted as broken.</p>
          </div>
        </div>
      </section>

      <!-- Servers -->
      <section class="card">
        <div class="card-header"><h3 class="card-title">Servers <span class="font-normal text-ink-400">· {{ providerLabel[form.provider] }}</span></h3></div>
        <div class="grid gap-4 p-4 sm:grid-cols-2">
          <div>
            <label class="label">Server type, then fallbacks</label>
            <div v-for="(t, i) in ps.types" :key="t" class="mb-1 flex items-center gap-2 rounded-lg bg-ink-850 px-3 py-1.5 text-sm">
              <span class="font-mono">{{ t }}</span>
              <span class="text-xs text-ink-400">{{ price(t) != null ? money(price(t), currency) + '/h' : '' }}</span>
              <span class="ml-auto flex gap-0.5">
                <button class="btn-ghost rounded p-1" @click="move(ps.types, i, -1)"><ArrowUp class="h-3 w-3" /></button>
                <button class="btn-ghost rounded p-1" @click="move(ps.types, i, 1)"><ArrowDown class="h-3 w-3" /></button>
                <button class="btn-ghost rounded p-1" @click="ps.types.splice(i, 1)"><Trash2 class="h-3 w-3" /></button>
              </span>
            </div>
            <form class="mt-1 flex gap-2" @submit.prevent="addTo(ps.types, newType); newType = ''">
              <input v-model="newType" class="input py-1 text-xs" :placeholder="form.provider === 'aws' ? 'e.g. c7a.4xlarge' : 'e.g. cpx62'" />
              <button class="btn btn-sm">Add</button>
            </form>
            <p class="mt-1 text-xs text-ink-500">The next type is tried when the first is not available. Checked against the provider on save.</p>
          </div>
          <div>
            <label class="label">{{ form.provider === 'aws' ? 'Availability zones' : 'Locations' }}</label>
            <div v-for="(l, i) in ps.locations" :key="l" class="mb-1 flex items-center gap-2 rounded-lg bg-ink-850 px-3 py-1.5 text-sm">
              <span class="font-mono">{{ l }}</span>
              <span class="ml-auto flex gap-0.5">
                <button class="btn-ghost rounded p-1" @click="move(ps.locations, i, -1)"><ArrowUp class="h-3 w-3" /></button>
                <button class="btn-ghost rounded p-1" @click="move(ps.locations, i, 1)"><ArrowDown class="h-3 w-3" /></button>
                <button class="btn-ghost rounded p-1" @click="ps.locations.splice(i, 1)"><Trash2 class="h-3 w-3" /></button>
              </span>
            </div>
            <form class="mt-1 flex gap-2" @submit.prevent="addTo(ps.locations, newLocation); newLocation = ''">
              <input v-model="newLocation" class="input py-1 text-xs" :placeholder="form.provider === 'aws' ? 'e.g. eu-central-1a' : 'e.g. nbg1'" />
              <button class="btn btn-sm">Add</button>
            </form>
          </div>
          <template v-if="form.provider === 'aws'">
            <label class="flex items-start gap-3">
              <input v-model="form.aws_spot" type="checkbox" class="mt-0.5 h-4 w-4 accent-indigo-500" />
              <span><span class="text-sm font-medium">Use spot servers</span><br /><span class="text-xs text-ink-400">Much cheaper, but AWS can take them back (tests are re-queued).</span></span>
            </label>
            <label class="flex items-start gap-3">
              <input v-model="form.aws_spot_to_on_demand" type="checkbox" class="mt-0.5 h-4 w-4 accent-indigo-500" :disabled="!form.aws_spot" />
              <span><span class="text-sm font-medium">Fall back to on-demand</span><br /><span class="text-xs text-ink-400">When spot capacity is missing; otherwise wait for spot.</span></span>
            </label>
          </template>
          <div>
            <label class="label">Disk size (GB)</label>
            <input v-model="form.disk_gb" type="number" min="20" class="input" />
            <p v-if="form.provider === 'hetzner'" class="mt-1 text-xs text-ink-500">Hetzner disks come with the server type; this applies to AWS.</p>
          </div>
        </div>
      </section>

      <!-- Worker -->
      <section class="card">
        <div class="card-header"><h3 class="card-title">Worker</h3></div>
        <div class="grid gap-4 p-4 sm:grid-cols-2">
          <div>
            <label class="label">Parallel tests per server</label>
            <input v-model="form.worker_concurrency" type="number" min="1" class="input" />
            <p class="mt-1 text-xs text-ink-500">Applies to newly started servers.</p>
          </div>
          <div>
            <label class="label">Worker image</label>
            <input v-model="form.worker_image" class="input font-mono text-xs" placeholder="default: the hub's CHIPERKA_WORKER_IMAGE" />
            <p class="mt-1 text-xs text-ink-500">Pin a version, e.g. when a new release misbehaves.</p>
          </div>
          <div class="sm:col-span-2">
            <label class="label">Server image ID</label>
            <input v-model="form.server_image" class="input font-mono text-xs" placeholder="default: the newest prepared image (stock Ubuntu when there is none)" />
          </div>
        </div>
      </section>

      <!-- Network -->
      <section class="card">
        <div class="card-header"><h3 class="card-title">Network</h3></div>
        <div class="space-y-4 p-4">
          <div>
            <label class="label">IP address SSH may come from</label>
            <div class="flex gap-2">
              <input v-model="form.ssh_allow_cidr" class="input font-mono" placeholder="203.0.113.7/32" />
              <button class="btn" @click="detectIP"><Locate class="h-4 w-4" />Detect</button>
            </div>
            <p class="mt-1 text-xs text-ink-500">The hub's public outbound address. The provider's firewall lets SSH in only from here; the hub opens a tunnel and nothing opens on the hub.</p>
          </div>
          <div v-if="state">
            <label class="label">Hub SSH key</label>
            <div class="flex gap-2">
              <input class="input font-mono text-xs" :value="state.hub_public_key" readonly />
              <CopyButton :text="state.hub_public_key" label="Copy" />
            </div>
            <p class="mt-1 text-xs text-ink-500">Cloud servers accept only this key, and only for the tunnel (no shell).</p>
          </div>
        </div>
      </section>
    </div>

    <!-- Side: impact + history -->
    <aside class="space-y-6">
      <div class="card sticky top-4">
        <div class="card-header"><h3 class="card-title">What this means for a full run</h3></div>
        <div class="p-4 text-sm">
          <template v-if="estimates.length">
            <div v-for="e in estimates" :key="e.servers" class="flex items-baseline justify-between border-b border-ink-800 py-2 last:border-0">
              <span class="text-ink-300">{{ e.servers }} server{{ e.servers > 1 ? 's' : '' }}</span>
              <span class="text-right font-mono text-xs">
                ~{{ duration(e.runMs) }}
                <template v-if="e.cost != null"> · ~{{ money(e.cost, currency) }}/run<br /><span class="text-ink-500">~{{ money(e.monthly, currency) }}/month at 40 runs</span></template>
              </span>
            </div>
            <p class="mt-3 text-xs text-ink-500">
              From the last full run ({{ est.tests }} tests, {{ duration(est.test_time_ms) }} of test time, longest {{ duration(est.longest_test_ms) }}),
              start-up {{ duration(est.startup_ms[form.provider]) }}{{ est.startup_measured[form.provider] ? ' (measured)' : ' (estimate)' }}<template v-if="price(ps.types[0]) == null">; price unknown until the provider is configured</template>.
              <template v-if="form.provider === 'hetzner'"> Hetzner bills started hours; runs within the same hour share them.</template>
            </p>
          </template>
          <p v-else class="text-xs text-ink-500">No finished run yet to estimate from.</p>
        </div>
      </div>

      <div class="card">
        <div class="card-header"><h3 class="card-title">History</h3></div>
        <div class="max-h-[28rem] divide-y divide-ink-800 overflow-y-auto">
          <div v-for="e in history" :key="e.id" class="px-4 py-2 text-xs">
            <div class="flex items-center gap-2">
              <span class="font-mono text-ink-500">{{ dateTime(e.ts) }}</span>
              <button class="btn btn-ghost btn-sm ml-auto" @click="revert(e)"><Undo2 class="h-3 w-3" />Revert</button>
            </div>
            <div class="mt-0.5 break-words text-ink-300">{{ e.message }}</div>
          </div>
          <div v-if="!history.length" class="px-4 py-6 text-center text-xs text-ink-500">No changes yet.</div>
        </div>
      </div>
    </aside>
  </div>
</template>
