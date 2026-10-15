<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ArrowLeft, Pencil, CheckCircle2, Trash2, LoaderCircle, Send } from '@lucide/vue'
import StatusDot from '@/components/dashboard/StatusDot.vue'
import IncidentFormDialog from '@/components/incidents/IncidentFormDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Textarea } from '@/components/ui/textarea'
import { toast } from '@/components/ui/sonner'
import { useIncidentsStore, type IncidentInput } from '@/stores/incidents'
import { useAuthStore } from '@/stores/auth'
import { api, ApiError } from '@/api'
import type { AgentChatMessage, AgentReport, Delivery, IncidentDetail } from '@/types'

const route = useRoute()
const router = useRouter()
const store = useIncidentsStore()
const auth = useAuthStore()

const id = computed(() => Number(route.params.id))
const detail = ref<IncidentDetail | null>(null)
const deliveries = ref<Delivery[]>([])
const loading = ref(true)
const retrying = ref<number | null>(null)
const editOpen = ref(false)
const confirmOpen = ref(false)
const chatInput = ref('')
const chatBusy = ref(false)
/** 与诊断 Agent 的排查会话（只读工具 + 记忆，服务端代理）。 */
const chat = ref<AgentChatMessage[]>([])

const agentReport = ref<AgentReport | null>(null)
const agentLoading = ref(false)

function errMsg(e: unknown) {
  return e instanceof ApiError ? e.message : '出错了'
}

async function load() {
  loading.value = true
  try {
    detail.value = await store.getDetail(id.value)
    try {
      deliveries.value = await api.listDeliveries(id.value)
    } catch {
      deliveries.value = []
    }
  } catch {
    detail.value = null
    deliveries.value = []
  } finally {
    loading.value = false
  }
}
async function loadAgent() {
  agentLoading.value = true
  try {
    agentReport.value = await api.agentReport(id.value)
  } catch {
    agentReport.value = null
  } finally {
    agentLoading.value = false
  }
}

const agentEmptyText = computed(() => {
  if (!agentReport.value) return '诊断 Agent 不可用。'
  switch (agentReport.value.reason) {
    case 'no agent run for this incident':
      return '该事件没有诊断记录。'
    case 'agent not configured':
      return '未配置诊断 Agent。'
    default:
      return '诊断 Agent 不可用。'
  }
})

/** 假设状态 → 颜色：supported 最重，weakened/ruled_out 最轻。 */
function hypothesisClass(status: string): string {
  if (status === 'supported') return 'bg-online/15 text-online'
  if (status === 'possible') return 'bg-warning/15 text-warning'
  return 'bg-muted text-muted-foreground'
}

onMounted(() => {
  load()
  loadAgent()
  loadChat()
})

const durationText = computed(() => {
  const inc = detail.value
  if (!inc) return ''
  // A warning event is momentary (started/resolved at the same instant), not a
  // span — a "duration" for it would just be a rounding artifact.
  if (inc.severity === 'warning') return '—'
  const end = inc.resolvedAt ? new Date(inc.resolvedAt).getTime() : Date.now()
  const mins = Math.max(1, Math.round((end - new Date(inc.startedAt).getTime()) / 60000))
  if (mins < 60) return `${mins} min`
  const hrs = Math.floor(mins / 60)
  return hrs < 24 ? `${hrs}h ${mins % 60}m` : `${Math.floor(hrs / 24)}d ${hrs % 24}h`
})

const defaultTitle = computed(() => {
  const inc = detail.value
  if (!inc) return ''
  if (inc.severity === 'warning') return '重试后恢复'
  return inc.status === 'ongoing' ? '进行中故障' : '故障'
})

function fmtDateTime(iso: string): string {
  return new Date(iso).toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  })
}

