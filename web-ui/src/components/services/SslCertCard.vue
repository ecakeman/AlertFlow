<script setup lang="ts">
import { computed } from 'vue'
import { ShieldCheck, ShieldAlert, ShieldOff } from '@lucide/vue'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { cn } from '@/lib/utils'
import type { ServiceTls } from '@/types'

const props = defineProps<{ tls: ServiceTls | null; url: string }>()

const isHttps = computed(() => props.url.startsWith('https://'))

function fmtDate(iso: string | null): string {
  if (!iso) return '—'
  return new Date(iso).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

const daysBadge = computed(() => {
  const d = props.tls?.daysLeft
  if (d == null) return null
  if (d < 0) return { label: '已过期', class: 'bg-offline/10 text-offline' }
  if (d < 7) return { label: `${d}d left`, class: 'bg-offline/10 text-offline' }
  if (d < 30) return { label: `${d}d left`, class: 'bg-unknown/15 text-amber-500' }
  return { label: `${d}d left`, class: 'bg-online/10 text-online' }
})
</script>

<template>
  <Card>
    <CardHeader class="flex-row items-center justify-between gap-2 space-y-0">
      <CardTitle class="text-sm">SSL 证书</CardTitle>
      <span v-if="daysBadge" :class="cn('rounded-full px-2 py-0.5 text-xs font-medium', daysBadge.class)">
        {{ daysBadge.label }}
      </span>
    </CardHeader>
    <CardContent>
      <!-- HTTP service: no certificate to inspect -->
      <div v-if="!isHttps" class="flex items-center gap-2 text-sm text-muted-foreground">
        <ShieldOff class="size-4 shrink-0" />不适用——该服务使用纯 HTTP。</div>

      <!-- Awaiting first HTTPS check -->
      <div v-else-if="!tls" class="flex items-center gap-2 text-sm text-muted-foreground">
        <ShieldCheck class="size-4 shrink-0" />等待首次检查…</div>

      <!-- Handshake failed / invalid cert -->
      <div v-else-if="tls.error" class="flex items-start gap-2 text-sm text-offline">
        <ShieldAlert class="mt-0.5 size-4 shrink-0" />
        <span>Certificate error: {{ tls.error }}</span>
      </div>

      <!-- Valid certificate -->
      <dl v-else class="grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 text-sm">
        <dt class="text-muted-foreground">签发者</dt>
        <dd class="truncate font-medium">{{ tls.issuer || '—' }}</dd>
        <dt class="text-muted-foreground">主题</dt>
        <dd class="truncate font-medium">{{ tls.subject || '—' }}</dd>
        <dt class="text-muted-foreground">生效于</dt>
        <dd class="font-medium tabular-nums">{{ fmtDate(tls.validFrom) }}</dd>
        <dt class="text-muted-foreground">到期于</dt>
        <dd class="font-medium tabular-nums">{{ fmtDate(tls.validUntil) }}</dd>
      </dl>
    </CardContent>
  </Card>
</template>
