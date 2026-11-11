import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'

// 路由按页面职责拆分，视图全部懒加载以减小首屏体积。
// 监控目标属于服务监控配置，不再作为独立页面；旧 /targets 链接统一回到服务列表。
const routes: RouteRecordRaw[] = [
  { path: '/', redirect: '/dashboard' },
  {
    path: '/dashboard',
    name: 'dashboard',
    component: () => import('../views/DashboardView.vue'),
    meta: { title: '运行总览' }
  },
  {
    path: '/services',
    name: 'services',
    component: () => import('../views/ServicesView.vue'),
    meta: { title: '服务' }
  },
  {
    path: '/services/:serviceId',
    name: 'service-detail',
    component: () => import('../views/ServiceDetailView.vue'),
    meta: { title: '服务详情' }
  },
  { path: '/targets', redirect: '/services' },
  {
    path: '/episodes',
    name: 'episodes',
    component: () => import('../views/EpisodesView.vue'),
    meta: { title: '故障记录' }
  },
  {
    path: '/episodes/:episodeId',
    name: 'episode-detail',
    component: () => import('../views/EpisodeDetailView.vue'),
    meta: { title: '故障详情' }
  },
  { path: '/:pathMatch(.*)*', redirect: '/dashboard' }
]

const router = createRouter({
  history: createWebHistory(),
  routes
})

router.afterEach((to) => {
  const title = (to.meta.title as string | undefined) ?? ''
  document.title = title ? `${title} · AlertFlow` : 'AlertFlow 控制台'
})

export default router
