import { createRouter, createWebHistory } from 'vue-router'
import { TOKEN_KEY } from '@/api/http'
import { useAuthStore } from '@/stores/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/LoginView.vue'),
      meta: { public: true, title: '登录' },
    },
    {
      path: '/',
      component: () => import('@/layouts/MainLayout.vue'),
      children: [
        {
          path: '',
          name: 'overview',
          component: () => import('@/views/OverviewView.vue'),
          meta: { title: '统计总览' },
        },
        {
          path: 'apps',
          name: 'apps',
          component: () => import('@/views/AppListView.vue'),
          meta: { title: '项目管理' },
        },
        {
          path: 'apps/:appId/jobs',
          name: 'jobs',
          component: () => import('@/views/JobListView.vue'),
          meta: { title: '任务列表' },
        },
        {
          path: 'jobs/:jobId/instances',
          name: 'instances',
          component: () => import('@/views/InstanceListView.vue'),
          meta: { title: '实例列表' },
        },
        {
          path: 'instances/:instanceId/logs',
          name: 'instance-logs',
          component: () => import('@/views/InstanceLogView.vue'),
          meta: { title: '实例日志' },
        },
        {
          path: 'system/users',
          name: 'system-users',
          component: () => import('@/views/SystemUsersView.vue'),
          meta: { title: '用户管理', roles: ['SUPER_ADMIN'] },
        },
        {
          path: 'system/login-logs',
          name: 'system-login-logs',
          component: () => import('@/views/LoginLogView.vue'),
          meta: { title: '登录日志', roles: ['ADMIN', 'SUPER_ADMIN'] },
        },
        {
          path: 'system/operation-logs',
          name: 'system-operation-logs',
          component: () => import('@/views/OperationLogView.vue'),
          meta: { title: '操作日志', roles: ['ADMIN', 'SUPER_ADMIN'] },
        },
        {
          path: 'system/nodes',
          name: 'system-nodes',
          component: () => import('@/views/NodeMetricsView.vue'),
          meta: { title: '节点监控', roles: ['ADMIN', 'SUPER_ADMIN'] },
        },
      ],
    },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

// 路由守卫：未登录跳登录页，已登录访问 /login 回首页（统计总览）
router.beforeEach((to) => {
  const logged = !!localStorage.getItem(TOKEN_KEY)
  if (!to.meta.public && !logged) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'login' && logged) {
    return { name: 'overview' }
  }
  // 角色受限页面：无角色信息时先放行（MainLayout 会补齐 me），有角色且不匹配则回首页
  const roles = to.meta.roles as string[] | undefined
  if (roles) {
    const auth = useAuthStore()
    if (auth.role && !roles.includes(auth.role)) {
      return { name: 'overview' }
    }
  }
  return true
})

export default router
