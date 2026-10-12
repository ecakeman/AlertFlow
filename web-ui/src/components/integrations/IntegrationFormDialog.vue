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
import type { Integration, IntegrationType } from '@/types'
import type { IntegrationInput } from '@/stores/integrations'

const props = defineProps<{ open: boolean; integration?: Integration | null }>()
const emit = defineEmits<{ 'update:open': [boolean]; submit: [IntegrationInput] }>()

const isEdit = computed(() => !!props.integration)

const type = ref<IntegrationType>('telegram')
const name = ref('')
const enabled = ref(true)
const notifyWarnings = ref(false)

// One flat bag of every possible field; only the relevant ones are shown/sent.
const f = reactive({
  botToken: '',
  chatId: '',
  webhookUrl: '',
  host: '',
  port: '587',
  username: '',
  password: '',
  from: '',
  to: '',
  url: '',
  method: 'POST',
  headers: '',
  bodyTemplate: '',
})

const secrets = computed(() => props.integration?.secrets ?? {})
function secretPlaceholder(field: string, fallback: string) {
  return secrets.value[field] ? '留空则保持当前值' : fallback
}

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
  type.value = it?.type ?? 'telegram'
  name.value = it?.name ?? ''
  enabled.value = it?.enabled ?? true
  notifyWarnings.value = it?.notifyWarnings ?? false
  const c = (it?.config ?? {}) as Record<string, unknown>
  f.botToken = ''
  f.chatId = str(c.chatId)
  f.webhookUrl = ''
  f.host = str(c.host)
  f.port = c.port != null ? str(c.port) : '587'
  f.username = str(c.username)
  f.password = ''
  f.from = str(c.from)
  f.to = str(c.to)
  f.url = str(c.url)
  f.method = str(c.method) || 'POST'
  f.headers = headersToText(c.headers)
  f.bodyTemplate = str(c.bodyTemplate)
}
watch(
  () => props.open,
  (o) => o && reset(),
)

const valid = computed(() => {
  if (name.value.trim() === '') return false
  switch (type.value) {
    case 'telegram':
      return f.chatId.trim() !== '' && (isEdit.value || f.botToken.trim() !== '')
    case 'slack':
      return isEdit.value || f.webhookUrl.trim() !== ''
    case 'email':
      return f.host.trim() !== '' && f.from.trim() !== '' && f.to.trim() !== ''
    case 'webhook':
      return f.url.trim() !== ''
  }
  return false
})

function buildConfig(): Record<string, unknown> {
  switch (type.value) {
    case 'telegram':
      return { botToken: f.botToken, chatId: f.chatId.trim() }
    case 'slack':
      return { webhookUrl: f.webhookUrl }
    case 'email':
      return {
        host: f.host.trim(),
        port: Number(f.port) || 587,
        username: f.username.trim(),
        password: f.password,
        from: f.from.trim(),
        to: f.to.trim(),
      }
    case 'webhook':
      return {
        url: f.url.trim(),
        method: f.method,
        headers: parseHeaders(f.headers),
        bodyTemplate: f.bodyTemplate,
      }
  }
  return {}
}

function submit() {
  if (!valid.value) return
  emit('submit', {
    type: type.value,
    name: name.value.trim(),
    enabled: enabled.value,
    notifyWarnings: notifyWarnings.value,
    config: buildConfig(),
  })
  emit('update:open', false)
}
</script>

