<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { Plus, Pencil, Trash2, ArrowUpRight, Boxes } from '@lucide/vue'
import PageHeader from '@/components/layout/PageHeader.vue'
import ServiceIcon from '@/components/dashboard/ServiceIcon.vue'
import StatusDot from '@/components/dashboard/StatusDot.vue'
import ServiceFormDialog from '@/components/services/ServiceFormDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Tooltip, TooltipTrigger, TooltipContent } from '@/components/ui/tooltip'
import { toast } from '@/components/ui/sonner'
import { useServicesStore, type ServiceInput } from '@/stores/services'
import { ApiError } from '@/api'
import type { Service } from '@/types'
import { prettyUrl, statusLabel } from '@/lib/format'

const services = useServicesStore()

onMounted(() => {
  if (!services.loaded) services.fetchServices()
})

function errMsg(e: unknown) {
  return e instanceof ApiError ? e.message : '出错了'
}

const formOpen = ref(false)
const formService = ref<Service | null>(null)
const confirmOpen = ref(false)
const confirmService = ref<Service | null>(null)

const modeLabel: Record<string, string> = { icon: 'Icon', name: '名称', dashboard: '仪表盘' }

function openAdd() {
  formService.value = null
  formOpen.value = true
}
function openEdit(s: Service) {
  formService.value = s
  formOpen.value = true
}
function openDelete(s: Service) {
  confirmService.value = s
  confirmOpen.value = true
}
async function onSubmit(input: ServiceInput) {
  try {
    if (formService.value) {
      await services.updateService(formService.value.id, input)
      toast.success('服务已更新')
    } else {
      await services.addService(input)
      toast.success('服务已添加')
    }
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onConfirmDelete() {
  if (!confirmService.value) return
  const name = confirmService.value.name
  try {
    await services.removeService(confirmService.value.id)
    toast.success(`已移除 ${name}`)
  } catch (e) {
    toast.error(errMsg(e))
  }
}
</script>

<template>
  <div class="mx-auto max-w-5xl px-6 py-6 lg:px-8">
    <PageHeader title="资源" description="管理你监控的服务。保存至 config.yaml。">
      <template #actions>
        <Button v-if="services.hasServices" @click="openAdd">
          <Plus />添加服务</Button>
      </template>
    </PageHeader>

    <div v-if="services.hasServices" class="mt-6 overflow-hidden rounded-xl border border-border bg-card">
      <table class="w-full text-sm">
        <thead>
          <tr class="border-b border-border text-left text-xs text-muted-foreground">
            <th class="px-4 py-3 font-medium">服务</th>
            <th class="hidden px-4 py-3 font-medium sm:table-cell">间隔</th>
            <th class="hidden px-4 py-3 font-medium md:table-cell">模式</th>
            <th class="px-4 py-3 font-medium">状态</th>
            <th class="px-4 py-3 text-right font-medium">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="s in services.services"
            :key="s.id"
            class="border-b border-border/60 transition-colors last:border-0 hover:bg-accent/40"
          >
            <td class="px-4 py-3">
              <div class="flex items-center gap-3">
                <ServiceIcon :service="s" class="size-8 shrink-0" />
                <div class="min-w-0">
                  <p class="truncate font-medium">{{ s.name }}</p>
                  <p class="truncate text-xs text-muted-foreground">{{ prettyUrl(s.url) }}</p>
                </div>
              </div>
            </td>
            <td class="hidden px-4 py-3 tabular-nums text-muted-foreground sm:table-cell">
              {{ s.check.interval }}s
            </td>
            <td class="hidden px-4 py-3 md:table-cell">
              <Badge variant="secondary">{{ modeLabel[s.widget.mode] }}</Badge>
            </td>
            <td class="px-4 py-3">
              <span class="inline-flex items-center gap-1.5">
                <StatusDot :status="s.status" :pulse="false" />
                <span class="text-xs">{{ statusLabel(s.status) }}</span>
              </span>
            </td>
            <td class="px-4 py-3">
              <div class="flex items-center justify-end gap-1">
                <Tooltip>
                  <TooltipTrigger as-child>
                    <Button variant="ghost" size="icon-sm" as-child>
                      <a :href="s.url" target="_blank" rel="noopener" aria-label="打开服务">
                        <ArrowUpRight />
                      </a>
                    </Button>
                  </TooltipTrigger>
                  <TooltipContent>打开</TooltipContent>
                </Tooltip>
                <Button variant="ghost" size="icon-sm" aria-label="编辑" @click="openEdit(s)">
                  <Pencil />
                </Button>
                <Button
                  variant="ghost"
                  size="icon-sm"
                  class="text-muted-foreground hover:text-offline"
                  aria-label="删除"
                  @click="openDelete(s)"
                >
                  <Trash2 />
                </Button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <div v-else class="mt-6 rounded-xl border border-border bg-card">
      <EmptyState
        :icon="Boxes"
        title="暂无服务"
        description="添加第一个服务即可开始监控。"
      >
        <Button @click="openAdd">
          <Plus />添加第一个服务</Button>
      </EmptyState>
    </div>

    <ServiceFormDialog v-model:open="formOpen" :service="formService" @submit="onSubmit" />
    <ConfirmDialog
      v-model:open="confirmOpen"
      :title="`删除 ${confirmService?.name ?? '服务'}？`"
      description="将移除该服务及其历史记录，且无法撤销。"
      confirm-label="删除"
      destructive
      @confirm="onConfirmDelete"
    />
  </div>
</template>
