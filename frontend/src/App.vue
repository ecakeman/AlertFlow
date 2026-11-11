<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useSessionStore } from './stores/session'
import { localTimeZoneLabel } from './utils/time'

const route = useRoute()
const session = useSessionStore()

const activeMenu = computed(() => {
  const path = route.path
  if (path.startsWith('/services')) return '/services'
  if (path.startsWith('/targets')) return '/targets'
  if (path.startsWith('/episodes')) return '/episodes'
  return '/dashboard'
})

const pageTitle = computed(() => (route.meta.title as string | undefined) ?? 'AlertFlow')
const timezone = localTimeZoneLabel()
</script>

<template>
  <el-container class="app-shell">
    <el-aside width="216px" class="app-aside">
      <div class="brand">
        <div class="brand-logo">AF</div>
        <div>
          <div class="brand-name">AlertFlow</div>
          <div class="brand-sub">服务监控与故障诊断</div>
        </div>
      </div>

      <el-menu :default-active="activeMenu" router class="app-menu">
        <el-menu-item index="/dashboard">
          <el-icon><Odometer /></el-icon>
          <span>总览</span>
        </el-menu-item>
        <el-menu-item index="/services">
          <el-icon><Monitor /></el-icon>
          <span>服务管理</span>
        </el-menu-item>
        <el-menu-item index="/targets">
          <el-icon><Aim /></el-icon>
          <span>监控目标</span>
        </el-menu-item>
        <el-menu-item index="/episodes">
          <el-icon><WarningFilled /></el-icon>
          <span>故障 Episode</span>
        </el-menu-item>
      </el-menu>

      <div class="aside-foot">
        <div class="aside-foot-label">浏览器本地时区</div>
        <div class="aside-foot-value">{{ timezone }}</div>
      </div>
    </el-aside>

    <el-container class="app-body">
      <el-header class="app-header" height="56px">
        <div class="header-left">
          <span class="page-title">{{ pageTitle }}</span>
          <el-tooltip content="环境标识，仅用于区分部署环境" placement="bottom">
            <el-tag size="small" type="info" effect="plain">{{ session.envLabel }}</el-tag>
          </el-tooltip>
        </div>
        <div class="header-right">
          <span class="header-hint">低频刷新：进入页面或手动刷新，无 WebSocket/SSE</span>
        </div>
      </el-header>

      <el-main class="app-main">
        <router-view />
      </el-main>
    </el-container>
  </el-container>
</template>
