<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { LogIn, Loader2 } from '@lucide/vue'
import BrandMark from '@/components/common/BrandMark.vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { toast } from '@/components/ui/sonner'
import { useAuthStore } from '@/stores/auth'
import { ApiError } from '@/api'

const router = useRouter()
const auth = useAuthStore()

const username = ref('admin')
const password = ref('')
const loading = ref(false)

const valid = computed(() => username.value.trim().length > 0 && password.value.length > 0)

async function submit() {
  if (!valid.value || loading.value) return
  loading.value = true
  try {
    await auth.login(username.value.trim(), password.value)
    router.push('/')
  } catch (e) {
    toast.error(e instanceof ApiError ? e.message : '登录失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="relative flex min-h-screen items-center justify-center overflow-hidden bg-background px-6">
    <div
      class="pointer-events-none absolute left-1/2 top-0 h-[420px] w-[720px] -translate-x-1/2 rounded-full bg-primary/15 blur-[120px]"
    />
    <div class="relative w-full max-w-sm">
      <div class="mb-7 flex flex-col items-center text-center">
        <BrandMark class="size-11" />
        <h1 class="mt-4 text-xl font-semibold tracking-tight">欢迎回来</h1>
        <p class="mt-1.5 text-sm text-muted-foreground">登录你的 AlertFlow 仪表盘。</p>
      </div>

      <div class="rounded-2xl border border-border bg-card p-6 shadow-elevation-medium">
        <form class="grid gap-4" @submit.prevent="submit">
          <div class="grid gap-2">
            <Label for="username">用户名</Label>
            <Input id="username" v-model="username" autocomplete="username" />
          </div>
          <div class="grid gap-2">
            <Label for="password">密码</Label>
            <Input id="password" v-model="password" type="password" autocomplete="current-password" />
          </div>
          <Button type="submit" class="mt-1 w-full" :disabled="!valid || loading">
            <Loader2 v-if="loading" class="animate-spin" />
            <LogIn v-else />登录</Button>
        </form>
      </div>
    </div>
  </div>
</template>
