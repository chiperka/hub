<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { Activity, CheckCircle2, ListOrdered, Server, PlayCircle, Plug } from 'lucide-vue-next'
import { api } from '../api'
import { refreshOn } from '../live'
import { ago, duration, runDuration } from '../format'
import PageHeader from '../components/PageHeader.vue'
import StatusBadge from '../components/StatusBadge.vue'
import RunProgress from '../components/RunProgress.vue'
import CapacityBar from '../components/CapacityBar.vue'
import EmptyState from '../components/EmptyState.vue'
import Spinner from '../components/Spinner.vue'

const router = useRouter()
const data = ref(null)

async function load() {
  data.value = await api.get('/api/overview')
}
onMounted(load)
refreshOn(['runs', 'workers', 'queue'], load, 800)

const s = computed(() => data.value?.stats || {})
const cards = computed(() => [
  { label: 'Runs (24h)', value: s.value.runs_24h ?? 0, sub: `${s.value.active_runs ?? 0} active`, icon: PlayCircle, tone: 'text-indigo-300' },
  {
    label: 'Pass rate (24h)', value: s.value.tests_24h ? `${Math.round((s.value.pass_rate_24h || 0) * 100)}%` : '—',
    sub: `${s.value.tests_24h ?? 0} tests · ${s.value.failed_tests_24h ?? 0} failed`, icon: CheckCircle2,
    tone: (s.value.pass_rate_24h ?? 1) >= 0.95 ? 'text-emerald-300' : 'text-amber-300',
  },
  { label: 'Queue', value: s.value.queue_pending ?? 0, sub: `${s.value.queue_running ?? 0} running`, icon: ListOrdered, tone: 'text-sky-300' },
  {
    label: 'Workers online', value: `${s.value.workers_online ?? 0}/${s.value.workers_total ?? 0}`,
    sub: `capacity ${s.value.capacity_used ?? 0}/${s.value.capacity_total ?? 0}`, icon: Server, tone: 'text-violet-300',
  },
])

const days = computed(() => {
  const byDay = Object.fromEntries((data.value?.daily || []).map((d) => [d.day, d]))
  const out = []
  for (let i = 13; i >= 0; i--) {
    const d = new Date(Date.now() - i * 86400000)
    const key = d.toLocaleDateString('sv-SE')
    const v = byDay[key] || { passed: 0, failed: 0 }
    out.push({ key, label: d.toLocaleDateString(undefined, { day: 'numeric', month: 'numeric' }), ...v, total: v.passed + v.failed })
  }
  return out
})
const maxDay = computed(() => Math.max(1, ...days.value.map((d) => d.total)))
</script>

<template>
  <PageHeader title="Dashboard" subtitle="What's happening across your projects and workers." />
  <Spinner v-if="!data" />
  <template v-else>
    <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      <div v-for="c in cards" :key="c.label" class="card p-4">
        <div class="flex items-center justify-between text-xs font-medium text-ink-400">
          {{ c.label }}
          <component :is="c.icon" class="h-4 w-4" :class="c.tone" />
        </div>
        <div class="mt-2 text-2xl font-semibold tracking-tight text-white">{{ c.value }}</div>
        <div class="mt-0.5 text-xs text-ink-400">{{ c.sub }}</div>
      </div>
    </div>

    <div class="mt-4 grid gap-4 xl:grid-cols-3">
      <div class="card xl:col-span-2">
        <div class="card-header">
          <h2 class="card-title">Recent runs</h2>
          <router-link to="/runs" class="text-xs link">View all</router-link>
        </div>
        <EmptyState v-if="!data.recent_runs.length" :icon="Plug" title="No runs yet" text="Point the chiperka CLI at this server and run `chiperka test --cloud`.">
          <router-link to="/connect" class="btn btn-primary">Connect the CLI</router-link>
        </EmptyState>
        <table v-else class="table">
          <tbody>
            <tr v-for="r in data.recent_runs" :key="r.id" class="clickable" @click="router.push(`/runs/${r.id}`)">
              <td class="w-28"><StatusBadge :status="r.status" size="sm" /></td>
              <td>
                <div class="font-mono text-xs text-ink-200">{{ r.id }}</div>
                <div class="text-xs text-ink-400">{{ r.project || 'no project' }} · {{ ago(r.started_at) }}</div>
              </td>
              <td class="hidden w-48 md:table-cell">
                <RunProgress :run="r" />
                <div class="mt-1 text-[11px] text-ink-400">{{ r.passed }}/{{ r.total }} passed</div>
              </td>
              <td class="w-20 text-right font-mono text-xs text-ink-400">{{ duration(runDuration(r)) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <div class="space-y-4">
        <div class="card">
          <div class="card-header">
            <h2 class="card-title">Tests per day</h2>
            <div class="flex items-center gap-3 text-[11px] text-ink-400">
              <span class="flex items-center gap-1"><span class="h-2 w-2 rounded-sm bg-emerald-500" />passed</span>
              <span class="flex items-center gap-1"><span class="h-2 w-2 rounded-sm bg-red-500" />failed</span>
            </div>
          </div>
          <div class="flex h-36 items-end gap-1 px-4 pb-2 pt-4">
            <div v-for="d in days" :key="d.key" class="group relative flex h-full flex-1 flex-col justify-end" :title="`${d.label}: ${d.passed} passed, ${d.failed} failed`">
              <div class="w-full rounded-t-sm bg-red-500/80" :style="{ height: (d.failed / maxDay) * 100 + '%' }" />
              <div class="w-full bg-emerald-500/80" :class="d.failed ? '' : 'rounded-t-sm'" :style="{ height: (d.passed / maxDay) * 100 + '%' }" />
              <div v-if="!d.total" class="h-px w-full bg-ink-700" />
            </div>
          </div>
          <div class="flex justify-between px-4 pb-3 text-[10px] text-ink-500">
            <span>{{ days[0].label }}</span><span>today</span>
          </div>
        </div>

        <div class="card">
          <div class="card-header">
            <h2 class="card-title">Workers</h2>
            <router-link to="/workers" class="text-xs link">Manage</router-link>
          </div>
          <EmptyState v-if="!data.workers.length" :icon="Server" title="No workers" text="Add a worker to start executing tests." />
          <ul v-else class="divide-y divide-ink-800">
            <li v-for="w in data.workers" :key="w.id" class="flex items-center gap-3 px-4 py-2.5">
              <span class="h-2 w-2 shrink-0 rounded-full" :class="w.status === 'online' ? (w.paused ? 'bg-amber-400' : 'bg-emerald-400') : 'bg-ink-500'" />
              <div class="min-w-0 flex-1">
                <div class="truncate text-sm">{{ w.name }}</div>
                <div class="truncate text-[11px] text-ink-400">{{ w.hostname || w.status }}</div>
              </div>
              <CapacityBar :used="w.used_weight" :total="w.capacity" />
            </li>
          </ul>
        </div>
      </div>
    </div>
  </template>
</template>
