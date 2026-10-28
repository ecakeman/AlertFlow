<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { Plus, Send, MessageSquare, Mail, Webhook, Bot, LoaderCircle, Plug, MoreHorizontal, Pencil, Trash2, SendHorizontal } from '@lucide/vue'
import PageHeader from '@/components/layout/PageHeader.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import IntegrationFormDialog from '@/components/integrations/IntegrationFormDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import {
  DropdownMenu,
  DropdownMenuTrigger,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
} from '@/components/ui/dropdown-menu'
import { toast } from '@/components/ui/sonner'
import { useIntegrationsStore, type IntegrationInput } from '@/stores/integrations'
import { ApiError } from '@/api'
import type { Integration, IntegrationType } from '@/types'

const store = useIntegrationsStore()

const meta: Record<IntegrationType, { label: string; icon: typeof Send; class: string }> = {
  telegram: { label: 'Telegram', icon: Send, class: 'bg-sky-500/15 text-sky-500' },
  slack: { label: 'Slack', icon: MessageSquare, class: 'bg-fuchsia-500/15 text-fuchsia-500' },
  email: { label: '邮件', icon: Mail, class: 'bg-amber-500/15 text-amber-500' },
  webhook: { label: 'Webhook', icon: Webhook, class: 'bg-emerald-500/15 text-emerald-500' },
  agent_webhook: { label: '诊断 Agent', icon: Bot, class: 'bg-emerald-500/15 text-emerald-500' },
}

function retired(it: Integration) {
  return it.type !== 'agent_webhook'
}

const formOpen = ref(false)
const editing = ref<Integration | null>(null)
const confirmOpen = ref(false)
const deleting = ref<Integration | null>(null)
const testingId = ref<number | null>(null)

function errMsg(e: unknown) {
  return e instanceof ApiError ? e.message : '出错了'
}

onMounted(() => store.fetchIntegrations().catch((e) => toast.error(errMsg(e))))

function openAdd() {
  editing.value = null
  formOpen.value = true
}
function openEdit(it: Integration) {
  editing.value = it
  formOpen.value = true
}
function openDelete(it: Integration) {
  deleting.value = it
  confirmOpen.value = true
}

async function onSubmit(input: IntegrationInput) {
  try {
    if (editing.value) {
      await store.update(editing.value.id, input)
      toast.success('集成已更新')
    } else {
      await store.create(input)
      toast.success('集成已添加')
    }
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onToggle(it: Integration, value: boolean) {
  try {
    // notifyWarnings must be resent: it is a plain overwrite server-side, so
    // omitting it here would silently clear the opt-in on every toggle.
    await store.update(it.id, {
      type: it.type,
      name: it.name,
      enabled: value,
      notifyWarnings: it.notifyWarnings,
      config: {},
    })
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onDelete() {
  if (!deleting.value) return
  try {
    await store.remove(deleting.value.id)
    toast.success('集成已移除')
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onTest(it: Integration) {
  testingId.value = it.id
  try {
    const res = await store.test(it.id)
    if (res.ok) toast.success(`测试已发送到 ${it.name}`)
    else toast.error(`测试失败：${res.error ?? '未知错误'}`)
  } catch (e) {
    toast.error(errMsg(e))
  } finally {
    testingId.value = null
  }
}
</script>

<template>
  <div class="mx-auto max-w-[1000px] px-6 py-6 lg:px-8">
    <PageHeader
      title="集成"
      description="故障打开时向诊断 Agent 发送调查任务。Slack、Telegram、邮件和通用 Webhook 已退役。"
    >
      <template #actions>
        <Button @click="openAdd">
          <Plus />添加 Agent</Button>
      </template>
    </PageHeader>

    <div v-if="store.loading && !store.loaded" class="flex justify-center py-24 text-muted-foreground">
      <LoaderCircle class="size-5 animate-spin" />
    </div>

    <EmptyState
      v-else-if="store.integrations.length === 0"
      :icon="Plug"
      title="暂无集成"
      description="添加诊断 Agent。历史外发渠道仍会列在这里，但不再发送。"
      class="mt-10"
    >
      <Button @click="openAdd">
        <Plus />添加 Agent</Button>
    </EmptyState>

    <div v-else class="mt-6 grid gap-3">
      <div
        v-for="it in store.integrations"
        :key="it.id"
        class="flex items-center gap-4 rounded-xl border border-border bg-card p-4"
      >
        <span :class="['flex size-10 shrink-0 items-center justify-center rounded-lg', meta[it.type].class]">
          <component :is="meta[it.type].icon" class="size-5" />
        </span>
        <div class="min-w-0 flex-1">
          <p class="truncate text-sm font-semibold">{{ it.name }}</p>
          <p class="text-xs text-muted-foreground">
            {{ meta[it.type].label }} · {{ retired(it) ? '已退役' : it.enabled ? 'Enabled' : 'Disabled' }}
          </p>
        </div>
        <Button v-if="!retired(it)" variant="outline" size="sm" :disabled="testingId === it.id" @click="onTest(it)">
          <LoaderCircle v-if="testingId === it.id" class="animate-spin" />
          <SendHorizontal v-else />发送测试</Button>
        <Switch v-if="!retired(it)" :model-value="it.enabled" @update:model-value="(v: boolean) => onToggle(it, v)" />
        <DropdownMenu>
          <DropdownMenuTrigger
            class="flex size-8 items-center justify-center rounded-md text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/40"
            aria-label="集成操作"
          >
            <MoreHorizontal class="size-4" />
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" class="w-40">
            <DropdownMenuItem v-if="!retired(it)" @select="openEdit(it)">
              <Pencil />编辑</DropdownMenuItem>
            <DropdownMenuSeparator v-if="!retired(it)" />
            <DropdownMenuItem variant="destructive" @select="openDelete(it)">
              <Trash2 />删除</DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </div>

    <IntegrationFormDialog v-model:open="formOpen" :integration="editing" @submit="onSubmit" />
    <ConfirmDialog
      v-model:open="confirmOpen"
      :title="`删除 ${deleting?.name ?? '集成'}？`"
      description="该渠道将不再接收事件通知。"
      confirm-label="删除"
      destructive
      @confirm="onDelete"
    />
  </div>
</template>
