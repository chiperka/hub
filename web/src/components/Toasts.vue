<script setup>
import { toasts } from './toast'
</script>

<template>
  <div class="pointer-events-none fixed bottom-4 right-4 z-[100] flex w-80 flex-col gap-2">
    <transition-group name="toast">
      <div
        v-for="t in toasts" :key="t.id"
        class="pointer-events-auto rounded-lg border px-4 py-3 text-sm shadow-xl backdrop-blur"
        :class="t.kind === 'error' ? 'border-red-500/40 bg-red-950/90 text-red-100' : 'border-ink-600 bg-ink-800/95 text-ink-100'"
      >{{ t.message }}</div>
    </transition-group>
  </div>
</template>

<style scoped>
.toast-enter-active, .toast-leave-active { transition: all .2s ease; }
.toast-enter-from, .toast-leave-to { opacity: 0; transform: translateY(8px); }
</style>
