<script setup>
import { CheckCircle2, XCircle, AlertTriangle, Loader2, Clock, MinusCircle, Ban } from 'lucide-vue-next'
defineProps({ status: String })
</script>

<template>
  <CheckCircle2 v-if="status === 'passed' || status === 'completed'" class="h-4 w-4 shrink-0 text-emerald-400" />
  <XCircle v-else-if="status === 'failed'" class="h-4 w-4 shrink-0 text-red-400" />
  <AlertTriangle v-else-if="status === 'error'" class="h-4 w-4 shrink-0 text-amber-400" />
  <Loader2 v-else-if="status === 'running'" class="h-4 w-4 shrink-0 animate-spin text-sky-400" />
  <Ban v-else-if="status === 'cancelled'" class="h-4 w-4 shrink-0 text-ink-500" />
  <MinusCircle v-else-if="status === 'skipped'" class="h-4 w-4 shrink-0 text-ink-500" />
  <Clock v-else class="h-4 w-4 shrink-0 text-ink-500" />
</template>
