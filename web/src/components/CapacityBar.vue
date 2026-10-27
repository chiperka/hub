<script setup>
import { computed } from 'vue'
const props = defineProps({ used: Number, total: Number })
const p = computed(() => (props.total > 0 ? Math.min(100, Math.round((props.used / props.total) * 100)) : 0))
</script>

<template>
  <div class="flex items-center gap-2">
    <div class="h-1.5 w-24 overflow-hidden rounded-full bg-ink-700/60">
      <div class="h-full rounded-full transition-all duration-500" :class="p > 85 ? 'bg-amber-400' : 'bg-indigo-400'" :style="{ width: p + '%' }" />
    </div>
    <span class="font-mono text-xs text-ink-400">{{ used || 0 }}/{{ total || '∞' }}</span>
  </div>
</template>
