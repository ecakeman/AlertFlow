import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

/**
 * Routes. `meta.bare` pages (auth/setup) render without the app shell;
 * everything else renders inside the sidebar + topbar layout.
 * Auth guards will be added when the backend is wired.
 */
const routes: RouteRecordRaw[] = [
  {
    path: '/',
    name: 'dashboard',
    component: () => import('@/views/DashboardView.vue'),
    meta: { title: '仪表盘' },
  },
  {
    path: '/services/:id',
    name: 'service-detail',
    component: () => import('@/views/ServiceDetailView.vue'),
    meta: { title: '服务' },
  },
  {
    path: '/resources',
    name: 'resources',
    component: () => import('@/views/ResourcesView.vue'),
    meta: { title: '资源' },
  },
  {
    path: '/incidents',
    name: 'incidents',
    component: () => import('@/views/IncidentsView.vue'),
    meta: { title: '事件' },
  },
  {
    path: '/incidents/:id',
    name: 'incident-detail',
    component: () => import('@/views/IncidentDetailView.vue'),
    meta: { title: '事件' },
  },
  {
    path: '/settings',
    name: 'settings',
    component: () => import('@/views/SettingsView.vue'),
    meta: { title: '设置' },
  },
  {
    path: '/integrations',
    name: 'integrations',
    component: () => import('@/views/IntegrationsView.vue'),
    meta: { title: '集成' },
  },
  {
    path: '/setup',
    name: 'setup',
    component: () => import('@/views/SetupView.vue'),
    meta: { title: 'Setup', bare: true },
  },
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    meta: { title: '登录', bare: true },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { title: '未找到', bare: true },
  },
]

const adminOnly = new Set(['resources', 'settings', 'integrations'])

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes,
  scrollBehavior() {
    return { top: 0 }
  },
})

// Gate navigation on first-run setup, authentication and role.
router.beforeEach(async (to) => {
  const auth = useAuthStore()
  if (!auth.ready) {
    await auth.bootstrap()
  }

  if (auth.needsSetup) {
    return to.name === 'setup' ? true : { name: 'setup' }
  }
  if (to.name === 'setup') {
    return { name: 'dashboard' }
  }
  if (!auth.isAuthenticated) {
    return to.name === 'login' ? true : { name: 'login' }
  }
  if (to.name === 'login') {
    return { name: 'dashboard' }
  }
  if (auth.currentUser?.role === 'readonly' && adminOnly.has(String(to.name))) {
    return { name: 'dashboard' }
  }
  return true
})

router.afterEach((to) => {
  const title = (to.meta.title as string) ?? ''
  document.title = title ? `${title} · AlertFlow` : 'AlertFlow'
})

export default router
