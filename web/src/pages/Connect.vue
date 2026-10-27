<script setup>
import { computed, onMounted, ref } from 'vue'
import { KeyRound, Plus, Trash2, Terminal, Bot } from 'lucide-vue-next'
import { api, session } from '../api'
import { ago } from '../format'
import PageHeader from '../components/PageHeader.vue'
import CodeBlock from '../components/CodeBlock.vue'
import CopyButton from '../components/CopyButton.vue'
import Modal from '../components/Modal.vue'
import { toast, toastError } from '../components/toast'

const info = ref(null)
const tokens = ref([])
const projects = ref([])
const project = ref('my-project')
const newToken = ref(null)
const creating = ref(false)
const tokenName = ref('')

async function load() {
  const [i, t, p] = await Promise.all([api.get('/api/connect'), api.get('/api/tokens'), api.get('/api/projects')])
  info.value = i
  tokens.value = t
  projects.value = p
  if (p.length) project.value = p[0].slug
}
onMounted(load)

const host = computed(() => { try { return new URL(info.value?.server_url).host } catch { return '' } })
const token = computed(() => newToken.value?.token || 'cpk_…')
const yaml = computed(() => `# .chiperka/chiperka.yaml\ncloud:\n  url: ${info.value?.server_url}\n  project: ${project.value}`)
const authJson = computed(() => JSON.stringify({ 'chiperka-token': { [host.value]: token.value } }, null, 2))
const envs = computed(() => `export CHIPERKA_CLOUD_URL=${info.value?.server_url}\nexport CHIPERKA_TOKEN=${token.value}\nchiperka test --cloud --project ${project.value}`)

async function createToken() {
  try {
    newToken.value = await api.post('/api/tokens', { name: tokenName.value || 'cli' })
    creating.value = false
    tokenName.value = ''
    tokens.value = await api.get('/api/tokens')
  } catch (e) { toastError(e) }
}
async function revoke(t) {
  try { await api.del(`/api/tokens/${t.id}`); tokens.value = await api.get('/api/tokens'); toast('Token revoked') } catch (e) { toastError(e) }
}
</script>

<template>
  <PageHeader title="Connect the CLI" subtitle="The stock chiperka CLI talks to this server exactly like it talks to Chiperka Cloud." />
  <div v-if="info" class="grid gap-4 xl:grid-cols-5">
    <div class="space-y-4 xl:col-span-3">
      <div class="card p-5">
        <div class="mb-3 flex items-center gap-2"><Terminal class="h-4 w-4 text-indigo-300" /><h2 class="card-title">1 · Point the project at this server</h2></div>
        <div class="mb-3 flex items-center gap-2 text-xs text-ink-400">
          Project
          <input v-model="project" class="input w-48 py-1 font-mono text-xs" list="projects" />
          <datalist id="projects"><option v-for="p in projects" :key="p.id" :value="p.slug" /></datalist>
        </div>
        <CodeBlock :code="yaml" />
        <p class="mt-2 text-xs text-ink-400">Or set <span class="kbd">CHIPERKA_CLOUD_URL</span> to override <span class="font-mono">cloud.url</span> without touching the file. Projects are created on first use.</p>
      </div>

      <div class="card p-5">
        <div class="mb-3 flex items-center gap-2"><KeyRound class="h-4 w-4 text-indigo-300" /><h2 class="card-title">2 · Authenticate</h2></div>
        <p v-if="info.auth_disabled" class="text-sm text-ink-300">Authentication is disabled on this server — no token needed.</p>
        <template v-else>
          <p class="mb-3 text-sm text-ink-300">Create an API token below, then either export it or put it into <span class="font-mono">auth.json</span> next to where you run the CLI:</p>
          <CodeBlock :code="authJson" />
        </template>
      </div>

      <div class="card p-5">
        <div class="mb-3 flex items-center gap-2"><Terminal class="h-4 w-4 text-indigo-300" /><h2 class="card-title">3 · Run tests</h2></div>
        <CodeBlock :code="envs" />
        <p class="mt-2 text-xs text-ink-400">
          Results stream back to the terminal and appear live under <router-link to="/runs" class="link">Runs</router-link>.
          <span class="font-mono">--junit</span>, <span class="font-mono">--html</span> and <span class="font-mono">--artifacts</span> download reports from this server.
        </p>
      </div>

      <div class="card p-5">
        <div class="mb-3 flex items-center gap-2"><Bot class="h-4 w-4 text-indigo-300" /><h2 class="card-title">MCP / AI agents</h2></div>
        <p class="text-sm text-ink-300">
          Run ids returned by <span class="font-mono">chiperka test --cloud</span> are <span class="font-mono">cr-&lt;id&gt;</span>, test ids
          <span class="font-mono">ct-…</span>. The results API (<span class="font-mono">/api/results/runs</span>, <span class="font-mono">/api/results/tests/&lt;uuid&gt;</span>)
          serves them in the same shape as local results, so <span class="font-mono">chiperka mcp</span> can drill into remote runs once the CLI's cloud result store is enabled.
        </p>
      </div>
    </div>

    <div class="xl:col-span-2">
      <div class="card">
        <div class="card-header">
          <h2 class="card-title">API tokens</h2>
          <button class="btn btn-primary btn-sm" @click="creating = true"><Plus class="h-3.5 w-3.5" />New token</button>
        </div>
        <div v-if="newToken" class="space-y-2 border-b border-ink-700/70 bg-emerald-500/5 p-4">
          <div class="text-xs text-emerald-200">New token <b>{{ newToken.name }}</b> — copy it now, it won't be shown again.</div>
          <div class="flex gap-2"><input class="input font-mono text-xs" :value="newToken.token" readonly @focus="$event.target.select()" /><CopyButton :text="newToken.token" label="Copy" /></div>
        </div>
        <ul class="divide-y divide-ink-800">
          <li v-for="t in tokens" :key="t.id" class="flex items-center gap-3 px-4 py-3">
            <KeyRound class="h-4 w-4 text-ink-500" />
            <div class="min-w-0 flex-1">
              <div class="truncate text-sm">{{ t.name }}</div>
              <div class="font-mono text-[11px] text-ink-400">{{ t.prefix }}… · used {{ ago(t.last_used_at) }}</div>
            </div>
            <button class="btn btn-ghost btn-sm" @click="revoke(t)"><Trash2 class="h-3.5 w-3.5" /></button>
          </li>
          <li v-if="!tokens.length" class="px-4 py-8 text-center text-sm text-ink-400">No API tokens yet.</li>
        </ul>
      </div>
    </div>
  </div>

  <Modal v-if="creating" title="New API token" @close="creating = false">
    <label class="label">Name</label>
    <input v-model="tokenName" class="input" placeholder="e.g. laptop, ci" autofocus @keyup.enter="createToken" />
    <template #footer>
      <button class="btn" @click="creating = false">Cancel</button>
      <button class="btn btn-primary" @click="createToken">Create</button>
    </template>
  </Modal>
</template>
