<script setup>
import { computed } from 'vue'
import { CheckCircle2, XCircle, AlertTriangle, Loader2, Clock, MinusCircle, Ban, PauseCircle, Wifi, WifiOff, Hourglass } from 'lucide-vue-next'

const props = defineProps({ status: String, size: { type: String, default: 'md' } })

const map = {
  passed: { icon: CheckCircle2, cls: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/25' },
  completed: { icon: CheckCircle2, cls: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/25' },
  online: { icon: Wifi, cls: 'bg-emerald-500/10 text-emerald-300 ring-emerald-500/25' },
  failed: { icon: XCircle, cls: 'bg-red-500/10 text-red-300 ring-red-500/25' },
  error: { icon: AlertTriangle, cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/25' },
  running: { icon: Loader2, cls: 'bg-sky-500/10 text-sky-300 ring-sky-500/25', spin: true },
  claimed: { icon: Loader2, cls: 'bg-sky-500/10 text-sky-300 ring-sky-500/25', spin: true },
  pending: { icon: Clock, cls: 'bg-ink-700/50 text-ink-300 ring-ink-600' },
  waiting: { icon: Hourglass, cls: 'bg-ink-700/50 text-ink-300 ring-ink-600' },
  skipped: { icon: MinusCircle, cls: 'bg-ink-700/50 text-ink-400 ring-ink-600' },
  cancelled: { icon: Ban, cls: 'bg-ink-700/50 text-ink-400 ring-ink-600' },
  paused: { icon: PauseCircle, cls: 'bg-amber-500/10 text-amber-300 ring-amber-500/25' },
  offline: { icon: WifiOff, cls: 'bg-red-500/10 text-red-300 ring-red-500/25' },
}
const s = computed(() => map[props.status] || map.pending)
</script>

<template>
  <span
    class="inline-flex items-center gap-1 rounded-full font-medium capitalize ring-1 ring-inset"
    :class="[s.cls, size === 'sm' ? 'px-1.5 py-0.5 text-[11px]' : 'px-2 py-0.5 text-xs']"
  >
    <component :is="s.icon" :class="[size === 'sm' ? 'h-3 w-3' : 'h-3.5 w-3.5', s.spin ? 'animate-spin' : '']" />
    {{ status }}
  </span>
</template>
