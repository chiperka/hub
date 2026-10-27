<script setup>
defineProps({ icon: [Object, Function], title: String, text: String })
</script>

<template>
  <div class="flex flex-col items-center justify-center px-6 py-14 text-center">
    <div v-if="icon" class="mb-3 grid h-11 w-11 place-items-center rounded-xl bg-ink-800 text-ink-400">
      <component :is="icon" class="h-5 w-5" />
    </div>
    <div class="text-sm font-medium text-ink-200">{{ title }}</div>
    <div v-if="text" class="mt-1 max-w-md text-sm text-ink-400">{{ text }}</div>
    <div v-if="$slots.default" class="mt-4"><slot /></div>
  </div>
</template>
