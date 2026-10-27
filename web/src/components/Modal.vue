<script setup>
import { onMounted, onBeforeUnmount } from 'vue'
import { X } from 'lucide-vue-next'
const props = defineProps({ title: String, wide: Boolean })
const emit = defineEmits(['close'])
const onKey = (e) => e.key === 'Escape' && emit('close')
onMounted(() => window.addEventListener('keydown', onKey))
onBeforeUnmount(() => window.removeEventListener('keydown', onKey))
</script>

<template>
  <teleport to="body">
    <div class="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/60 p-4 pt-[10vh] backdrop-blur-sm" @mousedown.self="emit('close')">
      <div class="card w-full shadow-2xl" :class="wide ? 'max-w-3xl' : 'max-w-lg'">
        <div class="card-header">
          <h3 class="card-title">{{ title }}</h3>
          <button class="btn-ghost rounded p-1" @click="emit('close')"><X class="h-4 w-4" /></button>
        </div>
        <div class="p-4"><slot /></div>
        <div v-if="$slots.footer" class="flex justify-end gap-2 border-t border-ink-700/70 px-4 py-3"><slot name="footer" /></div>
      </div>
    </div>
  </teleport>
</template>
