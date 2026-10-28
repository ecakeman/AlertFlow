<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogFooter,
  DialogTitle,
  DialogDescription,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { Switch } from '@/components/ui/switch'
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from '@/components/ui/select'
import type { Integration } from '@/types'
import type { IntegrationInput } from '@/stores/integrations'

const props = defineProps<{ open: boolean; integration?: Integration | null }>()
const emit = defineEmits<{ 'update:open': [boolean]; submit: [IntegrationInput] }>()

const isEdit = computed(() => !!props.integration)

const name = ref('')
const notifyWarnings = ref(false)
const f = reactive({
  url: '',
  method: 'POST',
  headers: '',
})

function str(v: unknown): string {
  return v == null ? '' : String(v)
}
function headersToText(h: unknown): string {
  if (!h || typeof h !== 'object') return ''
  return Object.entries(h as Record<string, unknown>)
    .map(([k, v]) => `${k}: ${str(v)}`)
    .join('\n')
}
function parseHeaders(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const idx = line.indexOf(':')
    if (idx === -1) continue
    const k = line.slice(0, idx).trim()
    const v = line.slice(idx + 1).trim()
    if (k) out[k] = v
  }
  return out
}

function reset() {
  const it = props.integration
  name.value = it?.name ?? ''
  notifyWarnings.value = it?.notifyWarnings ?? false
  const c = (it?.config ?? {}) as Record<string, unknown>
  f.url = str(c.url)
  f.method = str(c.method) || 'POST'
  f.headers = headersToText(c.headers)
}
watch(
  () => props.open,
  (o) => o && reset(),
)

const valid = computed(() => name.value.trim() !== '' && f.url.trim() !== '')

function submit() {
  if (!valid.value) return
  emit('submit', {
    type: 'agent_webhook',
    name: name.value.trim(),
    enabled: props.integration?.enabled ?? true,
    notifyWarnings: notifyWarnings.value,
    config: {
      url: f.url.trim(),
      method: f.method,
      headers: parseHeaders(f.headers),
    },
  })
  emit('update:open', false)
}
</script>

<template>
  <Dialog :open="open" @update:open="emit('update:open', $event)">
    <DialogContent>
      <DialogHeader>
        <DialogTitle>{{ isEdit ? '编辑 Agent' : '添加 Agent' }}</DialogTitle>
        <DialogDescription>故障打开时向诊断 Agent 发送调查任务。Slack、Telegram、邮件和通用 Webhook 已退役。</DialogDescription>
      </DialogHeader>

      <form class="grid gap-4" @submit.prevent="submit">
        <div class="grid gap-2">
          <Label for="int-name">名称</Label>
          <Input id="int-name" v-model="name" placeholder="诊断 Agent" autocomplete="off" />
        </div>
        <div class="grid grid-cols-[1fr_auto] gap-4">
          <div class="grid gap-2">
            <Label for="wh-url">URL</Label>
            <Input id="wh-url" v-model="f.url" placeholder="http://agent:8000/webhooks/alertflow" autocomplete="off" />
          </div>
          <div class="grid w-28 gap-2">
            <Label>方法</Label>
            <Select v-model="f.method">
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="POST">POST</SelectItem>
                <SelectItem value="PUT">PUT</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>
        <div class="grid gap-2">
          <Label for="wh-headers">请求头</Label>
          <Textarea
            id="wh-headers"
            v-model="f.headers"
            class="min-h-16 font-mono text-xs"
            placeholder="Authorization: Bearer token"
          />
        </div>
        <div class="flex items-start justify-between gap-4 rounded-lg border border-border px-3 py-2.5">
          <div class="min-w-0">
            <Label for="notify-warnings" class="cursor-pointer">警告时通知</Label>
            <p class="mt-0.5 text-xs text-muted-foreground">警告指检查失败后重试恢复。默认关闭——只发送故障。</p>
          </div>
          <Switch id="notify-warnings" v-model="notifyWarnings" class="mt-0.5 shrink-0" />
        </div>
      </form>

      <DialogFooter>
        <Button variant="ghost" @click="emit('update:open', false)">取消</Button>
        <Button :disabled="!valid" @click="submit">
          {{ isEdit ? '保存修改' : '添加 Agent' }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
