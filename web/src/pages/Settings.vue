<script setup>
import { onMounted, ref } from 'vue'
import { Users, Plus, Trash2, Info } from 'lucide-vue-next'
import { api, session } from '../api'
import { ago } from '../format'
import PageHeader from '../components/PageHeader.vue'
import Modal from '../components/Modal.vue'
import { toast, toastError } from '../components/toast'

const users = ref([])
const info = ref(null)
const adding = ref(false)
const form = ref({ email: '', password: '' })
const pw = ref({ current: '', new: '' })

async function load() {
  info.value = await api.get('/api/connect')
  if (!session.authDisabled) users.value = await api.get('/api/users')
}
onMounted(load)

async function addUser() {
  try { await api.post('/api/users', form.value); adding.value = false; form.value = { email: '', password: '' }; load(); toast('User created') } catch (e) { toastError(e) }
}
async function delUser(u) {
  try { await api.del(`/api/users/${u.id}`); load() } catch (e) { toastError(e) }
}
async function changePassword() {
  try { await api.post('/api/me/password', pw.value); pw.value = { current: '', new: '' }; toast('Password changed') } catch (e) { toastError(e) }
}
</script>

<template>
  <PageHeader title="Settings" />
  <div class="grid gap-4 xl:grid-cols-2">
    <div v-if="!session.authDisabled" class="card">
      <div class="card-header">
        <div class="flex items-center gap-2"><Users class="h-4 w-4 text-ink-400" /><h2 class="card-title">Users</h2></div>
        <button class="btn btn-sm" @click="adding = true"><Plus class="h-3.5 w-3.5" />Add user</button>
      </div>
      <ul class="divide-y divide-ink-800">
        <li v-for="u in users" :key="u.id" class="flex items-center gap-3 px-4 py-3">
          <div class="grid h-8 w-8 place-items-center rounded-full bg-ink-700 text-xs font-semibold uppercase">{{ u.email[0] }}</div>
          <div class="flex-1">
            <div class="text-sm">{{ u.email }}</div>
            <div class="text-[11px] text-ink-400">added {{ ago(u.created_at) }}</div>
          </div>
          <button v-if="u.email !== session.principal?.email" class="btn btn-ghost btn-sm" @click="delUser(u)"><Trash2 class="h-3.5 w-3.5" /></button>
        </li>
      </ul>
    </div>

    <div v-if="session.principal?.kind === 'user'" class="card p-5">
      <h2 class="card-title mb-3">Change password</h2>
      <form class="space-y-3" @submit.prevent="changePassword">
        <div><label class="label">Current password</label><input v-model="pw.current" type="password" class="input" autocomplete="current-password" /></div>
        <div><label class="label">New password</label><input v-model="pw.new" type="password" class="input" minlength="8" autocomplete="new-password" /></div>
        <button class="btn btn-primary">Update password</button>
      </form>
    </div>

    <div v-if="info" class="card p-5">
      <div class="mb-3 flex items-center gap-2"><Info class="h-4 w-4 text-ink-400" /><h2 class="card-title">About this server</h2></div>
      <dl class="grid grid-cols-[10rem_1fr] gap-y-2 text-sm">
        <dt class="text-ink-400">Version</dt><dd class="font-mono">{{ info.version }}</dd>
        <dt class="text-ink-400">Public URL</dt><dd class="font-mono">{{ info.server_url }}</dd>
        <dt class="text-ink-400">Authentication</dt><dd>{{ info.auth_disabled ? 'disabled (local mode)' : 'enabled' }}</dd>
        <dt class="text-ink-400">Worker image</dt><dd class="break-all font-mono text-xs">{{ info.worker_image || 'not set (CHIPERKA_WORKER_IMAGE)' }}</dd>
      </dl>
      <p class="mt-4 text-xs text-ink-400">Configuration is done with environment variables (<span class="kbd">CHIPERKA_PUBLIC_URL</span>, <span class="kbd">CHIPERKA_RETENTION_DAYS</span>, <span class="kbd">CHIPERKA_WORKER_JOIN_TOKEN</span>, …) — see the README.</p>
    </div>
  </div>

  <Modal v-if="adding" title="Add user" @close="adding = false">
    <div class="space-y-3">
      <div><label class="label">Email</label><input v-model="form.email" class="input" type="email" autofocus /></div>
      <div><label class="label">Password</label><input v-model="form.password" class="input" type="password" minlength="8" /></div>
    </div>
    <template #footer>
      <button class="btn" @click="adding = false">Cancel</button>
      <button class="btn btn-primary" @click="addUser">Create</button>
    </template>
  </Modal>
</template>
