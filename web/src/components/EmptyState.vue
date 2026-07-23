<script setup lang="ts">
import { Inbox } from "@lucide/vue";
defineProps<{ message: string }>();
</script>
<template><div class="empty-state"><Inbox :size="24" /><span>{{ message }}</span></div></template>