<template>
  <Dialog :open="open" @update:open="emit('update:open', $event)">
    <DialogContent>
      <DialogHeader>
        <DialogTitle>{{ isEdit ? '编辑集成' : '添加集成' }}</DialogTitle>
        <DialogDescription>当事件开始或恢复时通知渠道——若启用，也在检查重试恢复时通知。</DialogDescription>
      </DialogHeader>

      <form class="grid gap-4" @submit.prevent="submit">
        <div class="grid grid-cols-2 gap-4">
          <div class="grid gap-2">
            <Label>类型</Label>
            <Select v-model="type" :disabled="isEdit">
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="telegram">Telegram</SelectItem>
                <SelectItem value="slack">Slack</SelectItem>
                <SelectItem value="email">邮件 (SMTP)</SelectItem>
                <SelectItem value="webhook">自定义 Webhook</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div class="grid gap-2">
            <Label for="int-name">名称</Label>
            <Input id="int-name" v-model="name" placeholder="运维渠道" autocomplete="off" />
          </div>
        </div>

        <!-- Telegram -->
        <template v-if="type === 'telegram'">
          <div class="grid gap-2">
            <Label for="tg-token">机器人 Token</Label>
            <Input
              id="tg-token"
              v-model="f.botToken"
              type="password"
              :placeholder="secretPlaceholder('botToken', '123456:ABC-DEF…')"
              autocomplete="off"
            />
          </div>
          <div class="grid gap-2">
            <Label for="tg-chat">聊天 ID</Label>
            <Input id="tg-chat" v-model="f.chatId" placeholder="-1001234567890" autocomplete="off" />
          </div>
        </template>

        <!-- Slack -->
        <template v-else-if="type === 'slack'">
          <div class="grid gap-2">
            <Label for="sl-url">Webhook URL</Label>
            <Input
              id="sl-url"
              v-model="f.webhookUrl"
              type="password"
              :placeholder="secretPlaceholder('webhookUrl', 'https://hooks.slack.com/services/…')"
              autocomplete="off"
            />
          </div>
        </template>

        <!-- Email -->
        <template v-else-if="type === 'email'">
          <div class="grid grid-cols-[1fr_auto] gap-4">
            <div class="grid gap-2">
              <Label for="em-host">SMTP 主机</Label>
              <Input id="em-host" v-model="f.host" placeholder="smtp.gmail.com" autocomplete="off" />
            </div>
            <div class="grid w-24 gap-2">
              <Label for="em-port">端口</Label>
              <Input id="em-port" v-model="f.port" type="number" placeholder="587" />
            </div>
          </div>
          <div class="grid grid-cols-2 gap-4">
            <div class="grid gap-2">
              <Label for="em-user">用户名</Label>
              <Input id="em-user" v-model="f.username" autocomplete="off" />
            </div>
            <div class="grid gap-2">
              <Label for="em-pass">密码</Label>
              <Input
                id="em-pass"
                v-model="f.password"
                type="password"
                :placeholder="secretPlaceholder('password', '')"
                autocomplete="off"
              />
            </div>
          </div>
          <div class="grid grid-cols-2 gap-4">
            <div class="grid gap-2">
              <Label for="em-from">发件人</Label>
              <Input id="em-from" v-model="f.from" placeholder="alerts@x.com" autocomplete="off" />
            </div>
            <div class="grid gap-2">
              <Label for="em-to">To</Label>
              <Input id="em-to" v-model="f.to" placeholder="ops@x.com" autocomplete="off" />
            </div>
          </div>
          <p class="text-xs text-muted-foreground">在指定端口使用 STARTTLS（推荐 587）。多个收件人用逗号分隔。</p>
        </template>

        <!-- Webhook -->
        <template v-else>
          <div class="grid grid-cols-[1fr_auto] gap-4">
            <div class="grid gap-2">
              <Label for="wh-url">URL</Label>
              <Input id="wh-url" v-model="f.url" placeholder="https://example.com/hook" autocomplete="off" />
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
                  <SelectItem value="PATCH">PATCH</SelectItem>
                  <SelectItem value="GET">GET</SelectItem>
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
              placeholder="Authorization: Bearer token&#10;X-Source: AlertFlow"
            />
          </div>
          <div class="grid gap-2">
            <Label for="wh-body">请求体模板</Label>
            <Textarea
              id="wh-body"
              v-model="f.bodyTemplate"
              class="min-h-16 font-mono text-xs"
              placeholder="留空则使用默认 JSON 负载。支持 {{.ServiceName}}、{{.Event}}。"
            />
          </div>
        </template>

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
          {{ isEdit ? '保存修改' : '添加集成' }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
