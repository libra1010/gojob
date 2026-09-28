import http, { request } from './http'
import type { AppInfo, CreateAppReq, ModifyAppReq, WorkerNodeView } from './types'

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

/** app 当前在线 worker 节点（心跳注册表），含管理面那一侧的接单开关 */
export function listWorkers(appId: string) {
  return request<WorkerNodeView[]>(http.get('/app/workers', { params: { app_id: appId } }))
}

/**
 * 停/启某台 worker 的接单：后端改的就是在线节点表里的一个字段，生效靠选节点时过滤。
 * 在跑的任务不受影响；这台节点会继续心跳，只是领不到新任务。
 */
export function setWorkerAccept(appId: string, instanceId: string, accept: boolean) {
  return request<Record<string, never>>(
    http.post('/worker/acceptTasks', { app_id: appId, instance_id: instanceId, accept }),
  )
}
