import http, { request } from './http'
import type { LoginLog, OperationLog, PageResp } from './types'

export interface LogQuery {
  page?: number
  size?: number
  username?: string
  action?: string
  app_id?: string
}

/** 登录审计日志（ADMIN 及以上） */
export function loginLogs(q: LogQuery = {}) {
  return request<PageResp<LoginLog>>(http.get('/audit/loginLogs', { params: q }))
}

/** 操作审计日志（ADMIN 及以上） */
export function operationLogs(q: LogQuery = {}) {
  return request<PageResp<OperationLog>>(http.get('/audit/operationLogs', { params: q }))
}
