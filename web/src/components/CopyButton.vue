<script setup>
import { ref } from 'vue'
import { Copy, Check } from 'lucide-vue-next'
const props = defineProps({ text: String, label: String })
const done = ref(false)
async function copy() {
  try {
    await navigator.clipboard.writeText(props.text)
  } catch {
    const ta = document.createElement('textarea')
    ta.value = props.text
    document.body.appendChild(ta)
    ta.select()
    document.execCommand('copy')
    ta.remove()
  }
  done.value = true
  setTimeout(() => (done.value = false), 1500)
}
</script>

<template>
  <button class="btn btn-sm" type="button" @click="copy" :title="label || 'Copy'">
    <Check v-if="done" class="h-3.5 w-3.5 text-emerald-400" />
    <Copy v-else class="h-3.5 w-3.5" />
    <span v-if="label">{{ done ? 'Copied' : label }}</span>
  </button>
</template>
