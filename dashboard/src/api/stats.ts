import http, { request } from './http'
import type { StatsOverview } from './types'

/** 控制台首页统计总览：账号/任务/项目/Worker 汇总 + 今日执行 + 最近实例 */
export function getStatsOverview() {
  return request<StatsOverview>(http.get('/stats/overview'))
}
