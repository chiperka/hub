import { createRouter, createWebHistory } from 'vue-router'
import { session, loadSession } from './api'

const routes = [
  { path: '/login', component: () => import('./pages/Login.vue'), meta: { public: true, bare: true } },
  { path: '/', component: () => import('./pages/Dashboard.vue'), meta: { title: 'Dashboard' } },
  { path: '/runs', component: () => import('./pages/Runs.vue'), meta: { title: 'Runs' } },
  { path: '/runs/:id', component: () => import('./pages/RunDetail.vue'), props: true, meta: { title: 'Run' } },
  { path: '/tests/:id', component: () => import('./pages/TestDetail.vue'), props: true, meta: { title: 'Test' } },
  { path: '/queue', component: () => import('./pages/Queue.vue'), meta: { title: 'Queue' } },
  { path: '/workers', component: () => import('./pages/Workers.vue'), meta: { title: 'Workers' } },
  { path: '/projects', component: () => import('./pages/Projects.vue'), meta: { title: 'Projects' } },
  { path: '/connect', component: () => import('./pages/Connect.vue'), meta: { title: 'Connect CLI' } },
  { path: '/settings', component: () => import('./pages/Settings.vue'), meta: { title: 'Settings' } },
  { path: '/:pathMatch(.*)*', redirect: '/' },
]

export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior: (to, from, saved) => saved || (to.path !== from.path ? { top: 0 } : undefined),
})

router.beforeEach(async (to) => {
  if (!session.loaded) await loadSession()
  if (!to.meta.public && !session.authenticated) return { path: '/login', query: { next: to.fullPath } }
  if (to.path === '/login' && session.authenticated) return { path: '/' }
})

router.afterEach((to) => {
  document.title = to.meta.title ? `${to.meta.title} · Chiperka Hub` : 'Chiperka Hub'
})
