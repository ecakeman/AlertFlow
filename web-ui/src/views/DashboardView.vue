<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { Plus, Boxes, LoaderCircle, Pencil } from '@lucide/vue'
import ServiceGrid from '@/components/dashboard/ServiceGrid.vue'
import ServiceFormDialog from '@/components/services/ServiceFormDialog.vue'
import IconGeneratorDialog from '@/components/services/IconGeneratorDialog.vue'
import WarningPeriodDialog from '@/components/dashboard/WarningPeriodDialog.vue'
import UptimeServicesDialog from '@/components/dashboard/UptimeServicesDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import { Button } from '@/components/ui/button'
import { toast } from '@/components/ui/sonner'
import { useServicesStore, type ServiceInput } from '@/stores/services'
import { useAuthStore } from '@/stores/auth'
import { useSettingsStore } from '@/stores/settings'
import { useServicesPolling } from '@/composables/useServicesPolling'
import { optimizeToWebP, svgToWebP } from '@/lib/image'
import { api, ApiError } from '@/api'
import type { ChartType, DashboardStats, Service, WidgetMode } from '@/types'
import { formatUptime } from '@/lib/format'
import { cn } from '@/lib/utils'

const services = useServicesStore()
const auth = useAuthStore()
const settings = useSettingsStore()
useServicesPolling()

const formOpen = ref(false)
const formService = ref<Service | null>(null)
const iconOpen = ref(false)
const iconService = ref<Service | null>(null)
const confirmOpen = ref(false)
const confirmService = ref<Service | null>(null)
const warningPeriodOpen = ref(false)
const uptimeServicesOpen = ref(false)

const fileInput = ref<HTMLInputElement>()
const imageTargetId = ref<string | null>(null)
const hoveredServiceId = ref<string | null>(null)

// The "警告" tile's period-based count comes from a separate endpoint since
// it depends on an admin-configurable look-back window rather than a service's
// current live status (which services.warningCount still reflects, e.g. for
// the sidebar badge).
const dashboardStats = ref<DashboardStats | null>(null)
async function loadDashboardStats() {
  try {
    dashboardStats.value = await api.dashboardStats()
  } catch {
    /* non-fatal: falls back to the live warning count below */
  }
}
let statsTimer: ReturnType<typeof setInterval> | undefined

const showEmpty = computed(() => services.loaded && !services.hasServices)
const stats = computed(() => [
  { key: 'services', label: '服务', value: String(services.services.length) },
  { key: 'online', label: '在线', value: String(services.onlineCount), dot: 'bg-online' },
  { key: 'offline', label: '离线', value: String(services.offlineCount), dot: 'bg-offline' },
  {
    key: 'warning',
    label: '警告',
    value: String(dashboardStats.value?.warningServiceCount ?? services.warningCount),
    dot: 'bg-warning',
  },
  { key: 'avgUptime', label: '平均在线率', value: formatUptime(services.avgUptime) },
])

function errMsg(e: unknown) {
  return e instanceof ApiError ? e.message : '出错了'
}

