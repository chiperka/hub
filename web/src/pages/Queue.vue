<script setup>
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ListOrdered } from 'lucide-vue-next'
import { api } from '../api'
import { refreshOn } from '../live'
import { ago, duration } from '../format'
import PageHeader from '../components/PageHeader.vue'
import StatusBadge from '../components/StatusBadge.vue'
import EmptyState from '../components/EmptyState.vue'
import Spinner from '../components/Spinner.vue'

const router = useRouter()
const tab = ref('active')
const jobs = ref(null)
const counts = ref({ pending: 0, active: 0 })
const tick = ref(0)

const tabs = [
  { v: 'active', label: 'Running', status: 'claimed,running' },
  { v: 'pending', label: 'Queued', status: 'pending' },
  { v: 'done', label: 'Recently finished', status: 'completed,cancelled' },
]

async function load() {
  const cur = tabs.find((t) => t.v === tab.value)
  const [list, stats] = await Promise.all([
    api.get(`/api/jobs?status=${cur.status}&limit=300`),
    api.get('/api/overview'),
  ])
  jobs.value = list
  counts.value = { pending: stats.stats.queue_pending, active: stats.stats.queue_running }
}
watch(tab, () => { jobs.value = null; load() })
onMounted(load)
refreshOn(['queue', 'runs'], load, 500)
const timer = setInterval(() => tick.value++, 1000)
onBeforeUnmount(() => clearInterval(timer))

function age(j) {
  tick.value
  if (j.status === 'running') return duration(Date.now() - j.acked_at)
  if (j.status === 'claimed') return duration(Date.now() - j.claimed_at)
  if (j.status === 'pending') return duration(Date.now() - j.created_at)
  return duration(j.duration_ms)
}
</script>

<template>
  <PageHeader title="Queue" subtitle="Jobs waiting for, or running on, a worker. Jobs are interleaved fairly across runs." />

  <div class="mb-4 flex rounded-lg border border-ink-700 bg-ink-900 p-0.5 w-fit">
    <button
      v-for="t in tabs" :key="t.v" @click="tab = t.v"
      class="flex items-center gap-1.5 rounded-md px-3 py-1 text-sm font-medium transition"
      :class="tab === t.v ? 'bg-ink-700 text-white' : 'text-ink-400 hover:text-ink-100'"
    >
      {{ t.label }}
      <span v-if="t.v === 'active'" class="text-xs text-sky-300">{{ counts.active }}</span>
      <span v-if="t.v === 'pending'" class="text-xs text-ink-300">{{ counts.pending }}</span>
    </button>
  </div>

  <div class="card overflow-hidden">
    <Spinner v-if="!jobs" />
    <EmptyState v-else-if="!jobs.length" :icon="ListOrdered" :title="tab === 'pending' ? 'Queue is empty' : 'Nothing here'" />
    <div v-else class="overflow-x-auto">
      <table class="table">
        <thead>
          <tr>
            <th class="w-20">Job</th>
            <th class="w-28">Status</th>
            <th>Test</th>
            <th class="hidden lg:table-cell">Run</th>
            <th class="text-right">Weight</th>
            <th>Worker</th>
            <th class="text-right">{{ tab === 'done' ? 'Duration' : tab === 'pending' ? 'Waiting' : 'Running for' }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="j in jobs" :key="j.id" class="clickable" @click="router.push(`/tests/${j.test_id}`)">
            <td class="font-mono text-xs text-ink-400">#{{ j.id }}</td>
            <td><StatusBadge :status="j.result || j.status" size="sm" /></td>
            <td>
              <div class="max-w-md truncate text-sm">{{ j.test_name }}</div>
              <div v-if="j.retry_count" class="text-[11px] text-amber-300">retry {{ j.retry_count }}</div>
            </td>
            <td class="hidden lg:table-cell">
              <div class="font-mono text-xs text-ink-300">{{ j.run_id }}</div>
              <div class="text-[11px] text-ink-500">{{ j.project }}</div>
            </td>
            <td class="text-right font-mono text-xs text-ink-300">{{ j.weight }}</td>
            <td class="font-mono text-xs text-ink-300">{{ j.worker_id || '—' }}</td>
            <td class="text-right font-mono text-xs text-ink-300">{{ age(j) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
