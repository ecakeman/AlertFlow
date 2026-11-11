<script setup lang="ts">
defineProps<{
  message: string
  loading?: boolean
}>()

defineEmits<{ (e: 'retry'): void }>()
</script>

<template>
  <el-alert type="error" :closable="false" show-icon>
    <template #title>加载失败</template>
    <div class="error-retry-body">
      <span class="error-retry-message">{{ message }}</span>
      <el-button size="small" type="primary" plain :loading="loading" @click="$emit('retry')">
        重试
      </el-button>
    </div>
  </el-alert>
</template>

<style scoped>
.error-retry-body {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-wrap: wrap;
  margin-top: 4px;
}

.error-retry-message {
  word-break: break-word;
}
</style>
