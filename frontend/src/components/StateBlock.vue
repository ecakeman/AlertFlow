<script setup lang="ts">
// 统一的加载 / 错误 / 空状态。页面不再各自拼 el-alert / el-empty，保证语气与操作一致。
withDefaults(
  defineProps<{
    mode: 'loading' | 'error' | 'empty'
    message?: string
    retryText?: string
    compact?: boolean
  }>(),
  { retryText: '重试' }
)

defineEmits<{ (event: 'retry'): void }>()
</script>

<template>
  <div class="state-block" :class="{ 'state-block--error': mode === 'error', 'state-block--compact': compact }">
    <template v-if="mode === 'loading'">
      <span class="state-block__icon"><el-icon class="is-loading"><Loading /></el-icon></span>
      <span class="state-block__text">加载中…</span>
    </template>

    <template v-else-if="mode === 'error'">
      <span class="state-block__icon"><el-icon><WarningFilled /></el-icon></span>
      <span class="state-block__text">{{ message || '加载失败' }}</span>
      <div class="state-block__actions">
        <el-button size="small" @click="$emit('retry')">{{ retryText }}</el-button>
      </div>
    </template>

    <template v-else>
      <span class="state-block__icon"><el-icon><DocumentRemove /></el-icon></span>
      <span class="state-block__text">{{ message || '暂无数据' }}</span>
      <div v-if="$slots.default" class="state-block__actions">
        <slot />
      </div>
    </template>
  </div>
</template>
