<script setup lang="ts">
defineProps<{ title: string; subtitle: string; kicker?: string }>();
</script>

<template>
  <header class="page-heading page-heading-actions">
    <div><span class="page-kicker">{{ kicker || 'HOSTDESK' }}</span><h1>{{ title }}</h1><p>{{ subtitle }}</p></div>
    <div class="heading-actions"><slot /></div>
  </header>
</template>
