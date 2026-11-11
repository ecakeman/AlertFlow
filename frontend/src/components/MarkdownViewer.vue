<script setup lang="ts">
import { computed } from 'vue'
import { renderMarkdown } from '../utils/markdown'

const props = withDefaults(defineProps<{ content?: string | null }>(), { content: '' })

// marked 解析 + DOMPurify 过滤后再 v-html，禁止脚本/事件属性注入
const html = computed(() => renderMarkdown(props.content))
</script>

<template>
  <div class="markdown-body" v-html="html" />
</template>
