import http, { request } from './http'
import type { NodesResp } from './types'

/** 节点监控：调度中心各副本的 CPU/内存/GC/HTTP 连接 + worker 的 runtime 指标（ADMIN 及以上） */
export function getNodes() {
  return request<NodesResp>(http.get('/system/nodes'))
}