async function onEdit(input: IncidentInput) {
  try {
    await store.update(id.value, input)
    toast.success('事件已更新')
    load()
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onResolve() {
  try {
    await store.resolve(id.value)
    toast.success('事件已恢复')
    load()
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function onDelete() {
  try {
    await store.remove(id.value)
    toast.success('事件已删除')
    router.push('/incidents')
  } catch (e) {
    toast.error(errMsg(e))
  }
}
async function retry(deliveryId: number) {
  retrying.value = deliveryId
  try {
    await api.retryDelivery(deliveryId)
    deliveries.value = await api.listDeliveries(id.value)
    toast.success('通知已重新入队')
  } catch (e) {
    toast.error(errMsg(e))
  } finally {
    retrying.value = null
  }
}
async function sendChat() {
  const text = chatInput.value.trim()
  if (!text || chatBusy.value) return
  chat.value.push({ role: 'user', text })
  chatInput.value = ''
  chatBusy.value = true
  try {
    // 只带最近若干轮，避免会话上下文无限增长。
    const history = chat.value.slice(-8).map((m) => ({ role: m.role, text: m.text }))
    // 提交后不再等模型：问题入库、由 Agent 后台回答，这里轮询结果。
    const created = await api.agentChatRequest(id.value, text, history)
    const state = await pollChat(created.requestId)
    if (state.status === 'answered') {
      chat.value.push({ role: 'agent', text: state.reply ?? '', evidence_ids: state.evidenceIds })
    } else {
      chat.value.push({ role: 'agent', text: state.error || '这次没能给出回答。' })
    }
  } catch (e) {
    chat.value.push({ role: 'agent', text: e instanceof Error ? e.message : errMsg(e) })
  } finally {
    chatBusy.value = false
  }
}

/** 轮询一次请求直到有结论；超时按失败展示，而不是一直转圈。 */
async function pollChat(requestId: string, timeoutMs = 120_000) {
  const deadline = Date.now() + timeoutMs
  for (;;) {
    const state = await api.agentChatResult(id.value, requestId)
    if (state.status !== 'pending') return state
    if (Date.now() > deadline) throw new Error('等待诊断 Agent 回答超时')
    await new Promise((resolve) => setTimeout(resolve, 1500))
  }
}

/** 恢复该事件已有的问答（刷新页面后仍然看得到）。 */
async function loadChat() {
  try {
    const res = await api.agentChatHistory(id.value)
    chat.value = res.requests.flatMap((r) => {
      const turns: AgentChatMessage[] = [{ role: 'user', text: r.message }]
      if (r.status === 'answered') {
        turns.push({ role: 'agent', text: r.reply ?? '', evidence_ids: r.evidenceIds })
      } else if (r.status === 'failed') {
        turns.push({ role: 'agent', text: r.error || '这次没能给出回答。' })
      }
      return turns
    })
  } catch {
    // 历史加载失败不该影响页面其余部分。
    chat.value = []
  }
}
</script>

<template>
  <div class="mx-auto max-w-[820px] px-6 py-6 lg:px-8">
    <button
      class="mb-4 inline-flex cursor-pointer items-center gap-1.5 text-sm text-muted-foreground transition-colors hover:text-foreground"
      @click="router.push('/incidents')"
    >
      <ArrowLeft class="size-4" />事件</button>

    <div v-if="loading" class="flex justify-center py-24 text-muted-foreground">
      <LoaderCircle class="size-5 animate-spin" />
    </div>
    <div v-else-if="!detail" class="py-24 text-center text-muted-foreground">该事件已不存在。</div>

    <template v-else>
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div class="min-w-0">
          <div class="flex items-center gap-2">
            <StatusDot
              :status="
                detail.severity === 'warning' ? 'warning' : detail.status === 'ongoing' ? 'offline' : 'online'
              "
              :pulse="detail.severity !== 'warning' && detail.status === 'ongoing'"
            />
            <h2 class="truncate text-2xl font-semibold tracking-tight">
              {{ detail.title || defaultTitle }}
            </h2>
            <span
              v-if="detail.severity === 'warning'"
              class="rounded bg-warning/10 px-1.5 py-0.5 text-[10px] font-medium uppercase text-warning"
            >
              warning
            </span>
          </div>
          <p class="mt-1 text-sm text-muted-foreground">
            <RouterLink :to="`/services/${detail.serviceId}`" class="text-primary hover:underline">
              {{ detail.serviceName }}
            </RouterLink>
            · {{ detail.source === 'auto' ? '自动检测' : '手动记录' }}
          </p>
        </div>
        <div v-if="auth.isAdmin" class="flex items-center gap-2">
          <Button variant="outline" size="sm" @click="editOpen = true">
            <Pencil />编辑</Button>
          <Button v-if="detail.status === 'ongoing'" variant="outline" size="sm" @click="onResolve">
            <CheckCircle2 />标记恢复</Button>
          <Button variant="outline" size="sm" @click="confirmOpen = true">
            <Trash2 />删除</Button>
        </div>
      </header>

      <!-- Timeline -->
      <div class="mt-6 grid grid-cols-3 gap-3">
        <div class="rounded-xl border border-border bg-card px-4 py-3">
          <p class="text-xs text-muted-foreground">开始</p>
          <p class="mt-1 text-sm font-semibold tabular-nums">{{ fmtDateTime(detail.startedAt) }}</p>
        </div>
        <div class="rounded-xl border border-border bg-card px-4 py-3">
          <p class="text-xs text-muted-foreground">已恢复</p>
          <p class="mt-1 text-sm font-semibold tabular-nums">
            {{ detail.resolvedAt ? fmtDateTime(detail.resolvedAt) : '—' }}
          </p>
        </div>
        <div class="rounded-xl border border-border bg-card px-4 py-3">
          <p class="text-xs text-muted-foreground">时长</p>
          <p class="mt-1 text-sm font-semibold tabular-nums">{{ durationText }}</p>
        </div>
      </div>

      <Card class="mt-6">
        <CardHeader>
          <CardTitle class="text-sm">通知</CardTitle>
        </CardHeader>
        <CardContent>
          <p v-if="deliveries.length === 0" class="text-sm text-muted-foreground">该事件暂无通知任务。</p>
          <ul v-else class="space-y-3">
            <li
              v-for="row in deliveries"
              :key="row.id"
              class="rounded-lg border border-border px-3 py-2 text-sm"
            >
              <div class="flex flex-wrap items-center justify-between gap-2">
                <span class="font-medium">{{ row.integrationName }}</span>
                <span class="text-xs uppercase text-muted-foreground">{{ row.status }}</span>
              </div>
              <p class="mt-1 text-xs text-muted-foreground">
                {{ row.eventType }} · {{ row.attemptCount }} attempt{{ row.attemptCount === 1 ? '' : 's' }}
                <template v-if="row.status === 'retry_wait'">
                  · next {{ fmtDateTime(row.nextAttemptAt) }}
                </template>
                <template v-if="row.sentAt"> · sent {{ fmtDateTime(row.sentAt) }}</template>
              </p>
              <p v-if="row.lastError" class="mt-1 text-xs text-destructive">{{ row.lastError }}</p>
              <Button
                v-if="auth.isAdmin && row.status === 'dead'"
                class="mt-2"
                variant="outline"
                size="sm"
                :disabled="retrying === row.id"
                @click="retry(row.id)"
              >重试</Button>
            </li>
          </ul>
        </CardContent>
      </Card>

      <!-- Agent diagnosis (proxied from the diagnostic agent service) -->
      <Card class="mt-6">
        <CardHeader>
          <CardTitle class="text-sm">Agent 诊断</CardTitle>
        </CardHeader>
        <CardContent>
          <p v-if="agentLoading" class="text-sm text-muted-foreground">加载中…</p>
          <p v-else-if="!agentReport || !agentReport.available" class="text-sm text-muted-foreground">
            {{ agentEmptyText }}
          </p>
          <div v-else class="space-y-3">
            <div class="flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted-foreground">
              <span class="rounded bg-primary/10 px-1.5 py-0.5 font-medium uppercase text-primary">
                {{ agentReport.status }}
              </span>
              <span v-if="agentReport.eventType">{{ agentReport.eventType }}</span>
              <span v-if="agentReport.createdAt">· {{ fmtDateTime(agentReport.createdAt) }}</span>
              <span v-if="agentReport.usage">
                · {{ agentReport.usage.llm_calls ?? 0 }} LLM calls / {{ agentReport.usage.tokens ?? 0 }} tokens
              </span>
            </div>
            <p v-if="!agentReport.report" class="text-sm text-muted-foreground">诊断运行尚未生成报告。</p>
            <!-- 结论：整份报告最该先看的一块 -->
            <div
              v-if="agentReport.report?.conclusion?.text"
              class="rounded-lg border border-primary/30 bg-primary/5 px-3 py-2"
            >
              <p class="text-xs font-medium text-primary">结论</p>
              <p class="mt-1 whitespace-pre-wrap text-sm">{{ agentReport.report.conclusion.text }}</p>
              <p class="mt-1 text-xs text-muted-foreground">
                {{ agentReport.report.conclusion.layer }} · 置信度
                {{ agentReport.report.conclusion.confidence ?? '—' }}
                <template v-if="agentReport.report.conclusion.recurrence_of">
                  · 与既往事件
                  <span class="mono">{{ agentReport.report.conclusion.recurrence_of }}</span> 同源
                </template>
              </p>
            </div>
            <p v-if="agentReport.report?.summary" class="whitespace-pre-wrap text-sm">
              {{ agentReport.report.summary }}
            </p>
            <!-- 分层假设：结论背后的推理，按证据强弱排序 -->
            <div v-if="agentReport.report?.hypotheses?.length">
              <p class="text-xs font-medium text-muted-foreground">假设（按证据强弱）</p>
              <ul class="mt-1 space-y-2">
                <li
                  v-for="h in agentReport.report.hypotheses"
                  :key="h.id"
                  class="rounded-md border border-border px-3 py-2"
                >
                  <div class="flex flex-wrap items-center gap-2 text-[11px]">
                    <span class="rounded px-1.5 py-0.5 font-medium" :class="hypothesisClass(h.status)">
                      {{ h.status }}
                    </span>
                    <span class="text-muted-foreground">{{ h.layer }}</span>
                  </div>
                  <p class="mt-1 text-sm">{{ h.text }}</p>
                  <p v-if="h.supporting_evidence_ids?.length" class="mt-1 text-[11px] text-muted-foreground">
                    依据：<span class="mono">{{ h.supporting_evidence_ids.join(', ') }}</span>
                  </p>
                </li>
              </ul>
            </div>
            <p v-if="agentReport.report?.coverage" class="text-xs text-muted-foreground">
              checks: {{ agentReport.report.coverage.check_samples ?? '—' }}
              · diagnostics: {{ agentReport.report.coverage.diagnostic_source ?? '—' }}
              <template v-if="agentReport.report.coverage.gaps?.length">
                · gaps: {{ agentReport.report.coverage.gaps.join(', ') }}
              </template>
            </p>
            <div v-if="agentReport.report?.unknowns?.length">
              <p class="text-xs font-medium text-muted-foreground">未知项</p>
              <ul class="mt-1 list-disc space-y-1 pl-5 text-sm">
                <li v-for="u in agentReport.report.unknowns" :key="u.code">{{ u.text }}</li>
              </ul>
            </div>
            <div v-if="agentReport.report?.recommended_next_steps?.length">
              <p class="text-xs font-medium text-muted-foreground">下一步</p>
              <ul class="mt-1 list-disc space-y-1 pl-5 text-sm">
                <li v-for="(s, i) in agentReport.report.recommended_next_steps" :key="i">{{ s.text }}</li>
              </ul>
            </div>
          </div>
        </CardContent>
      </Card>

      <!-- 与 Agent 排查（取代原来的评论区） -->
      <Card class="mt-6">
        <CardHeader>
          <CardTitle class="text-sm">与 Agent 排查</CardTitle>
        </CardHeader>
        <CardContent>
          <p class="text-xs text-muted-foreground">
            基于本次事件的证据、诊断报告与长期记忆追问。Agent 只读证据，不会改动系统状态。
          </p>
          <ul v-if="chat.length" class="mt-3 space-y-3">
            <li v-for="(m, i) in chat" :key="i" class="flex gap-3">
              <span
                class="flex size-8 shrink-0 items-center justify-center rounded-full text-xs font-medium"
                :class="m.role === 'user' ? 'bg-muted text-muted-foreground' : 'bg-primary/15 text-primary'"
              >
                {{ m.role === 'user' ? '我' : 'AI' }}
              </span>
              <div class="min-w-0 flex-1">
                <p class="whitespace-pre-wrap text-sm">{{ m.text }}</p>
                <p v-if="m.evidence_ids?.length" class="mt-1 text-[11px] text-muted-foreground">
                  依据：<span class="mono">{{ m.evidence_ids.join(', ') }}</span>
                </p>
              </div>
            </li>
          </ul>

          <form class="mt-4 flex items-end gap-2" @submit.prevent="sendChat">
            <Textarea
              v-model="chatInput"
              placeholder="追问 Agent：这个结论的依据是什么？会不会是探针侧的问题？"
              class="min-h-10 flex-1"
              @keydown.enter.meta.prevent="sendChat"
            />
            <Button type="submit" :disabled="chatBusy || !chatInput.trim()">
              <LoaderCircle v-if="chatBusy" class="animate-spin" />
              <Send v-else />
              发送
            </Button>
          </form>
        </CardContent>
      </Card>
    </template>

    <IncidentFormDialog v-if="detail" v-model:open="editOpen" :incident="detail" @submit="onEdit" />
    <ConfirmDialog
      v-model:open="confirmOpen"
      title="删除该事件？"
      description="将永久删除该事件及其评论。"
      confirm-label="删除"
      destructive
      @confirm="onDelete"
    />
  </div>
</template>
