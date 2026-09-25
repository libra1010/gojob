import http, { request } from './http'
import type { AppInfo, CreateAppReq, ModifyAppReq, WorkerInfo } from './types'

export function listApps() {
  return request<AppInfo[]>(http.get('/app/list'))
}

/** 返回新建记录的 ID */
export function createApp(req: CreateAppReq) {
  return request<number>(http.post('/app/create', req))
}

export function modifyApp(req: ModifyAppReq) {
  return request<Record<string, never>>(http.post('/app/modify', req))
}

/** 项目下仍有任务时后端返回 409，message 说明还剩几个任务 */
export function deleteApp(appId: string) {
  return request<Record<string, never>>(http.post('/app/delete', { app_id: appId }))
}

/** app 当前在线 worker 节点（心跳注册表） */
export function listWorkers(appId: string) {
  return request<WorkerInfo[]>(http.get('/app/workers', { params: { app_id: appId } }))
}
