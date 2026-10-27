<script setup>
import { computed, onBeforeUnmount, onMounted, ref, toRef } from 'vue'
import { useRouter } from 'vue-router'
import { Square, RotateCcw, Download, Trash2, Search, ChevronRight, FileText, FileCode2, Archive, FolderOpen } from 'lucide-vue-next'
import { api } from '../api'
import { onLive, refreshOn, watchRun } from '../live'
import { ago, dateTime, duration, runDuration } from '../format'
import PageHeader from '../components/PageHeader.vue'
import StatusBadge from '../components/StatusBadge.vue'
import StatusIcon from '../components/StatusIcon.vue'
import RunProgress from '../components/RunProgress.vue'
import Spinner from '../components/Spinner.vue'
import Modal from '../components/Modal.vue'
import { toast, toastError } from '../components/toast'

const props = defineProps({ id: String })
const router = useRouter()
const data = ref(null)
const filter = ref('all')
const q = ref('')
const confirmDelete = ref(false)
const tick = ref(0)

async function load() {
  try {
    data.value = await api.get(`/api/runs/${props.id}`)
  } catch (e) {
    if (e.status === 404) router.replace('/runs')
  }
}
onMounted(load)
watchRun(toRef(props, 'id'))

// Live: patch test rows in place, refresh the run header from run_update.
onLive('test_update', (ev) => {
  const t = data.value?.tests.find((t) => t.id === ev.test_id)
  if (!t) return
  t.status = ev.status
  if (ev.duration != null) t.duration_ms = ev.duration
  if (ev.message != null) t.error_message = ev.message
  if (ev.worker_id) t.worker_id = ev.worker_id
  if (ev.status === 'running') t.started_at = Date.now()
})
onLive('run_update', (run) => { if (data.value && run?.id === props.id) data.value.run = run })
refreshOn(['run_completed', 'run_cancelled'], load, 200)
const timer = setInterval(() => tick.value++, 1000)
onBeforeUnmount(() => clearInterval(timer))

const run = computed(() => data.value?.run)
const tests = computed(() => data.value?.tests || [])
const counts = computed(() => {
  const c = { all: tests.value.length, failed: 0, passed: 0, running: 0, pending: 0, other: 0 }
  for (const t of tests.value) {
    if (t.status === 'failed' || t.status === 'error') c.failed++
    else if (t.status === 'passed') c.passed++
    else if (t.status === 'running') c.running++
    else if (t.status === 'pending') c.pending++
    else c.other++
  }
  return c
})
const tabs = computed(() => [
  { v: 'all', label: 'All', n: counts.value.all },
  { v: 'failed', label: 'Failed', n: counts.value.failed, cls: 'text-red-300' },
  { v: 'running', label: 'Running', n: counts.value.running + counts.value.pending, cls: 'text-sky-300' },
  { v: 'passed', label: 'Passed', n: counts.value.passed, cls: 'text-emerald-300' },
  { v: 'other', label: 'Skipped', n: counts.value.other },
])

function matches(t) {
  if (q.value && !t.name.toLowerCase().includes(q.value.toLowerCase()) && !t.suite.toLowerCase().includes(q.value.toLowerCase())) return false
  switch (filter.value) {
    case 'failed': return t.status === 'failed' || t.status === 'error'
    case 'running': return t.status === 'running' || t.status === 'pending'
    case 'passed': return t.status === 'passed'
    case 'other': return ['skipped', 'cancelled'].includes(t.status)
  }
  return true
}

const order = { failed: 0, error: 1, running: 2, pending: 3, cancelled: 4, skipped: 5, passed: 6 }
const groups = computed(() => {
  const by = new Map()
  for (const t of tests.value) {
    if (!matches(t)) continue
    if (!by.has(t.suite_id)) by.set(t.suite_id, { id: t.suite_id, name: t.suite, file: t.suite_file, tests: [] })
    by.get(t.suite_id).tests.push(t)
  }
  const gs = [...by.values()]
  for (const g of gs) {
    g.tests.sort((a, b) => (order[a.status] ?? 9) - (order[b.status] ?? 9) || a.id - b.id)
    g.worst = Math.min(...g.tests.map((t) => order[t.status] ?? 9))
  }
  return gs.sort((a, b) => a.worst - b.worst || a.id - b.id)
})

