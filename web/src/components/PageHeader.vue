<script setup>
defineProps({ title: String, subtitle: String })
</script>

<template>
  <div class="mb-6 flex flex-wrap items-end justify-between gap-4">
    <div class="min-w-0">
      <slot name="eyebrow" />
      <h1 class="truncate text-xl font-semibold tracking-tight text-white">{{ title }}<slot name="title" /></h1>
      <p v-if="subtitle" class="mt-1 text-sm text-ink-400">{{ subtitle }}</p>
      <slot name="meta" />
    </div>
    <div class="flex flex-wrap items-center gap-2"><slot /></div>
  </div>
</template>
