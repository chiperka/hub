<script setup>
import { onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { PlayCircle, Search } from 'lucide-vue-next'
import { api } from '../api'
import { refreshOn } from '../live'
import { ago, dateTime, duration, runDuration } from '../format'
import PageHeader from '../components/PageHeader.vue'
import StatusBadge from '../components/StatusBadge.vue'
import RunProgress from '../components/RunProgress.vue'
import EmptyState from '../components/EmptyState.vue'
import Spinner from '../components/Spinner.vue'

const route = useRoute()
const router = useRouter()
const PAGE = 50

const f = reactive({ project: route.query.project || '', status: route.query.status || '', q: route.query.q || '', page: 0 })
const runs = ref(null)
const total = ref(0)
const projects = ref([])

const statuses = [
  { v: '', label: 'All' },
  { v: 'active', label: 'Running' },
  { v: 'passed', label: 'Passed' },
  { v: 'failed', label: 'Failed' },
  { v: 'cancelled', label: 'Cancelled' },
]

async function load() {
  const p = new URLSearchParams({ limit: PAGE, offset: f.page * PAGE })
  if (f.project) p.set('project', f.project)
  if (f.status) p.set('status', f.status)
  if (f.q) p.set('q', f.q)
  const res = await api.get('/api/runs?' + p)
  runs.value = res.runs
  total.value = res.total
}

let qTimer
watch(() => [f.project, f.status], () => { f.page = 0; sync(); load() })
watch(() => f.q, () => { clearTimeout(qTimer); qTimer = setTimeout(() => { f.page = 0; sync(); load() }, 250) })
watch(() => f.page, load)

function sync() {
  const q = {}
  if (f.project) q.project = f.project
  if (f.status) q.status = f.status
  if (f.q) q.q = f.q
  router.replace({ query: q })
}

onMounted(async () => {
  load()
  projects.value = await api.get('/api/projects')
})
refreshOn('runs', load, 600)
</script>

<template>
  <PageHeader title="Runs" subtitle="Every test run submitted by the CLI or re-run from the UI." />

  <div class="mb-4 flex flex-wrap items-center gap-3">
    <div class="flex rounded-lg border border-ink-700 bg-ink-900 p-0.5">
      <button
        v-for="s in statuses" :key="s.v" @click="f.status = s.v"
        class="rounded-md px-3 py-1 text-sm font-medium transition"
        :class="f.status === s.v ? 'bg-ink-700 text-white' : 'text-ink-400 hover:text-ink-100'"
      >{{ s.label }}</button>
    </div>
    <select v-model="f.project" class="input w-auto min-w-40 py-1.5">
      <option value="">All projects</option>
      <option v-for="p in projects" :key="p.id" :value="p.slug">{{ p.slug }}</option>
    </select>
    <div class="relative ml-auto w-full sm:w-72">
      <Search class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-ink-500" />
      <input v-model="f.q" class="input py-1.5 pl-9" placeholder="Search run id, project or test name…" />
    </div>
  </div>

  <div class="card overflow-hidden">
    <Spinner v-if="!runs" />
    <EmptyState v-else-if="!runs.length" :icon="PlayCircle" title="No runs found" text="Runs appear here as soon as the CLI submits them." />
    <div v-else class="overflow-x-auto">
      <table class="table">
        <thead>
          <tr>
            <th class="w-32">Status</th>
            <th>Run</th>
            <th class="hidden lg:table-cell">Project</th>
            <th class="w-64">Progress</th>
            <th class="hidden text-right md:table-cell">Duration</th>
            <th class="hidden text-right md:table-cell">Started</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in runs" :key="r.id" class="clickable" @click="router.push(`/runs/${r.id}`)">
            <td><StatusBadge :status="r.status" /></td>
            <td>
              <div class="font-mono text-xs text-ink-100">{{ r.id }}</div>
              <div class="mt-0.5 text-[11px] text-ink-400">
                {{ r.source }}<template v-if="r.created_by"> · {{ r.created_by }}</template><template v-if="r.version"> · cli {{ r.version }}</template>
              </div>
            </td>
            <td class="hidden lg:table-cell"><span class="rounded-md bg-ink-800 px-2 py-0.5 text-xs text-ink-200">{{ r.project || '—' }}</span></td>
            <td>
              <RunProgress :run="r" />
              <div class="mt-1 flex gap-3 text-[11px] text-ink-400">
                <span class="text-emerald-300">{{ r.passed }} passed</span>
                <span v-if="r.failed + r.errored" class="text-red-300">{{ r.failed + r.errored }} failed</span>
                <span v-if="r.running + r.pending" class="text-sky-300">{{ r.running + r.pending }} left</span>
                <span class="ml-auto">{{ r.total }} total</span>
              </div>
            </td>
            <td class="hidden text-right font-mono text-xs text-ink-300 md:table-cell">{{ duration(runDuration(r)) }}</td>
            <td class="hidden text-right text-xs text-ink-400 md:table-cell" :title="dateTime(r.started_at)">{{ ago(r.started_at) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-if="total > PAGE" class="flex items-center justify-between border-t border-ink-700/70 px-4 py-2.5 text-xs text-ink-400">
      <span>{{ f.page * PAGE + 1 }}–{{ Math.min(total, (f.page + 1) * PAGE) }} of {{ total }}</span>
      <div class="flex gap-2">
        <button class="btn btn-sm" :disabled="f.page === 0" @click="f.page--">Previous</button>
        <button class="btn btn-sm" :disabled="(f.page + 1) * PAGE >= total" @click="f.page++">Next</button>
      </div>
    </div>
  </div>
</template>
