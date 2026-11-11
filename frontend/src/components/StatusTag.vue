<script setup lang="ts">
import { computed } from 'vue'
import { statusLabel, statusTone, type StatusTone } from '../utils/status'

const props = withDefaults(
  defineProps<{
    status?: string | null
    label?: string
    tone?: StatusTone
    size?: 'default' | 'small'
    dot?: boolean
  }>(),
  { size: 'default', dot: true }
)

const resolvedTone = computed<StatusTone>(() => props.tone ?? statusTone(props.status))
const text = computed(() => props.label ?? statusLabel(props.status))
</script>

<template>
  <span class="status-tag" :class="[`status-tag--${resolvedTone}`, { 'status-tag--small': size === 'small' }]">
    <span v-if="dot" class="status-dot" />
    <span>{{ text }}</span>
  </span>
</template>
