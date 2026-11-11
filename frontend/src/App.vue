<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'

const route = useRoute()

// 主导航只有三个业务入口：总览 / 服务 / 故障记录。
const activeMenu = computed(() => {
  const path = route.path
  if (path.startsWith('/services')) return '/services'
  if (path.startsWith('/episodes')) return '/episodes'
  return '/dashboard'
})
</script>

<template>
  <div class="app-shell">
    <aside class="app-aside">
      <div class="brand">
        <div class="brand-mark">AF</div>
        <div>
          <div class="brand-name">AlertFlow</div>
          <div class="brand-sub">服务故障诊断</div>
        </div>
      </div>

      <nav class="app-nav">
        <el-menu :default-active="activeMenu" router>
          <el-menu-item index="/dashboard">
            <el-icon><Odometer /></el-icon>
            <span>运行总览</span>
          </el-menu-item>
          <el-menu-item index="/services">
            <el-icon><Grid /></el-icon>
            <span>服务</span>
          </el-menu-item>
          <el-menu-item index="/episodes">
            <el-icon><Warning /></el-icon>
            <span>故障记录</span>
          </el-menu-item>
        </el-menu>
      </nav>
    </aside>

    <div class="app-content">
      <main class="app-main">
        <router-view />
      </main>
    </div>
  </div>
</template>
