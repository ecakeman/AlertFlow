<script setup lang="ts">
import { computed, ref, watch } from 'vue'
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
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from '@/components/ui/select'
import type { Service, WidgetMode } from '@/types'
import type { ServiceInput } from '@/stores/services'

const props = defineProps<{ open: boolean; service?: Service | null }>()
const emit = defineEmits<{ 'update:open': [boolean]; submit: [ServiceInput] }>()

const isEdit = computed(() => !!props.service)

const name = ref('')
const url = ref('')
const interval = ref(30)
const mode = ref<WidgetMode>('name')
const retryAttempts = ref(3)
// Held as text so a half-typed "1, 5," stays editable; parsed on submit.
const retryDelays = ref('1, 5, 10')

function reset() {
  name.value = props.service?.name ?? ''
  url.value = props.service?.url ?? ''
  interval.value = props.service?.check.interval ?? 30
  mode.value = props.service?.widget.mode ?? 'name'
  retryAttempts.value = props.service?.check.retryAttempts ?? 3
  retryDelays.value = (props.service?.check.retryDelays ?? [1, 5, 10]).join(', ')
}
watch(
  () => props.open,
  (o) => o && reset(),
)

const valid = computed(() => name.value.trim().length > 0 && /^https?:\/\/.+/.test(url.value.trim()))

/** "1, 5, 10" → [1, 5, 10]; junk and negatives are dropped. */
function parseDelays(text: string): number[] {
  return text
    .split(',')
    .map((part) => Number(part.trim()))
    .filter((n) => Number.isFinite(n) && n >= 0)
}

function submit() {
  if (!valid.value) return
  emit('submit', {
    name: name.value.trim(),
    url: url.value.trim(),
    interval: Number(interval.value) || 30,
    mode: mode.value,
    retryAttempts: Number(retryAttempts.value) || 1,
    retryDelays: parseDelays(retryDelays.value),
  })
  emit('update:open', false)
}
</script>

<template>
  <Dialog :open="open" @update:open="emit('update:open', $event)">
    <DialogContent>
      <DialogHeader>
        <DialogTitle>{{ isEdit ? '编辑服务' : '添加服务' }}</DialogTitle>
        <DialogDescription>
          {{ isEdit ? '更新该服务的监控与展示方式。' : '监控一个新的 Web 服务。改动会写入 config.yaml。' }}
        </DialogDescription>
      </DialogHeader>

      <form class="grid gap-4" @submit.prevent="submit">
        <div class="grid gap-2">
          <Label for="svc-name">名称</Label>
          <Input id="svc-name" v-model="name" placeholder="Grafana" autocomplete="off" />
        </div>
        <div class="grid gap-2">
          <Label for="svc-url">URL</Label>
          <Input id="svc-url" v-model="url" placeholder="https://grafana.home.lab" autocomplete="off" />
        </div>
        <div class="grid grid-cols-2 gap-4">
          <div class="grid gap-2">
            <Label for="svc-interval">检查间隔</Label>
            <div class="relative">
              <Input id="svc-interval" v-model="interval" type="number" min="5" class="pr-8" />
              <span class="pointer-events-none absolute right-3 top-1/2 -translate-y-1/2 text-xs text-muted-foreground">s</span>
            </div>
          </div>
          <div class="grid gap-2">
            <Label>小组件模式</Label>
            <Select v-model="mode">
              <SelectTrigger>
                <SelectValue placeholder="选择模式" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="icon">仅图标</SelectItem>
                <SelectItem value="name">图标 + 名称</SelectItem>
                <SelectItem value="dashboard">迷你仪表盘</SelectItem>
              </SelectContent>
            </Select>
          </div>
        </div>

        <div class="grid gap-3 rounded-lg border border-border px-3 py-3">
          <div>
            <p class="text-sm font-medium">可靠性</p>
            <p class="mt-0.5 text-xs text-muted-foreground">检查失败后会先重试，再判定服务下线。若重试成功，记为警告而非故障。</p>
          </div>
          <div class="grid grid-cols-2 gap-4">
            <div class="grid gap-2">
              <Label for="svc-retries">尝试次数</Label>
              <Input id="svc-retries" v-model="retryAttempts" type="number" min="1" max="10" />
              <p class="text-xs text-muted-foreground">1 disables retries.</p>
            </div>
            <div class="grid gap-2">
              <Label for="svc-delays">重试延迟</Label>
              <Input id="svc-delays" v-model="retryDelays" placeholder="1, 5, 10" autocomplete="off" />
              <p class="text-xs text-muted-foreground">每次尝试之间的秒数。</p>
            </div>
          </div>
        </div>
      </form>

      <DialogFooter>
        <Button variant="ghost" @click="emit('update:open', false)">取消</Button>
        <Button :disabled="!valid" @click="submit">{{ isEdit ? '保存修改' : '添加服务' }}</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
