<script setup>
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { LayoutDashboard, PlayCircle, ListOrdered, Server, FolderKanban, Plug, Settings, LogOut, Menu, X } from 'lucide-vue-next'
import { api, session } from './api'
import { connected, startLive } from './live'
import Toasts from './components/Toasts.vue'

const route = useRoute()
const router = useRouter()
const bare = computed(() => route.meta.bare)
const mobileOpen = ref(false)

watch(() => session.authenticated, (ok) => ok && startLive(), { immediate: true })
watch(() => route.fullPath, () => (mobileOpen.value = false))

const nav = [
  { to: '/', label: 'Dashboard', icon: LayoutDashboard, exact: true },
  { to: '/runs', label: 'Runs', icon: PlayCircle },
  { to: '/queue', label: 'Queue', icon: ListOrdered },
  { to: '/workers', label: 'Workers', icon: Server },
  { to: '/projects', label: 'Projects', icon: FolderKanban },
  { to: '/connect', label: 'Connect CLI', icon: Plug },
  { to: '/settings', label: 'Settings', icon: Settings },
]

function active(item) {
  if (item.exact) return route.path === item.to
  if (item.to === '/runs') return route.path.startsWith('/runs') || route.path.startsWith('/tests')
  return route.path.startsWith(item.to)
}

async function logout() {
  await api.post('/api/auth/logout')
  session.authenticated = false
  router.push('/login')
}
</script>

<template>
  <router-view v-if="bare" />
  <div v-else class="flex min-h-full">
    <!-- Sidebar -->
    <aside
      class="fixed inset-y-0 left-0 z-40 flex w-60 flex-col border-r border-ink-800 bg-ink-900/95 backdrop-blur transition-transform lg:translate-x-0"
      :class="mobileOpen ? 'translate-x-0' : '-translate-x-full'"
    >
      <div class="flex h-14 items-center gap-2.5 px-5">
        <div class="grid h-7 w-7 place-items-center rounded-lg bg-indigo-500 shadow-lg shadow-indigo-500/30">
          <svg viewBox="0 0 32 32" class="h-4 w-4"><path d="M8 16.5l5 5 11-11" fill="none" stroke="#fff" stroke-width="3.5" stroke-linecap="round" stroke-linejoin="round" /></svg>
        </div>
        <div class="leading-tight">
          <div class="text-sm font-semibold tracking-tight">Chiperka</div>
          <div class="text-[11px] text-ink-400">Hub</div>
        </div>
        <button class="btn-ghost ml-auto rounded p-1 lg:hidden" @click="mobileOpen = false"><X class="h-4 w-4" /></button>
      </div>
      <nav class="flex-1 space-y-0.5 px-3 py-2">
        <router-link
          v-for="item in nav" :key="item.to" :to="item.to"
          class="flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm font-medium transition"
          :class="active(item) ? 'bg-ink-800 text-white' : 'text-ink-400 hover:bg-ink-850 hover:text-ink-100'"
        >
          <component :is="item.icon" class="h-4 w-4" :class="active(item) ? 'text-indigo-300' : ''" />
          {{ item.label }}
        </router-link>
      </nav>
      <div class="border-t border-ink-800 p-3">
        <div class="flex items-center gap-2 px-2 py-1.5 text-xs text-ink-400">
          <span class="relative flex h-2 w-2">
            <span v-if="connected" class="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-60" />
            <span class="relative inline-flex h-2 w-2 rounded-full" :class="connected ? 'bg-emerald-400' : 'bg-ink-500'" />
          </span>
          {{ connected ? 'Live' : 'Reconnecting…' }}
          <span class="ml-auto font-mono text-[10px] text-ink-500">{{ session.version }}</span>
        </div>
        <div class="mt-1 flex items-center gap-2 rounded-lg px-2 py-1.5">
          <div class="grid h-7 w-7 place-items-center rounded-full bg-ink-700 text-xs font-semibold uppercase text-ink-200">
            {{ (session.principal?.name || '?').slice(0, 1) }}
          </div>
          <div class="min-w-0 flex-1 truncate text-xs text-ink-300">{{ session.authDisabled ? 'Local mode (no auth)' : session.principal?.name }}</div>
          <button v-if="!session.authDisabled" class="btn-ghost rounded p-1" title="Log out" @click="logout"><LogOut class="h-4 w-4" /></button>
        </div>
      </div>
    </aside>
    <div v-if="mobileOpen" class="fixed inset-0 z-30 bg-black/50 lg:hidden" @click="mobileOpen = false" />

    <!-- Main -->
    <div class="flex min-w-0 flex-1 flex-col lg:pl-60">
      <header class="sticky top-0 z-20 flex h-14 items-center gap-3 border-b border-ink-800 bg-ink-950/80 px-4 backdrop-blur lg:hidden">
        <button class="btn-ghost rounded p-1" @click="mobileOpen = true"><Menu class="h-5 w-5" /></button>
        <span class="text-sm font-semibold">Chiperka</span>
      </header>
      <main class="mx-auto w-full max-w-[1400px] flex-1 px-4 py-6 sm:px-6 lg:px-8">
        <router-view v-slot="{ Component }">
          <component :is="Component" :key="$route.path" />
        </router-view>
      </main>
    </div>
  </div>
  <Toasts />
</template>
