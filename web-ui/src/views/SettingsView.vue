<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import {
  Globe,
  Users,
  FolderCog,
  Upload,
  Download,
  Sun,
  Moon,
  Trash2,
  UserPlus,
  LayoutGrid,
} from '@lucide/vue'
import PageHeader from '@/components/layout/PageHeader.vue'
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from '@/components/ui/select'
import { toast } from '@/components/ui/sonner'
import { useSettingsStore } from '@/stores/settings'
import { useUiStore } from '@/stores/ui'
import { useAuthStore } from '@/stores/auth'
import { api, ApiError } from '@/api'
import type { AgentSettings, UserRole, WidgetMode } from '@/types'
import { initials } from '@/lib/format'
import { cn } from '@/lib/utils'

const settings = useSettingsStore()
const ui = useUiStore()
const auth = useAuthStore()

const configPath = ref('')
const importInput = ref<HTMLInputElement>()
const newUsername = ref('')
const newPassword = ref('')
const newRole = ref<UserRole>('readonly')
const busy = ref(false)

/** 诊断 Agent / 检索配置。密钥留空或保持打码＝沿用已存的值（后端按字段合并）。 */
function initAgentForm(): AgentSettings {
  const agent = settings.settings.agent
  return {
    baseUrl: agent?.baseUrl ?? '',
    apiToken: '',
    llm: { baseUrl: agent?.llm?.baseUrl ?? '', model: agent?.llm?.model ?? '', apiKey: '' },
    embed: {
      baseUrl: agent?.embed?.baseUrl ?? '',
      model: agent?.embed?.model ?? '',
      apiKey: '',
      dim: agent?.embed?.dim ?? 1024,
    },
    rerank: {
      baseUrl: agent?.rerank?.baseUrl ?? '',
      model: agent?.rerank?.model ?? '',
      apiKey: '',
      style: agent?.rerank?.style ?? 'dashscope',
    },
    memoryEnabled: agent?.memoryEnabled ?? true,
    contextBudgetTokens: agent?.contextBudgetTokens ?? 0,
  }
}
const agentForm = ref<AgentSettings>(initAgentForm())

async function saveAgent() {
  busy.value = true
  try {
    await api.updateAgentSettings(agentForm.value)
    // 保存后清掉刚输入的密钥：服务端会打码回显，避免在表单里留明文。
    agentForm.value = initAgentForm()
    toast.success('诊断 Agent 配置已保存')
  } catch (e) {
    toast.error(e instanceof ApiError ? e.message : '保存失败')
  } finally {
    busy.value = false
  }
}

const themeOptions = [
  { value: 'light' as const, icon: Sun, label: '浅色' },
  { value: 'dark' as const, icon: Moon, label: '深色' },
]

onMounted(async () => {
  try {
    await Promise.all([settings.fetch(), auth.fetchUsers()])
    configPath.value = settings.settings.configDir
  } catch (e) {
    toast.error(errMsg(e))
  }
})

function errMsg(e: unknown) {
  return e instanceof ApiError ? e.message : '出错了'
}

async function setDefaultMode(value: WidgetMode) {
  try {
    await settings.update({ defaultWidgetMode: value })
  } catch (e) {
    toast.error(errMsg(e))
  }
}

// Retry defaults are saved on blur rather than per keystroke: each save is a
// full-object PUT that rewrites config.yaml.
const retryAttempts = ref(String(settings.settings.check.retryAttempts))
const retryDelays = ref(settings.settings.check.retryDelays.join(', '))
watch(
  () => settings.settings.check,
  (check) => {
    retryAttempts.value = String(check.retryAttempts)
    retryDelays.value = check.retryDelays.join(', ')
  },
)