async function onSaveWarningPeriod(hours: number) {
  try {
    await settings.update({ dashboard: { ...settings.settings.dashboard, warningPeriodHours: hours } })
    await loadDashboardStats()
    toast.success('警告回看期已更新')
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onSaveUptimeServices(excludedIds: string[]) {
  try {
    await settings.update({
      dashboard: { ...settings.settings.dashboard, uptimeExcludedServices: excludedIds },
    })
    toast.success('平均在线率服务已更新')
  } catch (e) {
    toast.error(errMsg(e))
  }
}

function openAdd() {
  formService.value = null
  formOpen.value = true
}
function openEdit(id: string) {
  formService.value = services.getById(id) ?? null
  formOpen.value = true
}
function openGenerate(id: string) {
  iconService.value = services.getById(id) ?? null
  iconOpen.value = true
}
function openReplace(id: string) {
  imageTargetId.value = id
  fileInput.value?.click()
}
function openDelete(id: string) {
  confirmService.value = services.getById(id) ?? null
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
async function onApplyIcon(svg: string) {
  if (!iconService.value) return
  try {
    const blob = await svgToWebP(svg)
    await services.uploadIcon(iconService.value.id, blob)
    toast.success('图标已应用')
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
async function uploadImageFor(id: string, file: File) {
  try {
    const blob = await optimizeToWebP(file)
    await services.uploadIcon(id, blob)
    toast.success('图标已更新')
  } catch (err) {
    toast.error(errMsg(err))
  }
}
async function onFileChange(e: Event) {
  const input = e.target as HTMLInputElement
  const file = input.files?.[0]
  if (file && imageTargetId.value) await uploadImageFor(imageTargetId.value, file)
  input.value = ''
}
async function onSetWidgetMode(id: string, mode: WidgetMode) {
  try {
    await services.setWidgetMode(id, mode)
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onSetChartType(id: string, type: ChartType) {
  try {
    await services.setChartType(id, type)
  } catch (e) {
    toast.error(errMsg(e))
  }
}
function onHover(id: string, entering: boolean) {
  if (entering) hoveredServiceId.value = id
  else if (hoveredServiceId.value === id) hoveredServiceId.value = null
}

// Ctrl+V an image over the hovered card to replace its icon.
function onPaste(e: ClipboardEvent) {
  const el = document.activeElement as HTMLElement | null
  if (el && (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.isContentEditable)) return
  const id = hoveredServiceId.value
  if (!id) return
  for (const item of e.clipboardData?.items ?? []) {
    if (item.type.startsWith('image/')) {
      const file = item.getAsFile()
      if (file) {
        e.preventDefault()
        uploadImageFor(id, file)
      }
      return
    }
  }
}
onMounted(() => {
  window.addEventListener('paste', onPaste)
  if (!settings.loaded) settings.fetch().catch(() => {})
  loadDashboardStats()
  statsTimer = setInterval(loadDashboardStats, 10_000)
})
onUnmounted(() => {
  window.removeEventListener('paste', onPaste)
  if (statsTimer) clearInterval(statsTimer)
})
</script>

<template>
  <div class="mx-auto max-w-[1600px] px-6 py-6 lg:px-8">
    <header class="flex flex-wrap items-end justify-between gap-4">
      <div>
        <h2 class="text-2xl font-semibold tracking-tight">仪表盘</h2>
        <p class="mt-1 text-sm text-muted-foreground">服务的实时状态 · 拖拽调整位置，从角落调整大小。</p>
      </div>
      <Button v-if="services.hasServices && auth.isAdmin" @click="openAdd">
        <Plus />添加服务</Button>
    </header>

    <!-- Initial loading -->
    <div v-if="!services.loaded" class="flex items-center justify-center py-24 text-muted-foreground">
      <LoaderCircle class="size-5 animate-spin" />
    </div>

    <template v-else>
      <div v-if="services.hasServices" class="mt-6 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
        <div
          v-for="stat in stats"
          :key="stat.label"
          class="group relative rounded-xl border border-border bg-card px-4 py-3"
        >
          <div class="flex items-center gap-1.5">
            <span v-if="stat.dot" :class="cn('size-2 rounded-full', stat.dot)" />
            <p class="text-xs text-muted-foreground">{{ stat.label }}</p>
          </div>
          <p class="mt-1 text-xl font-semibold tabular-nums">{{ stat.value }}</p>
          <button
            v-if="auth.isAdmin && settings.loaded && stat.key === 'warning'"
            class="absolute top-2.5 right-2.5 cursor-pointer text-muted-foreground opacity-0 transition-opacity hover:text-foreground group-hover:opacity-100"
            title="配置警告回看期"
            @click="warningPeriodOpen = true"
          >
            <Pencil class="size-3.5" />
          </button>
          <button
            v-if="auth.isAdmin && settings.loaded && stat.key === 'avgUptime'"
            class="absolute top-2.5 right-2.5 cursor-pointer text-muted-foreground opacity-0 transition-opacity hover:text-foreground group-hover:opacity-100"
            title="配置计入的服务"
            @click="uptimeServicesOpen = true"
          >
            <Pencil class="size-3.5" />
          </button>
        </div>
      </div>

      <div class="mt-4">
        <ServiceGrid
          v-if="services.hasServices"
          class="-mx-2"
          :readonly="!auth.isAdmin"
          @edit="openEdit"
          @replace-image="openReplace"
          @generate-icon="openGenerate"
          @remove="openDelete"
          @set-widget-mode="onSetWidgetMode"
          @set-chart-type="onSetChartType"
          @drop-image="uploadImageFor"
          @hover="onHover"
        />
        <EmptyState
          v-else-if="showEmpty"
          :icon="Boxes"
          title="暂无服务"
          :description="
            auth.isAdmin
              ? '添加第一个服务，开始监控其状态、响应时间与在线率。'
              : '尚未添加任何服务。'
          "
        >
          <Button v-if="auth.isAdmin" @click="openAdd">
            <Plus />添加第一个服务</Button>
        </EmptyState>
      </div>
    </template>

    <input ref="fileInput" type="file" accept="image/*" class="hidden" @change="onFileChange" />

    <ServiceFormDialog v-model:open="formOpen" :service="formService" @submit="onSubmit" />
    <IconGeneratorDialog v-model:open="iconOpen" :service="iconService" @apply="onApplyIcon" />
    <ConfirmDialog
      v-model:open="confirmOpen"
      :title="`删除 ${confirmService?.name ?? '服务'}？`"
      description="将移除该服务及其历史记录，且无法撤销。"
      confirm-label="删除"
      destructive
      @confirm="onConfirmDelete"
    />
    <WarningPeriodDialog
      v-model:open="warningPeriodOpen"
      :hours="settings.settings.dashboard.warningPeriodHours"
      @submit="onSaveWarningPeriod"
    />
    <UptimeServicesDialog
      v-model:open="uptimeServicesOpen"
      :services="services.services"
      :excluded-ids="settings.settings.dashboard.uptimeExcludedServices"
      @submit="onSaveUptimeServices"
    />
  </div>
</template>
