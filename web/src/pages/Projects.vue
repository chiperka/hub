<script setup>
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { FolderKanban, Plus, Trash2 } from 'lucide-vue-next'
import { api } from '../api'
import { refreshOn } from '../live'
import { ago } from '../format'
import PageHeader from '../components/PageHeader.vue'
import EmptyState from '../components/EmptyState.vue'
import Spinner from '../components/Spinner.vue'
import Modal from '../components/Modal.vue'
import { toast, toastError } from '../components/toast'

const router = useRouter()
const projects = ref(null)
const adding = ref(false)
const form = ref({ slug: '', name: '' })
const confirm = ref(null)

async function load() { projects.value = await api.get('/api/projects') }
onMounted(load)
refreshOn('runs', load, 1000)

async function create() {
  try { await api.post('/api/projects', form.value); adding.value = false; form.value = { slug: '', name: '' }; load() } catch (e) { toastError(e) }
}
async function remove(p) {
  try { await api.del(`/api/projects/${p.id}`); confirm.value = null; toast('Project deleted'); load() } catch (e) { toastError(e) }
}
</script>

<template>
  <PageHeader title="Projects" subtitle="Runs are grouped by project. The CLI creates projects automatically from cloud.project.">
    <button class="btn btn-primary" @click="adding = true"><Plus class="h-4 w-4" />New project</button>
  </PageHeader>
  <div class="card overflow-hidden">
    <Spinner v-if="!projects" />
    <EmptyState v-else-if="!projects.length" :icon="FolderKanban" title="No projects" text="Set `cloud.project` in chiperka.yaml (or pass --project) and the project appears on the first run." />
    <table v-else class="table">
      <thead><tr><th>Project</th><th class="text-right">Runs</th><th class="text-right">Last run</th><th class="w-12"></th></tr></thead>
      <tbody>
        <tr v-for="p in projects" :key="p.id" class="clickable" @click="router.push(`/runs?project=${p.slug}`)">
          <td>
            <div class="font-medium">{{ p.name }}</div>
            <div class="font-mono text-xs text-ink-400">{{ p.slug }}</div>
          </td>
          <td class="text-right font-mono text-sm">{{ p.run_count }}</td>
          <td class="text-right text-xs text-ink-400">{{ ago(p.last_run_at) }}</td>
          <td class="text-right"><button class="btn btn-ghost btn-sm" @click.stop="confirm = p"><Trash2 class="h-3.5 w-3.5" /></button></td>
        </tr>
      </tbody>
    </table>
  </div>

  <Modal v-if="adding" title="New project" @close="adding = false">
    <div class="space-y-3">
      <div><label class="label">Slug (used by the CLI)</label><input v-model="form.slug" class="input font-mono" placeholder="my-api" autofocus /></div>
      <div><label class="label">Display name</label><input v-model="form.name" class="input" placeholder="My API" /></div>
    </div>
    <template #footer>
      <button class="btn" @click="adding = false">Cancel</button>
      <button class="btn btn-primary" @click="create">Create</button>
    </template>
  </Modal>
  <Modal v-if="confirm" title="Delete project" @close="confirm = null">
    <p class="text-sm text-ink-300">Delete project <b>{{ confirm.slug }}</b>? Its runs are kept but no longer grouped.</p>
    <template #footer>
      <button class="btn" @click="confirm = null">Cancel</button>
      <button class="btn btn-danger" @click="remove(confirm)">Delete</button>
    </template>
  </Modal>
</template>
