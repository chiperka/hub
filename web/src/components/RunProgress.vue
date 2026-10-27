<script setup>
import { computed } from 'vue'
const props = defineProps({ run: Object, height: { type: String, default: 'h-1.5' } })
const segs = computed(() => {
  const r = props.run || {}
  const total = r.total || 1
  return [
    { n: r.passed, cls: 'bg-emerald-500' },
    { n: r.failed, cls: 'bg-red-500' },
    { n: r.errored, cls: 'bg-amber-500' },
    { n: r.running, cls: 'bg-sky-500 animate-pulse' },
    { n: (r.skipped || 0) + (r.cancelled || 0), cls: 'bg-ink-500' },
  ].filter((s) => s.n > 0).map((s) => ({ ...s, w: (s.n / total) * 100 }))
})
</script>

<template>
  <div class="flex w-full overflow-hidden rounded-full bg-ink-700/60" :class="height">
    <div v-for="(s, i) in segs" :key="i" :class="s.cls" :style="{ width: s.w + '%' }" class="transition-all duration-500" />
  </div>
</template>
