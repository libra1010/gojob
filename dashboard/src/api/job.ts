import http, { request } from './http'
import type {
  CreateJobReq,
  ImportJobsResp,
  JobAction,
  JobInfo,
  ModifyJobReq,
  PageResult,
  JobDTO,
} from './types'

/**
 * 任务列表：index 从 0 起（与兼容协议会话接口的分页语义一致）。
 * pageSize<=0 表示不分页、一次给全量（建单抽屉里选「上游任务」要的是全项目候选）。
 */
export function listJobs(
  appId: string,
  opts: { index?: number; pageSize?: number; jobId?: number | string; keyword?: string } = {},
) {
  return request<PageResult<JobInfo>>(
    http.get('/job/list', {
      params: {
        app_id: appId,
        index: opts.index ?? 0,
        page_size: opts.pageSize ?? 10,
        job_id: opts.jobId === undefined || opts.jobId === '' ? undefined : opts.jobId,
        keyword: opts.keyword || undefined,
      },
    }),
  )
}

/** 一次给全量（不分页），给需要完整候选集的下拉用 */
export function listAllJobs(appId: string) {
  return listJobs(appId, { pageSize: 0 }).then((p) => p.data ?? [])
}

/** 单个任务导出：后端返回一个对象 */
export function exportJob(jobId: number) {
  return request<JobDTO>(http.get('/job/export', { params: { job_id: jobId } }))
}

/** 批量导出：任务 ID 列表 / 整个项目，后端一律返回数组 */
export function exportJobs(jobIds: number[]) {
  return request<JobDTO[]>(
    http.get('/job/export', { params: { job_ids: jobIds.join(',') } }),
  )
}

export function exportAppJobs(appId: string) {
  return request<JobDTO[]>(http.get('/job/export', { params: { app_id: appId } }))
}

export function getJob(jobId: number) {
  return request<JobInfo>(http.get('/job/get', { params: { job_id: jobId } }))
}

/** 返回新建任务的 ID */
export function createJob(req: CreateJobReq) {
  return request<number>(http.post('/job/create', req))
}

export function modifyJob(req: ModifyJobReq) {
  return request<Record<string, never>>(http.post('/job/modify', req))
}

/**
 * trigger 时若任务执行中且开启 skip_when_running，返回 409。
 * instanceParams 非空即「参数运行」（兼容协议同名语义）：只影响这一次运行，不改任务里配的默认值。
 */
export function operateJob(jobId: number, action: JobAction, instanceParams?: string) {
  return request<Record<string, never>>(
    http.post('/job/operate', { job_id: jobId, action, instance_params: instanceParams ?? '' }),
  )
}

export function deleteJob(jobId: number) {
  return request<Record<string, never>>(http.post('/job/delete', { job_id: jobId }))
}

/** 通用任务包格式（camelCase 数组）逐条导入 */
export function importJobs(appId: string, jobs: JobDTO[]) {
  return request<ImportJobsResp>(http.post('/job/import', { app_id: appId, jobs }))
}

/** 预览最近 count 次计划触发时间；API 型返回空数组 */
export function previewTime(timeExpressionType: string, timeExpression: string, count = 15) {
  return request<{ type: string; preview: string[] }>(
    http.get('/job/previewTime', {
      params: { time_expression_type: timeExpressionType, time_expression: timeExpression, count },
    }),
  )
}
