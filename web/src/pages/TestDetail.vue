<script setup>
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ChevronRight, RotateCcw, CheckCircle2, XCircle, Download, Eye, ArrowDownToLine } from 'lucide-vue-next'
import { api } from '../api'
import { onLive, refreshOn, watchRun } from '../live'
import { ago, bytes, dateTime, duration, pretty } from '../format'
import PageHeader from '../components/PageHeader.vue'
import StatusBadge from '../components/StatusBadge.vue'
import StatusIcon from '../components/StatusIcon.vue'
import Spinner from '../components/Spinner.vue'
import CodeBlock from '../components/CodeBlock.vue'
import EmptyState from '../components/EmptyState.vue'
import { toastError } from '../components/toast'

const props = defineProps({ id: String })
const router = useRouter()
const v = ref(null)
const logs = ref([])
const tab = ref('overview')
const runId = computed(() => v.value?.test.run_id || '')

async function load() {
  try {
    v.value = await api.get(`/api/tests/${props.id}`)
  } catch (e) {
    if (e.status === 404) router.replace('/runs')
  }
}
async function loadLogs() {
  logs.value = await api.get(`/api/tests/${props.id}/logs`)
}
onMounted(() => { load(); loadLogs() })
watchRun(runId)

const testId = computed(() => Number(props.id))
let reloadTimer
function reloadSoon() { clearTimeout(reloadTimer); reloadTimer = setTimeout(load, 300) }
onLive('step_update', (ev) => ev.test_id === testId.value && reloadSoon())
onLive('artifact', (ev) => ev.test_id === testId.value && reloadSoon())
onLive('test_update', (ev) => {
  if (ev.test_id !== testId.value) return
  if (ev.status === 'running') logs.value = []
  reloadSoon()
})
onLive('test_log', (ev) => {
  if (ev.test_id !== testId.value) return
  logs.value.push(...ev.logs)
  if (followLogs.value) nextTick(scrollLogs)
})
refreshOn('run_completed', load)

const t = computed(() => v.value?.test)
const failedAssertions = computed(() => (v.value?.assertions || []).filter((a) => !a.passed))
const tabs = computed(() => [
  { v: 'overview', label: 'Overview' },
  { v: 'http', label: 'HTTP', n: v.value?.http.length },
  { v: 'cli', label: 'CLI', n: v.value?.cli.length },
  { v: 'timeline', label: 'Timeline', n: v.value?.steps.length },
  { v: 'logs', label: 'Logs', n: logs.value.length },
  { v: 'artifacts', label: 'Artifacts', n: v.value?.artifacts.length },
  { v: 'definition', label: 'Definition' },
])

// ── timeline ──
const timeline = computed(() => {
  const steps = (v.value?.steps || []).filter((s) => s.started_at)
  if (!steps.length) return null
  const start = Math.min(...steps.map((s) => s.started_at))
  const end = Math.max(...steps.map((s) => s.completed_at || Date.now()))
  const span = Math.max(1, end - start)
  return {
    span,
    rows: (v.value?.steps || []).map((s) => ({
      ...s,
      left: s.started_at ? ((s.started_at - start) / span) * 100 : 0,
      width: s.started_at ? Math.max(0.4, (((s.completed_at || Date.now()) - s.started_at) / span) * 100) : 0,
    })),
  }
})
const barColor = (s) => ({
  completed: 'bg-emerald-500/80', failed: 'bg-red-500/80', running: 'bg-sky-500/80 animate-pulse', skipped: 'bg-ink-600',
}[s] || 'bg-ink-500')

// ── logs ──
const logBox = ref(null)
const followLogs = ref(true)
const logFilter = ref('')
const filteredLogs = computed(() => {
  const q = logFilter.value.toLowerCase()
  return q ? logs.value.filter((l) => JSON.stringify(l).toLowerCase().includes(q)) : logs.value
})
function scrollLogs() { if (logBox.value) logBox.value.scrollTop = logBox.value.scrollHeight }
watch(tab, (x) => x === 'logs' && nextTick(scrollLogs))
const levelCls = (l) => ({ error: 'text-red-300', warn: 'text-amber-300', warning: 'text-amber-300', debug: 'text-ink-500' }[l] || 'text-ink-300')
const fmtRel = (ms) => (ms == null ? '' : (ms / 1000).toFixed(3).padStart(8, ' '))

