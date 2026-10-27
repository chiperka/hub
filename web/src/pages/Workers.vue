<script setup>
import { onMounted, ref } from 'vue'
import { Plus, Server, Pause, Play, KeyRound, Trash2, Pencil, Cpu, Container } from 'lucide-vue-next'
import { api } from '../api'
import { refreshOn } from '../live'
import { ago } from '../format'
import PageHeader from '../components/PageHeader.vue'
import StatusBadge from '../components/StatusBadge.vue'
import CapacityBar from '../components/CapacityBar.vue'
import EmptyState from '../components/EmptyState.vue'
import Spinner from '../components/Spinner.vue'
import Modal from '../components/Modal.vue'
import CodeBlock from '../components/CodeBlock.vue'
import CopyButton from '../components/CopyButton.vue'
import { toast, toastError } from '../components/toast'

const workers = ref(null)
const adding = ref(false)
const newName = ref('')
const creds = ref(null) // { worker, token, commands }
const credTab = ref('docker')
const confirm = ref(null)
const renaming = ref(null)

async function load() { workers.value = await api.get('/api/workers') }
onMounted(load)
refreshOn(['workers', 'queue'], load, 500)

function state(w) {
  if (w.status === 'online' && w.paused) return 'paused'
  return w.status
}

async function provision() {
  try {
    creds.value = await api.post('/api/workers', { name: newName.value })
    adding.value = false
    newName.value = ''
    credTab.value = 'docker'
    load()
  } catch (e) { toastError(e) }
}
async function rotate(w) {
  try {
    creds.value = await api.post(`/api/workers/${w.id}/token`)
    confirm.value = null
    toast('New token issued — the old one stops working immediately')
  } catch (e) { toastError(e) }
}
async function togglePause(w) {
  try { await api.post(`/api/workers/${w.id}/${w.paused ? 'unpause' : 'pause'}`); load() } catch (e) { toastError(e) }
}
async function remove(w) {
  try { await api.del(`/api/workers/${w.id}`); confirm.value = null; toast('Worker deleted'); load() } catch (e) { toastError(e) }
}
async function rename() {
  try { await api.patch(`/api/workers/${renaming.value.id}`, { name: renaming.value.name }); renaming.value = null; load() } catch (e) { toastError(e) }
}
</script>

