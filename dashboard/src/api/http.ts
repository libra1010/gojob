import axios, { AxiosError } from 'axios'
import { ElMessage } from 'element-plus'
import type { Resp } from './types'

export const TOKEN_KEY = 'gojob_token'

/** 会话续期响应头（小写：axios 会把响应头名统一转小写）。 */
const TOKEN_RENEWED_HEADER = 'x-gojob-token-renewed'

/** 后端业务错误（code != 0），携带原始 code 供调用方按需分支处理 */
export class ApiError extends Error {
  code: number
  constructor(code: number, message: string) {
    super(message)
    this.code = code
  }
}

const http = axios.create({
  baseURL: '/openapi',
  timeout: 20000,
})

// 请求拦截：统一挂 token
http.interceptors.request.use((config) => {
  const token = localStorage.getItem(TOKEN_KEY)
  if (token) {
    config.headers['X-GoJob-Token'] = token
  }
  return config
})

function gotoLogin() {
  localStorage.removeItem(TOKEN_KEY)
  if (window.location.pathname !== '/login') {
    const redirect = encodeURIComponent(window.location.pathname + window.location.search)
    window.location.href = `/login?redirect=${redirect}`
  }
}

// 响应拦截：拆信封、401 清 token 跳登录、code!=0 弹错误提示
http.interceptors.response.use(
  (response) => {
    const body = response.data as Resp<unknown>
    if (body === null || typeof body !== 'object' || body.code === undefined) {
      return Promise.reject(new ApiError(-1, '响应格式异常'))
    }
    if (body.code === 0) {
      // 无状态会话票由服务端经响应头续发（见 auth.RenewedHeader），本地存的一直是最新那张
      const fresh = response.headers?.[TOKEN_RENEWED_HEADER]
      if (typeof fresh === 'string' && fresh) {
        localStorage.setItem(TOKEN_KEY, fresh)
      }
      return response
    }
    if (body.code === 401) {
      ElMessage.error('登录已失效，请重新登录')
      gotoLogin()
      return Promise.reject(new ApiError(401, body.message || 'not logged in'))
    }
    ElMessage.error(body.message || '请求失败')
    return Promise.reject(new ApiError(body.code, body.message || '请求失败'))
  },
  (error: AxiosError) => {
    // 仅处理网络/HTTP 层错误；业务 code!=0 后端也以 HTTP 200 返回，走上分支
    ElMessage.error(error.message || '网络错误')
    return Promise.reject(error)
  },
)

/** 统一取 data 字段 */
export async function request<T>(promise: Promise<{ data: Resp<T> }>): Promise<T> {
  const res = await promise
  return res.data.data
}

export default http
