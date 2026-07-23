<script setup lang="ts">
import { X } from "@lucide/vue";

withDefaults(defineProps<{ title: string; wide?: boolean; busy?: boolean; submitLabel?: string }>(), { submitLabel: "保存" });
const emit = defineEmits<{ close: []; submit: [] }>();
</script>

<template>
  <Teleport to="body">
    <div class="modal-layer" @mousedown.self="emit('close')">
      <form class="form-modal" :class="{ wide }" @submit.prevent="emit('submit')">
        <header class="modal-head"><h2>{{ title }}</h2><button class="icon-button" type="button" title="关闭" @click="emit('close')"><X :size="18" /></button></header>
        <div class="modal-body"><slot /></div>
        <footer class="modal-actions"><slot name="actions"><button class="button quiet" type="button" @click="emit('close')">取消</button><button class="button primary" type="submit" :disabled="busy">{{ submitLabel }}</button></slot></footer>
      </form>
    </div>
  </Teleport>
</template>
