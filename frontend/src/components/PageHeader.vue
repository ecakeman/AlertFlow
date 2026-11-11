<script setup lang="ts">
import { useRouter } from 'vue-router'

const props = withDefaults(
  defineProps<{
    title: string
    subtitle?: string
    backTo?: string
    backText?: string
  }>(),
  { backText: '返回' }
)

const router = useRouter()

function goBack() {
  if (props.backTo) router.push(props.backTo)
}
</script>

<template>
  <div class="page-header">
    <div class="page-header-main">
      <div v-if="backTo" class="page-back" @click="goBack">
        <el-icon><ArrowLeft /></el-icon>
        <span>{{ backText }}</span>
      </div>
      <div class="page-title-row">
        <h1 class="page-title">{{ title }}</h1>
        <slot name="title-extra" />
      </div>
      <p v-if="subtitle" class="page-subtitle">{{ subtitle }}</p>
      <slot name="meta" />
    </div>
    <div v-if="$slots.actions" class="page-actions">
      <slot name="actions" />
    </div>
  </div>
</template>
