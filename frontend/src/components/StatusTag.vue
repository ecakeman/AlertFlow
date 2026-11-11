<script setup lang="ts">
import { computed } from 'vue'
import { statusLabel, statusTagType } from '../utils/status'

const props = defineProps<{
  status?: string | null
  /** 覆盖默认中文文案（例如“已禁用”） */
  label?: string
  size?: 'small' | 'default' | 'large'
}>()

const text = computed(() => props.label ?? statusLabel(props.status))
const type = computed(() => statusTagType(props.status))
</script>

<template>
  <el-tag :type="type" :size="size ?? 'small'" effect="light">{{ text }}</el-tag>
</template>
