<script setup>
import CopyButton from './CopyButton.vue'
defineProps({ code: String, maxHeight: { type: String, default: '28rem' }, copy: { type: Boolean, default: true } })
</script>

<template>
  <div class="group relative">
    <pre class="code whitespace-pre-wrap break-words" :style="{ maxHeight }">{{ code }}</pre>
    <div v-if="copy && code" class="absolute right-2 top-2 opacity-0 transition group-hover:opacity-100">
      <CopyButton :text="code" />
    </div>
  </div>
</template>
