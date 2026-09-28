import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { login as loginApi, logout as logoutApi } from '@/api/user'
import { TOKEN_KEY } from '@/api/http'
import type { Role } from '@/api/types'

export const useAuthStore = defineStore('auth', () => {
  const token = ref<string>(localStorage.getItem(TOKEN_KEY) || '')
  const username = ref<string>('')
  const nickname = ref<string>('')
  const role = ref<Role | ''>('')
  const appScopes = ref<string[]>([])

  const isSuperAdmin = computed(() => role.value === 'SUPER_ADMIN')
  const isAdmin = computed(() => role.value === 'SUPER_ADMIN' || role.value === 'ADMIN')
  // 可对项目做写操作（非只读）
  const canWrite = computed(() => role.value === 'SUPER_ADMIN' || role.value === 'ADMIN')

  function setToken(tk: string) {
    token.value = tk
    if (tk) localStorage.setItem(TOKEN_KEY, tk)
    else localStorage.removeItem(TOKEN_KEY)
  }

  function setProfile(p: { username?: string; nickname?: string; role?: Role; app_scopes?: string[] }) {
    if (p.username !== undefined) username.value = p.username
    if (p.nickname !== undefined) nickname.value = p.nickname
    if (p.role !== undefined) role.value = p.role
    if (p.app_scopes !== undefined) appScopes.value = p.app_scopes
  }

  /** 某 app 是否可写（只读恒 false；超管恒 true；ADMIN 命中 scope 或 "*"） */
  function canWriteApp(appId: string) {
    if (!canWrite.value) return false
    if (isSuperAdmin.value) return true
    return appScopes.value.includes('*') || appScopes.value.includes(appId)
  }

  async function login(name: string, password: string) {
    const resp = await loginApi({ username: name, password })
    setToken(resp.token)
    role.value = resp.role
    username.value = resp.username
    nickname.value = resp.nickname
    // 登录响应不含 scope，补拉一次 me（失败不阻断进入，后续路由守卫会用）
    try {
      const { getMe } = await import('@/api/user')
      const me = await getMe()
      setProfile({ role: me.role, app_scopes: me.app_scopes, nickname: me.nickname })
    } catch {
      /* 拦截器已处理 */
    }
  }

  async function logout() {
    try {
      await logoutApi()
    } catch {
      // 服务端已不认该 token 也无所谓，本地照常清理
    }
    setToken('')
    username.value = ''
    nickname.value = ''
    role.value = ''
    appScopes.value = []
  }

  return {
    token, username, nickname, role, appScopes,
    isSuperAdmin, isAdmin, canWrite, canWriteApp,
    setToken, setProfile, login, logout,
  }
})