<template>
  <PageHeader title="Workers" subtitle="Machines that execute tests. Any host with Docker can join over the internet.">
    <button class="btn btn-primary" @click="adding = true"><Plus class="h-4 w-4" />Add worker</button>
  </PageHeader>

  <Spinner v-if="!workers" />
  <div v-else-if="!workers.length" class="card">
    <EmptyState :icon="Server" title="No workers yet" text="Add a worker, run the generated docker command on any machine with Docker, and it will start pulling jobs from the queue.">
      <button class="btn btn-primary" @click="adding = true"><Plus class="h-4 w-4" />Add worker</button>
    </EmptyState>
  </div>
  <div v-else class="grid gap-4 md:grid-cols-2 2xl:grid-cols-3">
    <div v-for="w in workers" :key="w.id" class="card flex flex-col">
      <div class="flex items-start gap-3 p-4">
        <div class="grid h-10 w-10 shrink-0 place-items-center rounded-lg" :class="w.status === 'online' ? 'bg-emerald-500/10 text-emerald-300' : 'bg-ink-800 text-ink-400'">
          <Server class="h-5 w-5" />
        </div>
        <div class="min-w-0 flex-1">
          <div class="flex items-center gap-2">
            <span class="truncate font-semibold">{{ w.name }}</span>
            <span v-if="w.is_local" class="rounded bg-indigo-500/15 px-1.5 py-0.5 text-[10px] font-medium uppercase text-indigo-300">embedded</span>
          </div>
          <div class="truncate font-mono text-xs text-ink-400">{{ w.id }}<template v-if="w.hostname"> · {{ w.hostname }}</template></div>
        </div>
        <StatusBadge :status="state(w)" size="sm" />
      </div>
      <div class="grid grid-cols-2 gap-x-4 gap-y-3 border-t border-ink-800 px-4 py-3 text-xs">
        <div>
          <div class="mb-1 text-ink-400">Capacity (weight)</div>
          <CapacityBar :used="w.used_weight" :total="w.capacity" />
        </div>
        <div>
          <div class="mb-1 text-ink-400">Running jobs</div>
          <div class="font-mono text-ink-200">{{ w.running_jobs }} <span class="text-ink-500">/ {{ w.concurrency || '—' }} slots</span></div>
        </div>
        <div>
          <div class="mb-1 flex items-center gap-1 text-ink-400"><Cpu class="h-3 w-3" />Version</div>
          <div class="font-mono text-ink-200">{{ w.version || '—' }}</div>
        </div>
        <div>
          <div class="mb-1 flex items-center gap-1 text-ink-400"><Container class="h-3 w-3" />Completed jobs</div>
          <div class="font-mono text-ink-200">{{ w.completed_jobs }}</div>
        </div>
        <div class="col-span-2 text-ink-400">
          <template v-if="w.status === 'waiting'">Waiting for the worker to connect for the first time…</template>
          <template v-else>Last heartbeat {{ ago(w.last_heartbeat) }}<template v-if="w.max_containers"> · max {{ w.max_containers }} containers</template></template>
        </div>
      </div>
      <div class="mt-auto flex items-center gap-1 border-t border-ink-800 px-3 py-2">
        <button class="btn btn-ghost btn-sm" @click="togglePause(w)">
          <component :is="w.paused ? Play : Pause" class="h-3.5 w-3.5" />{{ w.paused ? 'Resume' : 'Pause' }}
        </button>
        <button class="btn btn-ghost btn-sm" @click="renaming = { id: w.id, name: w.name }"><Pencil class="h-3.5 w-3.5" />Rename</button>
        <template v-if="!w.is_local">
          <button class="btn btn-ghost btn-sm" @click="confirm = { kind: 'rotate', w }"><KeyRound class="h-3.5 w-3.5" />Token</button>
          <button class="btn btn-ghost btn-sm ml-auto text-red-300" @click="confirm = { kind: 'delete', w }"><Trash2 class="h-3.5 w-3.5" /></button>
        </template>
      </div>
    </div>
  </div>

  <Modal v-if="adding" title="Add worker" @close="adding = false">
    <form @submit.prevent="provision" class="space-y-3">
      <div>
        <label class="label">Name</label>
        <input v-model="newName" class="input" placeholder="e.g. build-server-01" autofocus />
      </div>
      <p class="text-xs text-ink-400">You will get a one-time token and a ready-to-run command. The worker needs Docker and outbound HTTPS access to this server — no inbound ports.</p>
    </form>
    <template #footer>
      <button class="btn" @click="adding = false">Cancel</button>
      <button class="btn btn-primary" @click="provision">Create worker</button>
    </template>
  </Modal>

  <Modal v-if="creds" :title="`Connect ${creds.worker.name}`" wide @close="creds = null">
    <div class="space-y-4">
      <div class="rounded-lg border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-xs text-amber-200">
        This token is shown only once. Store it now — you can always issue a new one.
      </div>
      <div>
        <label class="label">Worker token</label>
        <div class="flex gap-2">
          <input class="input font-mono text-xs" :value="creds.token" readonly @focus="$event.target.select()" />
          <CopyButton :text="creds.token" label="Copy" />
        </div>
      </div>
      <div>
        <div class="mb-2 flex gap-1 border-b border-ink-700/70">
          <button class="tab" :class="credTab === 'docker' && 'tab-active'" @click="credTab = 'docker'">Docker</button>
          <button class="tab" :class="credTab === 'binary' && 'tab-active'" @click="credTab = 'binary'">Binary</button>
        </div>
        <CodeBlock :code="creds.commands[credTab]" />
        <p class="mt-2 text-xs text-ink-400">
          The worker connects to <span class="font-mono text-ink-200">{{ creds.api_url }}</span>. If that is not reachable from the worker host, set
          <span class="kbd">CHIPERKA_PUBLIC_URL</span> on the server. Test images must be pullable by the worker (push locally built images to a registry).
        </p>
      </div>
    </div>
    <template #footer><button class="btn btn-primary" @click="creds = null">Done</button></template>
  </Modal>

  <Modal v-if="renaming" title="Rename worker" @close="renaming = null">
    <input v-model="renaming.name" class="input" @keyup.enter="rename" autofocus />
    <template #footer>
      <button class="btn" @click="renaming = null">Cancel</button>
      <button class="btn btn-primary" @click="rename">Save</button>
    </template>
  </Modal>

  <Modal v-if="confirm" :title="confirm.kind === 'delete' ? 'Delete worker' : 'Issue new token'" @close="confirm = null">
    <p v-if="confirm.kind === 'delete'" class="text-sm text-ink-300">Delete <b>{{ confirm.w.name }}</b>? Its token is revoked and running jobs are re-queued.</p>
    <p v-else class="text-sm text-ink-300">Issue a new token for <b>{{ confirm.w.name }}</b>? The current token stops working — restart the worker with the new one.</p>
    <template #footer>
      <button class="btn" @click="confirm = null">Cancel</button>
      <button v-if="confirm.kind === 'delete'" class="btn btn-danger" @click="remove(confirm.w)">Delete</button>
      <button v-else class="btn btn-primary" @click="rotate(confirm.w)">Issue token</button>
    </template>
  </Modal>
</template>
