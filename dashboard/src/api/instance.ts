import http, { request } from './http'
import type { InstanceDetail, InstanceTask, JobInstance, TaskLog } from './types'

export function listInstances(jobId: number) {
  return request<JobInstance[]>(http.get('/instance/list', { params: { job_id: jobId } }))
}

/**
 * 实例详情（对齐 PowerJob 的 /instance/detailPlus）：实例行 + Task 统计 + 一页子任务。
 * customQuery 是"select * from task_info where ___ limit 10"里的那一段，
 * 后端只认白名单语法，写歪了会在 query_error 里给出原因而不是让整个请求失败。
 */
export function instanceDetail(instanceId: number, customQuery = '') {
  return request<InstanceDetail>(http.post('/instance/detailPlus', {
    instance_id: instanceId,
    custom_query: customQuery,
  }))
}

export function getInstance(instanceId: number) {
  return request<JobInstance>(http.get('/instance/get', { params: { instance_id: instanceId } }))
}

/** 某实例下的全部任务（日志页先列任务再拉日志） */
export function listInstanceTasks(instanceId: number) {
  return request<InstanceTask[]>(http.get('/instance/tasks', { params: { instance_id: instanceId } }))
}

/**
 * 停止实例：WAITING 任务当场终止，RUNNING 任务由 server 记录停止意图，
 * 再经长轮询/心跳响应下发给 worker 协作取消（server 重启不丢该指令）。
 */
export function stopInstance(instanceId: number, appId: string) {
  return request<Record<string, never>>(http.post('/instance/stop', {
    instance_id: instanceId,
    app_id: appId,
  }))
}

/**
 * 增量拉取任务日志：按 seq 升序返回 seq > afterSeq 的日志。
 * 后端 limit 上限 2000，默认 500；轮询时以已见最大 seq 作为游标。
 */
export function listTaskLogs(taskId: number, afterSeq = 0, limit = 500) {
  return request<TaskLog[]>(
    http.get('/instance/logs', { params: { task_id: taskId, after_seq: afterSeq, limit } }),
  )
}
