<script setup>
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, session, loadSession } from '../api'

const route = useRoute()
const router = useRouter()
const email = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  busy.value = true
  try {
    await api.post(session.setupRequired ? '/api/auth/setup' : '/api/auth/login', { email: email.value, password: password.value })
    await loadSession()
    router.replace(route.query.next || '/')
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div class="flex min-h-full items-center justify-center bg-[radial-gradient(ellipse_at_top,rgba(99,102,241,0.15),transparent_60%)] px-4">
    <div class="w-full max-w-sm">
      <div class="mb-8 flex flex-col items-center text-center">
        <div class="mb-4 grid h-12 w-12 place-items-center rounded-2xl bg-indigo-500 shadow-xl shadow-indigo-500/30">
          <svg viewBox="0 0 32 32" class="h-6 w-6"><path d="M8 16.5l5 5 11-11" fill="none" stroke="#fff" stroke-width="3.5" stroke-linecap="round" stroke-linejoin="round" /></svg>
        </div>
        <h1 class="text-xl font-semibold tracking-tight">{{ session.setupRequired ? 'Set up Chiperka' : 'Sign in to Chiperka' }}</h1>
        <p class="mt-1 text-sm text-ink-400">
          {{ session.setupRequired ? 'Create the first administrator account.' : 'Chiperka Hub — self-hosted test control panel' }}
        </p>
      </div>
      <form class="card space-y-4 p-6" @submit.prevent="submit">
        <div>
          <label class="label">Email</label>
          <input v-model="email" class="input" type="email" autocomplete="username" required autofocus />
        </div>
        <div>
          <label class="label">Password</label>
          <input v-model="password" class="input" type="password" :autocomplete="session.setupRequired ? 'new-password' : 'current-password'" required minlength="8" />
        </div>
        <p v-if="error" class="rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-sm text-red-300">{{ error }}</p>
        <button class="btn btn-primary w-full py-2" :disabled="busy">{{ session.setupRequired ? 'Create account' : 'Sign in' }}</button>
      </form>
    </div>
  </div>
</template>
