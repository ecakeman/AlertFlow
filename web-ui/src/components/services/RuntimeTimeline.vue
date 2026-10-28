<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { api, ApiError } from '@/api'
import type { TimelineEvent, TimelinePage } from '@/types'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'

const props = defineProps<{ serviceId: string }>()

const windows = [
  { id: '1h', label: '1h', ms: 60 * 60 * 1000 },
  { id: '24h', label: '24h', ms: 24 * 60 * 60 * 1000 },
  { id: '7d', label: '7d', ms: 7 * 24 * 60 * 60 * 1000 },
  { id: '30d', label: '30d', ms: 30 * 24 * 60 * 60 * 1000 },
] as const

const sourceOptions = [
  { id: '', label: '全部' },
  { id: 'kubernetes', label: 'Kubernetes' },
  { id: 'application_diagnostics', label: '应用诊断' },
  { id: 'incident', label: 'Incident' },
  { id: 'agent', label: 'Agent' },
] as const

const windowId = ref<(typeof windows)[number]['id']>('24h')
const source = ref('')
const range = ref<{ from: string; to: string } | null>(null)
const page = ref<TimelinePage | null>(null)
const events = ref<TimelineEvent[]>([])
const loading = ref(false)
const loadingMore = ref(false)
const error = ref('')
const openId = ref<number | null>(null)

const sourceLabel = (id: string) => sourceOptions.find((item) => item.id === id)?.label ?? id

const notices = computed(() => {
  const coverage = page.value?.coverage
  if (!coverage) return []
  return Object.entries(coverage).flatMap(([name, item]) => {
    if (item.status === 'ok' || item.status === 'empty') return []
    if (item.status === 'disabled') {
      if (item.lastError) return [`${sourceLabel(name)}：${item.lastError}`]
      return [`${sourceLabel(name)}未配置`]
    }
    const extra = item.lastError ? `，${item.lastError}` : ''
    return [`${sourceLabel(name)}：${item.status}${extra}`]
  })
})

const quiet = computed(() => {
  const coverage = page.value?.coverage
  if (!coverage) return false
  return Object.values(coverage).every((item) =>
    item.status === 'ok' || item.status === 'empty' || item.status === 'disabled',
  )
})

const clamped = computed(() => {
  if (!page.value) return false
  const selected = windows.find((item) => item.id === windowId.value)
  if (!selected) return false
  const span = Date.parse(page.value.window.to) - Date.parse(page.value.window.from)
  return span + 60_000 < selected.ms
})

const groups = computed(() => {
  const out: { day: string; events: TimelineEvent[] }[] = []
  for (const ev of events.value) {
    const day = new Date(ev.occurredAt).toLocaleDateString()
    const last = out[out.length - 1]
    if (!last || last.day !== day) out.push({ day, events: [ev] })
    else last.events.push(ev)
  }
  return out
})

function when(value: string) {
  return new Date(value).toLocaleString()
}

function details(ev: TimelineEvent) {
  return JSON.stringify(ev.details ?? {}, null, 2)
}

async function load(cursor?: string) {
  const more = Boolean(cursor)
  if (more) loadingMore.value = true
  else loading.value = true
  error.value = ''
  const selected = windows.find((item) => item.id === windowId.value) ?? windows[1]
  if (!cursor || !range.value) {
    const to = new Date()
    const from = new Date(to.getTime() - selected.ms)
    range.value = { from: from.toISOString(), to: to.toISOString() }
  }
  const { from, to } = range.value
  try {
    const res = await api.serviceTimeline(props.serviceId, {
      from,
      to,
      sources: source.value || undefined,
      cursor,
      limit: 50,
    })
    page.value = res
    events.value = more ? events.value.concat(res.events) : res.events
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : '时间轴加载失败'
    if (!more) events.value = []
  } finally {
    loading.value = false
    loadingMore.value = false
  }
}

watch([() => props.serviceId, windowId, source], () => void load(), { immediate: true })
</script>

<template>
  <div>
    <div class="flex flex-wrap items-center gap-2">
      <Button
        v-for="item in windows"
        :key="item.id"
        size="sm"
        :variant="windowId === item.id ? 'default' : 'outline'"
        @click="windowId = item.id"
      >
        {{ item.label }}
      </Button>
      <span class="mx-1 h-4 w-px bg-border" />
      <Button
        v-for="item in sourceOptions"
        :key="item.id || 'all'"
        size="sm"
        :variant="source === item.id ? 'default' : 'outline'"
        @click="source = item.id"
      >
        {{ item.label }}
      </Button>
    </div>

    <p v-if="clamped" class="mt-3 text-xs text-muted-foreground">
      只显示最近 {{ page?.window.retentionDays }} 天内的事件。
    </p>
    <ul v-if="notices.length" class="mt-3 space-y-1 text-sm text-warning">
      <li v-for="notice in notices" :key="notice">{{ notice }}</li>
    </ul>
    <p v-if="error" class="mt-3 text-sm text-offline">{{ error }}</p>
    <p v-else-if="loading" class="mt-3 text-sm text-muted-foreground">加载时间轴…</p>
    <p v-else-if="events.length === 0 && quiet" class="mt-3 text-sm text-muted-foreground">
      这段时间没有事件。
    </p>

    <div v-for="group in groups" :key="group.day" class="mt-4">
      <p class="text-xs text-muted-foreground">{{ group.day }}</p>
      <ul class="mt-1 divide-y divide-border">
        <li v-for="ev in group.events" :key="ev.id">
          <button
            type="button"
            class="flex w-full cursor-pointer items-start gap-3 py-2 text-left"
            @click="openId = openId === ev.id ? null : ev.id"
          >
            <Badge :variant="ev.severity === 'error' ? 'offline' : ev.severity === 'warning' ? 'outline' : 'secondary'">
              {{ sourceLabel(ev.source) }}
            </Badge>
            <span class="min-w-0 flex-1">
              <span class="block truncate text-sm">{{ ev.title }}</span>
              <span class="text-xs text-muted-foreground">{{ when(ev.occurredAt) }}</span>
            </span>
          </button>
          <div v-if="openId === ev.id" class="mb-2 space-y-1 pl-1 text-xs text-muted-foreground">
            <p v-if="ev.summary">{{ ev.summary }}</p>
            <p v-if="ev.resource">
              {{ ev.resource.kind }} {{ ev.resource.namespace }}/{{ ev.resource.name }}
            </p>
            <RouterLink
              v-if="ev.incidentId"
              class="text-primary underline"
              :to="`/incidents/${ev.incidentId}`"
            >
              查看故障
            </RouterLink>
            <pre class="overflow-x-auto whitespace-pre-wrap">{{ details(ev) }}</pre>
          </div>
        </li>
      </ul>
    </div>

    <Button
      v-if="page?.nextCursor"
      class="mt-3"
      size="sm"
      variant="outline"
      :disabled="loadingMore"
      @click="load(page.nextCursor ?? undefined)"
    >
      加载更早
    </Button>
  </div>
</template>
