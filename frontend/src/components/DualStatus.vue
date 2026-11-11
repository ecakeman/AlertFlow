<script setup lang="ts">
import StatusTag from './StatusTag.vue'

// 双来源健康状态：监控观测与应用上报分开表达，避免混成一个“健康”标签。
withDefaults(
  defineProps<{
    monitorStatus?: string | null
    appStatus?: string | null
    inline?: boolean
    size?: 'default' | 'small'
  }>(),
  { inline: false, size: 'small' }
)
</script>

<template>
  <div class="dual-status" :class="{ 'dual-status--inline': inline }">
    <span class="dual-status-line">
      <span class="dual-status-label">监控</span>
      <StatusTag :status="monitorStatus" :size="size" />
    </span>
    <span class="dual-status-line">
      <span class="dual-status-label">应用</span>
      <StatusTag :status="appStatus" :size="size" />
    </span>
  </div>
</template>