// ── artifacts ──
const preview = ref(null)
async function openArtifact(a) {
  if (a.size > 2 * 1024 * 1024) { window.open(artifactUrl(a), '_blank'); return }
  const res = await fetch(artifactUrl(a))
  preview.value = { name: a.name, text: pretty(await res.text()) }
}
const artifactUrl = (a, dl) => `/api/tests/${props.id}/artifacts/${a.name.split('/').map(encodeURIComponent).join('/')}${dl ? '?download=1' : ''}`

const definition = computed(() => pretty(JSON.stringify(t.value?.definition ?? {})))

function headersText(h) {
  if (!h) return ''
  if (typeof h === 'string') return h
  return Object.entries(h).map(([k, val]) => `${k}: ${Array.isArray(val) ? val.join(', ') : val}`).join('\n')
}
const statusCls = (c) => (c >= 500 ? 'text-red-300' : c >= 400 ? 'text-amber-300' : c >= 200 ? 'text-emerald-300' : 'text-ink-300')

async function rerun() {
  try {
    const r = await api.post(`/api/tests/${props.id}/rerun`)
    router.push(`/runs/${r.id}`)
  } catch (e) { toastError(e) }
}
</script>

<template>
  <Spinner v-if="!v" />
  <template v-else>
    <PageHeader :title="t.name">
      <template #eyebrow>
        <div class="mb-1 flex items-center gap-1 text-xs text-ink-400">
          <router-link to="/runs" class="hover:text-ink-200">Runs</router-link>
          <ChevronRight class="h-3 w-3" />
          <router-link :to="`/runs/${t.run_id}`" class="font-mono hover:text-ink-200">{{ t.run_id }}</router-link>
          <ChevronRight class="h-3 w-3" />
          <span>{{ t.suite }}</span>
        </div>
      </template>
      <template #meta>
        <div class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-ink-400">
          <StatusBadge :status="t.status" />
          <span v-if="t.duration_ms">took <span class="font-mono text-ink-200">{{ duration(t.duration_ms) }}</span></span>
          <span v-if="t.worker_id">on <router-link to="/workers" class="link font-mono">{{ t.worker_id }}</router-link></span>
          <span v-if="t.started_at" :title="dateTime(t.started_at)">started {{ ago(t.started_at) }}</span>
          <span v-if="v.attempts.length > 1" class="text-amber-300">{{ v.attempts.length }} attempts</span>
          <span v-if="t.endpoint">endpoint <span class="font-mono text-ink-200">{{ t.endpoint }}</span></span>
          <span v-for="tag in t.tags" :key="tag" class="rounded bg-ink-800 px-1.5 py-0.5 text-[11px] text-ink-300">#{{ tag }}</span>
        </div>
      </template>
      <button class="btn" @click="rerun"><RotateCcw class="h-4 w-4" />Re-run test</button>
    </PageHeader>

    <div class="mb-4 flex gap-1 overflow-x-auto border-b border-ink-700/70">
      <button v-for="x in tabs" :key="x.v" class="tab whitespace-nowrap" :class="tab === x.v && 'tab-active'" @click="tab = x.v">
        {{ x.label }}<span v-if="x.n" class="ml-1.5 rounded-full bg-ink-800 px-1.5 text-[11px] text-ink-300">{{ x.n }}</span>
      </button>
    </div>

    <!-- Overview -->
    <div v-if="tab === 'overview'" class="grid gap-4 xl:grid-cols-3">
      <div class="space-y-4 xl:col-span-2">
        <div v-if="t.error_message && t.status !== 'passed'" class="rounded-xl border border-red-500/30 bg-red-500/5 p-4">
          <div class="mb-1 text-xs font-semibold uppercase tracking-wide text-red-300">{{ t.status === 'error' ? 'Error' : 'Failure' }}</div>
          <pre class="whitespace-pre-wrap break-words font-mono text-sm text-red-100">{{ t.error_message }}</pre>
        </div>

        <div class="card">
          <div class="card-header">
            <h2 class="card-title">Assertions</h2>
            <span class="text-xs text-ink-400">{{ v.assertions.length - failedAssertions.length }}/{{ v.assertions.length }} passed</span>
          </div>
          <EmptyState v-if="!v.assertions.length" title="No assertion results" :text="t.status === 'pending' || t.status === 'running' ? 'Results appear when the test finishes.' : 'The assert step did not run.'" />
          <ul v-else class="divide-y divide-ink-800">
            <li v-for="(a, i) in v.assertions" :key="i" class="px-4 py-3">
              <div class="flex items-start gap-2.5">
                <CheckCircle2 v-if="a.passed" class="mt-0.5 h-4 w-4 shrink-0 text-emerald-400" />
                <XCircle v-else class="mt-0.5 h-4 w-4 shrink-0 text-red-400" />
                <div class="min-w-0 flex-1">
                  <div class="flex items-center gap-2">
                    <span class="rounded bg-ink-800 px-1.5 py-0.5 font-mono text-[11px] text-ink-300">{{ a.type }}</span>
                    <span class="text-sm" :class="a.passed ? 'text-ink-200' : 'text-red-200'">{{ a.message }}</span>
                  </div>
                  <div v-if="!a.passed && (a.expected || a.actual)" class="mt-2 grid gap-2 md:grid-cols-2">
                    <div>
                      <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-ink-400">Expected</div>
                      <CodeBlock :code="pretty(a.expected)" max-height="16rem" />
                    </div>
                    <div>
                      <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-ink-400">Actual</div>
                      <CodeBlock :code="pretty(a.actual)" max-height="16rem" />
                    </div>
                  </div>
                </div>
              </div>
            </li>
          </ul>
        </div>

        <div v-if="v.services.length" class="card overflow-hidden">
          <div class="card-header"><h2 class="card-title">Services</h2></div>
          <div class="overflow-x-auto">
            <table class="table">
              <thead><tr><th>Service</th><th>Image</th><th class="text-right">Pull/resolve</th><th class="text-right">Start</th><th class="text-right">Healthy</th><th class="text-right">Total</th></tr></thead>
              <tbody>
                <tr v-for="s in v.services" :key="s.name">
                  <td class="font-medium">{{ s.name }}</td>
                  <td class="max-w-xs truncate font-mono text-xs text-ink-300" :title="s.image">{{ s.image }}</td>
                  <td class="text-right font-mono text-xs text-ink-400">{{ duration(s.image_resolve_ms) }}</td>
                  <td class="text-right font-mono text-xs text-ink-400">{{ duration(s.container_start_ms) }}</td>
                  <td class="text-right font-mono text-xs text-ink-400">{{ duration(s.health_check_ms) }}</td>
                  <td class="text-right font-mono text-xs text-ink-200">{{ duration(s.duration_ms) }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <div class="space-y-4">
        <div class="card p-4">
          <h2 class="card-title mb-3">Phases</h2>
          <dl class="space-y-2 text-sm">
            <div v-for="[k, label] in [['services_ms','Services'],['setup_ms','Setup'],['execution_ms','Execution'],['assertion_ms','Assertions'],['teardown_ms','Teardown'],['cleanup_ms','Cleanup']]" :key="k" class="flex justify-between">
              <dt class="text-ink-400">{{ label }}</dt><dd class="font-mono text-xs text-ink-200">{{ v.phases[k] ? duration(v.phases[k]) : '—' }}</dd>
            </div>
          </dl>
        </div>
        <div class="card">
          <div class="card-header"><h2 class="card-title">Attempts</h2></div>
          <ul class="divide-y divide-ink-800">
            <li v-for="j in v.attempts" :key="j.id" class="px-4 py-2.5 text-xs">
              <div class="flex items-center gap-2">
                <StatusBadge :status="j.result || j.status" size="sm" />
                <span class="font-mono text-ink-400">job #{{ j.id }}</span>
                <span v-if="j.retry_count" class="text-amber-300">retry {{ j.retry_count }}</span>
                <span class="ml-auto font-mono text-ink-400">{{ j.duration_ms ? duration(j.duration_ms) : '' }}</span>
              </div>
              <div class="mt-1 text-ink-400">{{ j.worker_id || 'unassigned' }} · {{ ago(j.created_at) }}</div>
              <div v-if="j.error_message" class="mt-1 line-clamp-3 font-mono text-red-300/90">{{ j.error_message }}</div>
            </li>
          </ul>
        </div>
      </div>
    </div>

    <!-- HTTP -->
    <div v-else-if="tab === 'http'" class="space-y-4">
      <EmptyState v-if="!v.http.length" title="No HTTP exchanges" />
      <div v-for="h in v.http" :key="h.step" class="card overflow-hidden">
        <div class="card-header">
          <div class="flex min-w-0 items-center gap-2">
            <span class="rounded bg-ink-800 px-1.5 py-0.5 text-[11px] uppercase text-ink-400">{{ h.phase }}{{ h.phase !== 'execution' ? ' #' + h.sequence : '' }}</span>
            <span class="font-mono text-xs font-semibold text-indigo-300">{{ h.method }}</span>
            <span class="truncate font-mono text-xs text-ink-200">{{ h.url }}</span>
          </div>
          <div class="flex shrink-0 items-center gap-3 font-mono text-xs">
            <span :class="statusCls(h.status_code)">{{ h.status_code || '—' }}</span>
            <span class="text-ink-400">{{ duration(h.duration_ms) }}</span>
          </div>
        </div>
        <div v-if="h.error" class="border-b border-ink-700/70 bg-red-500/5 px-4 py-2 font-mono text-xs text-red-300">{{ h.error }}</div>
        <div class="grid gap-4 p-4 lg:grid-cols-2">
          <div class="min-w-0 space-y-2">
            <div class="text-[11px] font-medium uppercase tracking-wide text-ink-400">Request</div>
            <CodeBlock v-if="h.request_headers" :code="headersText(h.request_headers)" max-height="10rem" />
            <CodeBlock v-if="h.request_body" :code="pretty(h.request_body)" />
            <div v-if="!h.request_headers && !h.request_body" class="text-xs text-ink-500">No headers or body.</div>
          </div>
          <div class="min-w-0 space-y-2">
            <div class="text-[11px] font-medium uppercase tracking-wide text-ink-400">Response</div>
            <CodeBlock v-if="h.response_headers" :code="headersText(h.response_headers)" max-height="10rem" />
            <CodeBlock :code="pretty(h.response_body) || '(empty body)'" />
          </div>
        </div>
      </div>
    </div>

    <!-- CLI -->
    <div v-else-if="tab === 'cli'" class="space-y-4">
      <EmptyState v-if="!v.cli.length" title="No CLI executions" />
      <div v-for="c in v.cli" :key="c.step" class="card overflow-hidden">
        <div class="card-header">
          <div class="flex min-w-0 items-center gap-2">
            <span class="rounded bg-ink-800 px-1.5 py-0.5 text-[11px] uppercase text-ink-400">{{ c.phase }}{{ c.phase !== 'execution' ? ' #' + c.sequence : '' }}</span>
            <span class="text-xs text-ink-400">{{ c.service }}</span>
            <span class="truncate font-mono text-xs text-ink-100">$ {{ c.command }}</span>
          </div>
          <div class="flex shrink-0 items-center gap-3 font-mono text-xs">
            <span :class="c.exit_code === 0 ? 'text-emerald-300' : 'text-red-300'">exit {{ c.exit_code }}</span>
            <span class="text-ink-400">{{ duration(c.duration_ms) }}</span>
          </div>
        </div>
        <div class="space-y-3 p-4">
          <div v-if="c.working_dir" class="text-xs text-ink-400">cwd <span class="font-mono text-ink-200">{{ c.working_dir }}</span></div>
          <div>
            <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-ink-400">stdout</div>
            <CodeBlock :code="c.stdout || '(empty)'" />
          </div>
          <div v-if="c.stderr">
            <div class="mb-1 text-[11px] font-medium uppercase tracking-wide text-ink-400">stderr</div>
            <CodeBlock :code="c.stderr" />
          </div>
        </div>
      </div>
    </div>

    <!-- Timeline -->
    <div v-else-if="tab === 'timeline'" class="card overflow-hidden">
      <div class="card-header">
        <h2 class="card-title">Step timeline</h2>
        <span v-if="timeline" class="font-mono text-xs text-ink-400">{{ duration(timeline.span) }}</span>
      </div>
      <EmptyState v-if="!v.steps.length" title="No steps yet" text="The job has not been picked up by a worker." />
      <div v-else class="divide-y divide-ink-800">
        <div v-for="s in (timeline ? timeline.rows : v.steps)" :key="s.name" class="grid grid-cols-[12rem_1fr_5rem] items-center gap-3 px-4 py-2">
          <div class="flex min-w-0 items-center gap-2">
            <StatusIcon :status="s.status === 'completed' ? 'passed' : s.status" />
            <div class="min-w-0">
              <div class="truncate font-mono text-xs text-ink-100">{{ s.name }}</div>
              <div class="truncate text-[11px] text-ink-500">{{ s.instruction }}{{ s.run === 'always' ? ' · always' : '' }}</div>
            </div>
          </div>
          <div class="relative h-5 rounded bg-ink-850">
            <div v-if="s.width" class="absolute top-1 h-3 rounded" :class="barColor(s.status)" :style="{ left: s.left + '%', width: s.width + '%' }" :title="s.error || ''" />
          </div>
          <div class="text-right font-mono text-xs text-ink-400">{{ s.duration_ms ? duration(s.duration_ms) : '' }}</div>
          <div v-if="s.error" class="col-span-3 -mt-1 pl-6 font-mono text-[11px] text-red-300">{{ s.error }}</div>
        </div>
      </div>
    </div>

    <!-- Logs -->
    <div v-else-if="tab === 'logs'" class="card overflow-hidden">
      <div class="card-header">
        <input v-model="logFilter" class="input max-w-xs py-1" placeholder="Filter logs…" />
        <label class="flex items-center gap-2 text-xs text-ink-400"><input v-model="followLogs" type="checkbox" class="accent-indigo-500" />Follow</label>
      </div>
      <div ref="logBox" class="max-h-[70vh] overflow-auto bg-ink-950 p-2 font-mono text-xs leading-relaxed">
        <div v-if="!filteredLogs.length" class="p-6 text-center text-ink-500">No log entries{{ t.status === 'pending' ? ' — test is queued' : '' }}.</div>
        <div v-for="(l, i) in filteredLogs" :key="i" class="flex gap-3 rounded px-2 hover:bg-ink-900">
          <span class="shrink-0 whitespace-pre text-ink-500">{{ fmtRel(l.relative_ms) }}</span>
          <span class="w-12 shrink-0 uppercase" :class="levelCls(l.level)">{{ l.level }}</span>
          <span v-if="l.service" class="shrink-0 text-violet-300">{{ l.service }}</span>
          <span v-if="l.step" class="shrink-0 text-sky-300/80">{{ l.step }}</span>
          <span v-if="l.action" class="shrink-0 text-ink-400">{{ l.action }}</span>
          <span class="min-w-0 whitespace-pre-wrap break-words text-ink-200">{{ l.message }}</span>
        </div>
      </div>
    </div>

    <!-- Artifacts -->
    <div v-else-if="tab === 'artifacts'" class="space-y-4">
      <div class="card overflow-hidden">
        <EmptyState v-if="!v.artifacts.length" title="No artifacts" text="Service logs and files declared as artifacts are uploaded here." />
        <table v-else class="table">
          <thead><tr><th>Name</th><th class="text-right">Size</th><th class="w-32"></th></tr></thead>
          <tbody>
            <tr v-for="a in v.artifacts" :key="a.name">
              <td class="font-mono text-xs">{{ a.name }}</td>
              <td class="text-right font-mono text-xs text-ink-400">{{ bytes(a.size) }}</td>
              <td class="text-right">
                <div class="flex justify-end gap-1">
                  <button class="btn btn-sm" @click="openArtifact(a)"><Eye class="h-3.5 w-3.5" />View</button>
                  <a class="btn btn-sm" :href="artifactUrl(a, true)"><ArrowDownToLine class="h-3.5 w-3.5" /></a>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="preview" class="card overflow-hidden">
        <div class="card-header"><h2 class="card-title font-mono">{{ preview.name }}</h2><button class="btn btn-sm" @click="preview = null">Close</button></div>
        <div class="p-4"><CodeBlock :code="preview.text" max-height="60vh" /></div>
      </div>
    </div>

    <!-- Definition -->
    <div v-else-if="tab === 'definition'" class="card p-4">
      <CodeBlock :code="definition" max-height="75vh" />
    </div>
  </template>
</template>