async function saveRetryDefaults() {
  const attempts = Number(retryAttempts.value) || 1
  const delays = retryDelays.value
    .split(',')
    .map((part) => Number(part.trim()))
    .filter((n) => Number.isFinite(n) && n >= 0)
  if (
    attempts === settings.settings.check.retryAttempts &&
    delays.join(',') === settings.settings.check.retryDelays.join(',')
  ) {
    return
  }
  try {
    await settings.update({
      check: { ...settings.settings.check, retryAttempts: attempts, retryDelays: delays },
    })
    toast.success('重试默认值已保存')
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function applyConfigPath() {
  const path = configPath.value.trim()
  if (!path) return
  busy.value = true
  try {
    await settings.setConfigPath(path)
    toast.success('配置目录已更新——正在重载…')
    setTimeout(() => window.location.reload(), 600)
  } catch (e) {
    toast.error(errMsg(e))
    busy.value = false
  }
}
async function exportConfig() {
  try {
    const blob = await api.exportConfig()
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'alertflow-backup.zip'
    a.click()
    URL.revokeObjectURL(url)
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onImport(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  input.value = ''
  if (!file) return
  busy.value = true
  try {
    const res = await api.importConfig(file)
    toast.success(`已导入 ${res.services} 个服务——正在重载…`)
    setTimeout(() => window.location.reload(), 700)
  } catch (err) {
    toast.error(errMsg(err))
    busy.value = false
  }
}
async function addUser() {
  const name = newUsername.value.trim()
  if (!name || newPassword.value.length < 8) return
  try {
    await auth.addUser(name, newPassword.value, newRole.value)
    toast.success(`已邀请 ${name}`)
    newUsername.value = ''
    newPassword.value = ''
    newRole.value = 'readonly'
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function removeUser(id: number, name: string) {
  try {
    await auth.removeUser(id)
    toast.success(`Removed ${name}`)
  } catch (e) {
    toast.error(errMsg(e))
  }
}
</script>

<template>
  <div class="mx-auto max-w-3xl px-6 py-6 lg:px-8">
    <PageHeader title="设置" description="访问、用户与配置。" />

    <Tabs default-value="general" class="mt-6">
      <TabsList class="mb-6">
        <TabsTrigger value="general"><Globe />通用</TabsTrigger>
        <TabsTrigger value="users"><Users />用户</TabsTrigger>
        <TabsTrigger value="config"><FolderCog />配置</TabsTrigger>
      </TabsList>

      <!-- General -->
      <TabsContent value="general" class="grid gap-3">
        <div class="flex items-center justify-between gap-4 rounded-xl border border-border bg-card p-4">
          <div class="min-w-0">
            <p class="text-sm font-medium">外观</p>
            <p class="mt-0.5 text-xs text-muted-foreground">选择界面主题。</p>
          </div>
          <div class="inline-flex shrink-0 rounded-lg bg-muted p-1">
            <button
              v-for="opt in themeOptions"
              :key="opt.value"
              type="button"
              :class="
                cn(
                  'flex items-center gap-1.5 rounded-md px-3 py-1.5 text-sm font-medium transition-all',
                  ui.theme === opt.value
                    ? 'bg-card text-foreground shadow-elevation-low'
                    : 'text-muted-foreground hover:text-foreground',
                )
              "
              @click="ui.theme = opt.value"
            >
              <component :is="opt.icon" class="size-4" />
              {{ opt.label }}
            </button>
          </div>
        </div>

        <div class="rounded-xl border border-border bg-card p-4">
          <div class="min-w-0">
            <p class="text-sm font-medium">检查重试</p>
            <p class="mt-0.5 text-xs text-muted-foreground">新服务及未单独设置的服务默认值。检查失败后重试恢复记为警告，而非故障。</p>
          </div>
          <div class="mt-3 grid grid-cols-2 gap-4">
            <div class="grid gap-2">
              <Label for="set-retries">尝试次数</Label>
              <Input
                id="set-retries"
                v-model="retryAttempts"
                type="number"
                min="1"
                max="10"
                @blur="saveRetryDefaults"
              />
              <p class="text-xs text-muted-foreground">1 disables retries.</p>
            </div>
            <div class="grid gap-2">
              <Label for="set-delays">重试延迟</Label>
              <Input
                id="set-delays"
                v-model="retryDelays"
                placeholder="1, 5, 10"
                autocomplete="off"
                @blur="saveRetryDefaults"
              />
              <p class="text-xs text-muted-foreground">每次尝试之间的秒数。</p>
            </div>
          </div>
        </div>

        <div class="rounded-xl border border-border bg-card p-4">
          <div class="min-w-0">
            <p class="text-sm font-medium">诊断 Agent / 检索</p>
            <p class="mt-0.5 text-xs text-muted-foreground">
              模型端点与密钥保存在本机配置里。密钥读取时打码，留空表示不修改。
            </p>
          </div>
          <div class="mt-3 grid grid-cols-2 gap-4">
            <div class="grid gap-2">
              <Label for="agent-base">Agent API 地址</Label>
              <Input id="agent-base" v-model="agentForm.baseUrl" placeholder="http://agent:8000" autocomplete="off" />
            </div>
            <div class="grid gap-2">
              <Label for="agent-token">Agent API Token</Label>
              <Input id="agent-token" v-model="agentForm.apiToken" type="password" placeholder="留空＝不修改" autocomplete="off" />
            </div>
          </div>
          <div class="mt-3 grid gap-2">
            <Label>LLM（诊断与对话）</Label>
            <div class="grid grid-cols-3 gap-2">
              <Input v-model="agentForm.llm.baseUrl" placeholder="base url" autocomplete="off" />
              <Input v-model="agentForm.llm.model" placeholder="model" autocomplete="off" />
              <Input v-model="agentForm.llm.apiKey" type="password" placeholder="api key（留空＝不修改）" autocomplete="off" />
            </div>
          </div>
          <div class="mt-3 grid gap-2">
            <Label>Embedding（记忆 · 向量召回）</Label>
            <div class="grid grid-cols-3 gap-2">
              <Input v-model="agentForm.embed.baseUrl" placeholder="base url" autocomplete="off" />
              <Input v-model="agentForm.embed.model" placeholder="model" autocomplete="off" />
              <Input v-model="agentForm.embed.apiKey" type="password" placeholder="api key（留空＝不修改）" autocomplete="off" />
            </div>
          </div>
          <div class="mt-3 grid gap-2">
            <Label>Rerank（记忆 · 精排）</Label>
            <div class="grid grid-cols-3 gap-2">
              <Input v-model="agentForm.rerank.baseUrl" placeholder="base url" autocomplete="off" />
              <Input v-model="agentForm.rerank.model" placeholder="model" autocomplete="off" />
              <Input v-model="agentForm.rerank.apiKey" type="password" placeholder="api key（留空＝不修改）" autocomplete="off" />
            </div>
          </div>
          <div class="mt-3 flex items-center justify-between gap-4">
            <Label class="flex items-center gap-2">
              <input v-model="agentForm.memoryEnabled" type="checkbox" class="size-4" />
              启用长期记忆（事件簿 + 找回既往同类事件）
            </Label>
            <div class="flex items-center gap-2">
              <Label for="agent-budget" class="text-xs text-muted-foreground">上下文预算 tokens</Label>
              <Input
                id="agent-budget"
                v-model.number="agentForm.contextBudgetTokens"
                type="number"
                min="0"
                placeholder="24000"
                class="w-32"
              />
            </div>
            <Button size="sm" :disabled="busy" @click="saveAgent">保存</Button>
          </div>
        </div>

        <div class="flex items-center justify-between gap-4 rounded-xl border border-border bg-card p-4">
          <div class="min-w-0">
            <p class="text-sm font-medium">默认小组件模式</p>
            <p class="mt-0.5 text-xs text-muted-foreground">新服务使用的布局。</p>
          </div>
          <Select :model-value="settings.settings.defaultWidgetMode" @update:model-value="(v) => setDefaultMode(v as WidgetMode)">
            <SelectTrigger class="w-44">
              <LayoutGrid class="size-4 text-muted-foreground" />
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="icon">仅图标</SelectItem>
              <SelectItem value="name">图标 + 名称</SelectItem>
              <SelectItem value="dashboard">迷你仪表盘</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </TabsContent>

      <!-- Users -->
      <TabsContent value="users" class="grid gap-3">
        <div class="overflow-hidden rounded-xl border border-border bg-card">
          <div
            v-for="u in auth.users"
            :key="u.id"
            class="flex items-center gap-3 border-b border-border/60 p-4 last:border-0"
          >
            <Avatar class="size-9">
              <AvatarFallback class="bg-primary/15 text-primary">{{ initials(u.username) }}</AvatarFallback>
            </Avatar>
            <div class="min-w-0 flex-1">
              <div class="flex items-center gap-2">
                <p class="truncate text-sm font-medium">{{ u.username }}</p>
                <Badge :variant="u.role === 'admin' ? 'default' : 'secondary'">
                  {{ u.role === 'admin' ? '管理员' : '只读' }}
                </Badge>
                <Badge v-if="u.id === auth.currentUser?.id" variant="outline">你</Badge>
              </div>
            </div>
            <Button
              v-if="u.id !== auth.currentUser?.id"
              variant="ghost"
              size="icon-sm"
              class="text-muted-foreground hover:text-offline"
              aria-label="移除用户"
              @click="removeUser(u.id, u.username)"
            >
              <Trash2 />
            </Button>
          </div>
        </div>

        <div class="rounded-xl border border-border bg-card p-4">
          <p class="text-sm font-medium">邀请成员</p>
          <p class="mt-0.5 text-xs text-muted-foreground">为同事创建账户（密码至少 8 个字符）。</p>
          <div class="mt-3 grid gap-2 sm:grid-cols-[1fr_1fr_auto]">
            <Input v-model="newUsername" placeholder="username" autocomplete="off" />
            <Input v-model="newPassword" type="password" placeholder="password" autocomplete="new-password" />
            <Select v-model="newRole">
              <SelectTrigger class="sm:w-36">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="readonly">只读</SelectItem>
                <SelectItem value="admin">管理员</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <div class="mt-2 flex justify-end">
            <Button :disabled="!newUsername.trim() || newPassword.length < 8" @click="addUser">
              <UserPlus />创建账户</Button>
          </div>
        </div>
      </TabsContent>

      <!-- Configuration -->
      <TabsContent value="config" class="grid gap-3">
        <div class="rounded-xl border border-border bg-card p-4">
          <p class="text-sm font-medium">配置目录</p>
          <p class="mt-0.5 text-xs text-muted-foreground">config.yaml、图标与数据库的存放位置。修改后会重载应用。</p>
          <div class="mt-3 flex flex-col gap-2 sm:flex-row">
            <Input v-model="configPath" placeholder="/config" class="flex-1 font-mono text-xs" />
            <Button variant="outline" :disabled="busy" @click="applyConfigPath">应用</Button>
          </div>
        </div>

        <div class="flex flex-wrap items-center justify-between gap-4 rounded-xl border border-border bg-card p-4">
          <div class="min-w-0">
            <p class="text-sm font-medium">备份与恢复</p>
            <p class="mt-0.5 text-xs text-muted-foreground">导出或导入含 config.yaml 与图标的 .zip。导入前会先备份当前配置。</p>
          </div>
          <div class="flex gap-2">
            <Button variant="outline" :disabled="busy" @click="importInput?.click()">
              <Upload />导入</Button>
            <Button variant="outline" @click="exportConfig">
              <Download />导出</Button>
          </div>
        </div>

        <div class="rounded-xl border border-border bg-card p-4">
          <div class="flex items-center justify-between">
            <div>
              <p class="text-sm font-medium">指标保留</p>
              <p class="mt-0.5 text-xs text-muted-foreground">早于该时长的历史会被自动删除。</p>
            </div>
            <Badge variant="secondary">{{ settings.settings.check.retentionDays }} days</Badge>
          </div>
        </div>

        <input ref="importInput" type="file" accept=".zip" class="hidden" @change="onImport" />
      </TabsContent>
    </Tabs>
  </div>
</template>
