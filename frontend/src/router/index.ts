import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { useSessionStore } from '../stores/session'

// 路由按页面职责拆分，视图全部懒加载以减小首屏体积。
const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/dashboard' },
  {
    path: '/dashboard',
    name: 'dashboard',
    component: () => import('../views/DashboardView.vue'),
    meta: { title: '总览' }
  },
  {
    path: '/services',
    name: 'services',
    component: () => import('../views/ServicesView.vue'),
    meta: { title: '服务列表' }
  },
  {
    path: '/services/:serviceId',
    name: 'service-detail',
    component: () => import('../views/ServiceDetailView.vue'),
    meta: { title: '服务详情' }
  },
  {
    path: '/targets',
    name: 'targets',
    component: () => import('../views/TargetsView.vue'),
    meta: { title: '监控目标管理' }
  },
  {
    path: '/episodes',
    name: 'episodes',
    component: () => import('../views/EpisodesView.vue'),
    meta: { title: '故障 Episode' }
  },
  {
    path: '/episodes/:episodeId',
    name: 'episode-detail',
    component: () => import('../views/EpisodeDetailView.vue'),
    meta: { title: 'Episode 详情' }
  },
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' }
]

const router = createRouter({
  history: createWebHistory(),
  routes
})

router.afterEach((to) => {
  const title = (to.meta.title as string | undefined) ?? ''
  document.title = title ? `${title} · AlertFlow` : 'AlertFlow 运维控制台'
  try {
    useSessionStore().setCurrentTitle(title)
  } catch {
    // pinia 尚未安装时忽略（首屏 afterEach 早于 app.use 的极端情况）
  }
})

export default router