function testDuration(t) {
  tick.value
  if (t.status === 'running' && t.started_at) return Date.now() - t.started_at
  return t.duration_ms || null
}

async function stop() {
  try { await api.post(`/api/runs/${props.id}/stop`); toast('Run stopped') } catch (e) { toastError(e) }
}
async function rerun(failedOnly) {
  try {
    const r = await api.post(`/api/runs/${props.id}/rerun`, { failed_only: failedOnly })
    router.push(`/runs/${r.id}`)
  } catch (e) { toastError(e) }
}
async function del() {
  try { await api.del(`/api/runs/${props.id}`); toast('Run deleted'); router.push('/runs') } catch (e) { toastError(e) }
}
</script>

<template>
  <Spinner v-if="!run" />
  <template v-else>
    <PageHeader>
      <template #eyebrow>
        <div class="mb-1 flex items-center gap-1 text-xs text-ink-400">
          <router-link to="/runs" class="hover:text-ink-200">Runs</router-link>
          <ChevronRight class="h-3 w-3" />
          <router-link v-if="run.project" :to="`/runs?project=${run.project}`" class="hover:text-ink-200">{{ run.project }}</router-link>
        </div>
      </template>
      <template #title><span class="font-mono">{{ run.id }}</span></template>
      <template #meta>
        <div class="mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-ink-400">
          <StatusBadge :status="run.status" />
          <span :title="dateTime(run.started_at)">started {{ ago(run.started_at) }}</span>
          <span>duration <span class="font-mono text-ink-200">{{ (tick, duration(runDuration(run))) }}</span></span>
          <span v-if="run.created_by">by {{ run.created_by }}</span>
          <span v-if="run.version">cli {{ run.version }}</span>
          <span v-if="run.rerun_of">re-run of <router-link class="link font-mono" :to="`/runs/${run.rerun_of}`">{{ run.rerun_of }}</router-link></span>
          <span v-if="!run.snapshots_ready" class="text-amber-300">waiting for snapshot upload…</span>
        </div>
      </template>
      <button v-if="!run.completed_at" class="btn btn-danger" @click="stop"><Square class="h-4 w-4" />Stop</button>
      <template v-else>
        <button v-if="run.failed + run.errored + run.cancelled > 0" class="btn btn-primary" @click="rerun(true)"><RotateCcw class="h-4 w-4" />Re-run failed</button>
        <button class="btn" @click="rerun(false)"><RotateCcw class="h-4 w-4" />Re-run all</button>
      </template>
      <div class="group relative">
        <button class="btn"><Download class="h-4 w-4" />Reports</button>
        <div class="invisible absolute right-0 z-10 mt-1 w-52 rounded-lg border border-ink-700 bg-ink-850 p-1 opacity-0 shadow-xl transition group-focus-within:visible group-focus-within:opacity-100 group-hover:visible group-hover:opacity-100">
          <a :href="`/api/runs/${run.id}/report.html`" target="_blank" class="flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-ink-700"><FileText class="h-4 w-4 text-ink-400" />HTML report</a>
          <a :href="`/api/runs/${run.id}/report.xml`" class="flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-ink-700"><FileCode2 class="h-4 w-4 text-ink-400" />JUnit XML</a>
          <a :href="`/api/runs/${run.id}/artifacts.zip`" class="flex items-center gap-2 rounded-md px-3 py-2 text-sm hover:bg-ink-700"><Archive class="h-4 w-4 text-ink-400" />Artifacts (zip)</a>
        </div>
      </div>
      <button class="btn btn-ghost" title="Delete run" @click="confirmDelete = true"><Trash2 class="h-4 w-4" /></button>
    </PageHeader>

    <div class="card mb-4 p-4">
      <div class="grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-6">
        <div><div class="text-xs text-ink-400">Total</div><div class="text-xl font-semibold">{{ run.total }}</div></div>
        <div><div class="text-xs text-ink-400">Passed</div><div class="text-xl font-semibold text-emerald-300">{{ run.passed }}</div></div>
        <div><div class="text-xs text-ink-400">Failed</div><div class="text-xl font-semibold text-red-300">{{ run.failed }}</div></div>
        <div><div class="text-xs text-ink-400">Errors</div><div class="text-xl font-semibold text-amber-300">{{ run.errored }}</div></div>
        <div><div class="text-xs text-ink-400">Running / queued</div><div class="text-xl font-semibold text-sky-300">{{ run.running }} / {{ run.pending }}</div></div>
        <div><div class="text-xs text-ink-400">Skipped</div><div class="text-xl font-semibold text-ink-300">{{ run.skipped + run.cancelled }}</div></div>
      </div>
      <RunProgress :run="run" height="h-2" class="mt-4" />
    </div>

    <div class="mb-3 flex flex-wrap items-center gap-3">
      <div class="flex rounded-lg border border-ink-700 bg-ink-900 p-0.5">
        <button
          v-for="t in tabs" :key="t.v" @click="filter = t.v"
          class="flex items-center gap-1.5 rounded-md px-3 py-1 text-sm font-medium transition"
          :class="filter === t.v ? 'bg-ink-700 text-white' : 'text-ink-400 hover:text-ink-100'"
        >{{ t.label }}<span class="text-xs" :class="t.n ? t.cls : 'text-ink-500'">{{ t.n }}</span></button>
      </div>
      <div class="relative ml-auto w-full sm:w-72">
        <Search class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-ink-500" />
        <input v-model="q" class="input py-1.5 pl-9" placeholder="Filter tests…" />
      </div>
    </div>

    <div class="space-y-3">
      <div v-for="g in groups" :key="g.id" class="card overflow-hidden">
        <div class="flex items-center gap-2 border-b border-ink-700/70 bg-ink-850/50 px-4 py-2">
          <FolderOpen class="h-4 w-4 text-ink-500" />
          <span class="text-sm font-medium">{{ g.name }}</span>
          <span class="truncate font-mono text-[11px] text-ink-500">{{ g.file }}</span>
          <span class="ml-auto text-xs text-ink-400">{{ g.tests.length }}</span>
        </div>
        <router-link
          v-for="t in g.tests" :key="t.id" :to="`/tests/${t.id}`"
          class="flex items-start gap-3 border-b border-ink-800 px-4 py-2.5 transition last:border-b-0 hover:bg-ink-850"
        >
          <StatusIcon :status="t.status" class="mt-0.5" />
          <div class="min-w-0 flex-1">
            <div class="truncate text-sm text-ink-100">{{ t.name }}</div>
            <div v-if="t.error_message && t.status !== 'passed'" class="mt-0.5 line-clamp-2 font-mono text-xs text-red-300/90">{{ t.error_message }}</div>
          </div>
          <span v-if="t.worker_id" class="hidden shrink-0 rounded bg-ink-800 px-1.5 py-0.5 font-mono text-[10px] text-ink-400 md:inline">{{ t.worker_id }}</span>
          <span class="w-16 shrink-0 text-right font-mono text-xs text-ink-400">{{ duration(testDuration(t)) }}</span>
        </router-link>
      </div>
      <div v-if="!groups.length" class="card py-10 text-center text-sm text-ink-400">No tests match the filter.</div>
    </div>

    <Modal v-if="confirmDelete" title="Delete run" @close="confirmDelete = false">
      <p class="text-sm text-ink-300">Delete run <span class="font-mono">{{ run.id }}</span> with all results, logs and artifacts? This cannot be undone.</p>
      <template #footer>
        <button class="btn" @click="confirmDelete = false">Cancel</button>
        <button class="btn btn-danger" @click="del">Delete</button>
      </template>
    </Modal>
  </template>
</template>
