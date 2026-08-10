import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/Login.vue'),
      meta: { requiresAuth: false },
    },
    {
      path: '/callback',
      name: 'callback',
      component: () => import('@/views/Login.vue'),
      meta: { requiresAuth: false },
    },
    {
      path: '/invite/:token',
      name: 'invite',
      component: () => import('@/views/Invite.vue'),
      meta: { requiresAuth: false },
    },
    {
      path: '/',
      component: () => import('@/layouts/AppLayout.vue'),
      children: [
        {
          path: '',
          name: 'dashboard',
          component: () => import('@/views/Dashboard.vue'),
        },
        {
          path: 'projects',
          name: 'projects',
          component: () => import('@/views/Projects.vue'),
        },
        {
          path: 'apps',
          name: 'apps',
          component: () => import('@/views/Apps.vue'),
        },
        {
          path: 'services',
          name: 'services',
          component: () => import('@/views/Services.vue'),
        },
        {
          path: 'services/:name/data',
          name: 'service-data',
          component: () => import('@/views/ServiceData.vue'),
          meta: { fullBleed: true },
        },
        {
          path: 'functions',
          name: 'functions',
          component: () => import('@/views/Functions.vue'),
        },
        {
          path: 'functions/new',
          name: 'function-new',
          component: () => import('@/views/FunctionForm.vue'),
          meta: { fullBleed: true },
        },
        {
          path: 'functions/:fn',
          name: 'function-edit',
          component: () => import('@/views/FunctionForm.vue'),
          meta: { fullBleed: true },
        },
        {
          path: 'jobs',
          name: 'jobs',
          component: () => import('@/views/Jobs.vue'),
        },
        {
          path: 'routes',
          name: 'routes',
          component: () => import('@/views/Routes.vue'),
        },
        {
          path: 'volumes',
          name: 'volumes',
          component: () => import('@/views/Volumes.vue'),
        },
        {
          path: 'storage',
          name: 'storage',
          component: () => import('@/views/Storage.vue'),
        },
        {
          path: 'backups',
          name: 'backups',
          component: () => import('@/views/Backups.vue'),
        },
        {
          path: 'alerts',
          name: 'alerts',
          component: () => import('@/views/Alerts.vue'),
        },
        {
          path: 'users',
          name: 'users',
          component: () => import('@/views/Users.vue'),
          meta: { requiredRole: 'admin' },
        },
        {
          path: 'migration',
          name: 'migration',
          component: () => import('@/views/Migration.vue'),
          meta: { requiredRole: 'admin' },
        },
        {
          path: 'settings',
          name: 'settings',
          component: () => import('@/views/Settings.vue'),
          meta: { requiredRole: 'admin' },
        },
        {
          path: 'platform',
          name: 'platform',
          component: () => import('@/views/Platform.vue'),
          meta: { requiredRole: 'admin' },
        },
        {
          path: 'platform/:name/pods',
          name: 'platform-pods',
          component: () => import('@/views/PodDrilldown.vue'),
          meta: { requiredRole: 'admin' },
        },
        {
          path: 'apps/:name/pods',
          name: 'app-pods',
          component: () => import('@/views/PodDrilldown.vue'),
        },
        {
          path: 'services/:name/pods',
          name: 'service-pods',
          component: () => import('@/views/PodDrilldown.vue'),
        },
        {
          path: 'internal/resource-controls',
          name: 'internal-resource-controls',
          component: () => import('@/views/internal/resource-controls-playground.vue'),
          meta: { requiredRole: 'admin' },
        },
      ],
    },
  ],
})

router.beforeEach((to) => {
  const auth = useAuthStore()
  if (to.meta.requiresAuth === false) return true
  if (!auth.isAuthenticated) {
    return { name: 'login' }
  }
  if (to.meta.requiredRole === 'admin' && !auth.isAdmin) {
    return { name: 'dashboard' }
  }
  return true
})

export default router
