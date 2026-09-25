// 与 internal/protocol/protocol.go、server/store/model.go 的 json tag 一一对应。
// 注意：uint64 ID 在此规模下直接用 number 承接。

/** 统一响应信封 {code, message, data}，code=0 成功，401 未登录 */
export interface Resp<T> {
  code: number
  message: string
  data: T
}

// ---------- 用户与登录 ----------

export interface LoginReq {
  username: string
  password: string
}

export interface LoginResp {
  token: string
  username: string
  nickname: string
  role: Role
}

export type Role = 'SUPER_ADMIN' | 'ADMIN' | 'READONLY'
export type UserStatus = 'ACTIVE' | 'DISABLED'

export interface MeResp {
  username: string
  nickname: string
  role: Role
  status: UserStatus
  app_scopes: string[]
}

export interface UserInfo {
  id: number
  username: string
  nickname: string
  role: Role
  status: UserStatus
  app_scopes: string[]
  create_time: string
}

export interface UserReq {
  username: string
  password?: string
  nickname?: string
  role?: Role
  status?: UserStatus
  app_scopes?: string[]
}

export interface LoginLog {
  id: number
  username: string
  ip: string
  user_agent: string
  success: boolean
  reason: string
  login_time: string
}

export interface OperationLog {
  id: number
  username: string
  role: Role
  action: string
  app_id: string
  method: string
  path: string
  params: string
  result_code: number
  success: boolean
  ip: string
  create_time: string
}

/** 分页审计响应 */
export interface PageResp<T> {
  total: number
  rows: T[]
}

// ---------- App（项目） ----------

export interface AppInfo {
  id: number
  app_id: string
  app_name: string
  create_time: string
  modify_time: string
  /** 当前持有该项目调度租约的 Server 副本 node_id；为空表示暂无活副本调度该项目 */
  current_server: string
}

export interface CreateAppReq {
  app_id: string
  app_name: string
  password: string
}

/** 指针字段：缺省即不修改；password 空串=不修改 */
export interface ModifyAppReq {
  app_id: string
  app_name?: string
  password?: string
}

// ---------- 在线 Worker 节点 ----------

/** SDK 类型，与服务端 registry 一致 */
export type SdkType = 'GO' | 'JAVA' | 'JAVA_SPRINGBOOT' | 'JAVA_QUARKUS'

export interface WorkerInfo {
  instance_id: string
  app_id: string
  address: string
  cpu_load: number
  /** 可用 CPU 核数（健康分用） */
  cpu_processors: number
  memory_used: number
  /** 内存上限 MB，0=未知 */
  memory_max: number
  /** 机器标签，SPECIFY 分发策略按它匹配 */
  tag: string
  version: string
  sdk_type: SdkType | string
  /** 以下 runtime 指标由心跳上报，平台取不到时为 0（页面显示“—”） */
  cpu_percent: number
  gc_count: number
  gc_pause_ms: number
  rss_mb: number
  /** goroutine 数 / JVM 线程数 */
  threads: number
  open_fds: number
  running_tasks: number
}

// ---------- 节点监控 ----------

/** server_nodes.samples 列：按列存的采样序列，前端直接喂给 sparkline */
export interface NodeSeries {
  t: number[]
  cpu: number[]
  mem: number[]
  gc: number[]
  conns: number[]
  qps: number[]
  poll: number[]
}

export interface ServerNode {
  node_id: string
  hostname: string
  address: string
  public_url: string
  boot_time: string
  sample_time: string
  online: boolean
  /** 距最后一次心跳的毫秒数，用于分辨“刚掉线”和“早就死了” */
  silent_ms: number
  cpu_percent: number
  rss_mb: number
  heap_inuse_mb: number
  goroutines: number
  gc_count: number
  gc_pause_ms: number
  conns_active: number
  conns_idle: number
  poll_inflight: number
  req_inflight: number
  req_per_sec: number
  samples: NodeSeries | null
  /** 该副本当前持有调度归属的项目（租约未过期才算它的） */
  owned_apps: string[] | null
  owned_count: number
}

/** 一个项目的调度归属：同一时刻只有一个副本调度同一个项目 */
export interface LeaseHolder {
  app_id: string
  holder: string
  epoch: number
  lease_expire: string
  /** false = 租约已过期或从未有副本持有：此时该项目暂时没有副本在调度 */
  alive: boolean
}

export interface WorkerNode extends WorkerInfo {
  last_heartbeat: string
  online: boolean
}

export interface NodesResp {
  interval_ms: number
  offline_after_ms: number
  /** 处理本次请求的副本 node_id：多副本下用它标出"你正在看的是谁的自报数据之外的全局视图" */
  self: string
  lease_ttl_ms: number
  leases: LeaseHolder[] | null
  servers: ServerNode[]
  workers: WorkerNode[]
}

// ---------- Job（任务） ----------

/**
 * PowerJob 的 ProcessorType（5.1.2 枚举）：BUILT_IN(内建，按 worker 注册名/类全名解析) /
 * EXTERNAL(外置，PowerJob 用 JAR 动态容器，gojob 的 worker 会直接判失败)。
 * SHELL/PYTHON 在 PowerJob 已 @Deprecated，导入时会被改写成 BUILT_IN + 官方脚本处理器。
 */
export type ProcessorType = 'BUILT_IN' | 'EXTERNAL'
/**
 * 执行类型，与 PowerJob ExecuteType 枚举同名同值。
 * WORKFLOW 属于工作流引擎，gojob 未实现（任务前后置关系用时间类型 DEPENDENCY 表达），
 * 因此不出现在建单下拉里。
 */
export type ExecuteType = 'STANDALONE' | 'BROADCAST' | 'MAP' | 'MAP_REDUCE' | 'WORKFLOW'
export type LogLevel = 'DEBUG' | 'INFO' | 'WARN' | 'ERROR' | 'OFF'
/** DEPENDENCY = gojob 扩展的调度方式：不按时触发，由上游任务实例成功后拉起 */
export type TimeExpressionType =
  | 'CRON'
  | 'FIXED_RATE'
  | 'FIXED_DELAY'
  | 'DAILY_TIME_INTERVAL'
  | 'API'
  | 'DEPENDENCY'

/** 分发策略：1/2/11 与 PowerJob DispatchStrategy 同值，21 是 gojob 扩展（PowerJob 无轮询） */
export type DispatchStrategy = 1 | 2 | 11 | 21

/** 每日固定间隔表达式（JSON 存进 time_expression），对齐 PowerJob DailyTimeIntervalExpress */
export interface DailyIntervalExpress {
  interval: number
  intervalUnit?: 'MILLISECONDS' | 'SECONDS' | 'MINUTES' | 'HOURS'
  startTimeOfDay: string
  endTimeOfDay: string
  /** 1(周一)~7(周日)，空=每天 */
  daysOfWeek?: number[]
}

export interface JobInfo {
  id: number
  app_id: string
  job_name: string
  job_description: string
  processor: string
  processor_type: ProcessorType | string
  execute_type: ExecuteType | string
  time_expression_type: TimeExpressionType | string
  time_expression: string
  job_params: string
  instance_params: string
  max_retry_times: number
  instance_retry_num: number
  concurrency: number
  instance_num_limit: number
  skip_when_running: boolean
  designated_workers: string
  max_worker_count: number
  dispatch_strategy: DispatchStrategy | number
  dispatch_strategy_config: string
  /** 依赖触发（gojob 扩展）：上游任务 ID，0=无上游；上游实例成功后本任务自动触发一次 */
  upstream_job_id: number
  log_level: LogLevel | string
  enable: boolean
  next_trigger_time: string
  life_cycle: number
  create_time: string
  modify_time: string
}

export interface CreateJobReq {
  app_id: string
  job_name: string
  job_description: string
  processor: string
  /** BUILT_IN | EXTERNAL，缺省后端按 BUILT_IN 处理 */
  processor_type: string
  /** 一期仅 STANDALONE 生效 */
  execute_type: string
  time_expression_type: string
  time_expression: string
  job_params: string
  instance_params: string
  max_retry_times: number
  instance_retry_num: number
  concurrency: number
  instance_num_limit: number
  skip_when_running: boolean
  /** 逗号分隔 ip:port，空串=不指定机器 */
  designated_workers: string
  /** 0=不限制 */
  max_worker_count: number
  /** 0 或省略 = 健康优先 */
  dispatch_strategy: number
  /** 仅分发策略=指定机器时生效：tagEquals:xxx / tagIn:xxx / 逗号分隔 ip:port */
  dispatch_strategy_config: string
  /** 依赖触发：上游任务 ID，0=不依赖 */
  upstream_job_id: number
  log_level: string
}

/** 指针字段：缺省即不修改；表单编辑时全量下发 */
export interface ModifyJobReq {
  job_id: number
  job_name?: string
  job_description?: string
  processor?: string
  processor_type?: string
  execute_type?: string
  time_expression_type?: string
  time_expression?: string
  job_params?: string
  instance_params?: string
  max_retry_times?: number
  instance_retry_num?: number
  concurrency?: number
  instance_num_limit?: number
  skip_when_running?: boolean
  designated_workers?: string
  max_worker_count?: number
  dispatch_strategy?: number
  dispatch_strategy_config?: string
  /** 依赖触发：传 0 取消依赖，省略保持原值 */
  upstream_job_id?: number
  log_level?: string
}

export type JobAction = 'pause' | 'resume' | 'trigger'

/** 列表接口的分页包装，语义对齐 PowerJob 的 PageResult：index 从 0 起，page_size<=0 表示一次给全量 */
export interface PageResult<T> {
  index: number
  page_size: number
  total_items: number
  total_pages: number
  data: T[] | null
}

/** 任务列表查询条件（PowerJob 任务管理页只有这两个过滤器：任务 ID 精确 + 关键字模糊） */
export interface JobListQuery {
  app_id: string
  /** 0 基页码 */
  index?: number
  /** <=0 = 不分页，返回全量 */
  page_size?: number
  job_id?: number | string
  keyword?: string
}

export interface OperateJobReq {
  job_id: number
  action: JobAction
  /** 仅 trigger：非空即「参数运行」，只影响这一次运行 */
  instance_params?: string
}

// ---------- PowerJob 导入 ----------

/** PowerJob 控制台导出的 JobDTO（camelCase），与 protocol.PowerJobJob 对齐 */
export interface PowerJobJob {
  jobName?: string
  jobDescription?: string
  timeExpressionType?: string
  timeExpression?: string
  executeType?: string
  processorType?: string
  processorInfo?: string
  jobParams?: string
  maxInstanceNum?: number
  concurrency?: number
  instanceRetryNum?: number
  taskRetryNum?: number
  designatedWorkers?: string
  maxWorkerCount?: number
  dispatchStrategy?: number
  dispatchStrategyConfig?: string
  enable?: boolean
  /** gojob 扩展字段：PowerJob 的导入方会忽略未知键，gojob 自己导出再导入则不丢配置 */
  instanceParams?: string
  skipWhenRunning?: boolean
  logLevel?: string
}

export interface ImportJobsReq {
  app_id: string
  jobs: PowerJobJob[]
}

export interface ImportJobItem {
  job_name: string
  job_id: number
  success: boolean
  reason: string
}

export interface ImportJobsResp {
  total: number
  succeeded: number
  failed: number
  items: ImportJobItem[]
}

// ---------- 实例 ----------

export type InstanceStatus = 'WAITING' | 'RUNNING' | 'STOPPED' | 'SUCCEED' | 'FAILED'

/** 终态：不再产生新任务/日志，可停止轮询 */
export const FINAL_STATUSES: readonly string[] = ['SUCCEED', 'FAILED', 'STOPPED']

export function isFinalStatus(status: string | undefined | null): boolean {
  return !!status && FINAL_STATUSES.includes(status)
}

export interface JobInstance {
  id: number
  job_id: number
  app_id: string
  job_name: string
  processor: string
  job_params: string
  status: InstanceStatus | string
  fixed_params: string
  /** 建单时从任务抄下来的执行类型：决定这是一次执行还是一棵任务树 */
  execute_type?: ExecuteType | string
  /** 实例级结果（PowerJob InstanceInfoDO.result）：MAP_REDUCE 取 reduce 的返回值，其余取根任务 */
  result?: string
  running_times: number
  try_times: number
  total_task_num: number
  failed_task_num: number
  source: string // CRON / FIXED_RATE / FIXED_DELAY / DAILY_TIME_INTERVAL / API / DEPENDENCY
  /** 依赖触发的实例：拉起本实例的上游实例 ID；非依赖触发为 null */
  upstream_instance_id?: number | null
  create_time: string
  modify_time: string
}

// ---------- 实例任务与任务日志 ----------

// DISCARDED 是 gojob 独有状态：父任务重派时它未收敛的后代被作废，既不算失败也不触发下游
export type TaskStatus = 'WAITING' | 'RUNNING' | 'SUCCEED' | 'FAILED' | 'STOPPED' | 'DISCARDED'

export interface InstanceTask {
  id: number
  job_id: number
  instance_id: number
  app_id: string
  processor: string
  job_params: string
  fixed_params: string
  status: TaskStatus | string
  worker_id: string
  attempts: number
  max_attempts: number
  result: string
  log_level: LogLevel | string
  dispatch_time: string
  create_time: string
  modify_time: string
  /** 多任务形态：根任务 parent_task_id=0、task_key="0"，reduce 任务 task_key 固定 "9999" */
  parent_task_id?: number
  task_key?: string
  task_name?: string
  sub_task_content?: string
  execute_type?: ExecuteType | string
}

/** 实例详情弹窗（POST /openapi/instance/detailPlus）——对齐 PowerJob 的 InstanceDetail */
export interface TaskInfoRow {
  task_id: string
  task_name: string
  task_content: string
  processor_addr: string
  failed_cnt: number
  status: TaskStatus | string
  status_str: string
  created_time: string
  last_modified_time: string
  result: string
}

export interface TaskStats {
  total: number
  waiting: number
  running: number
  succeed: number
  failed: number
  stopped: number
  discarded: number
}

export interface InstanceDetail {
  instance: JobInstance
  task_stats: TaskStats
  tasks: TaskInfoRow[]
  total: number
  /** 子任务查询语句没被理解时给的原因；非空表示这一页没按输入过滤 */
  query_error?: string
}

/** 单条任务日志，seq 在任务内从 1 递增，增量拉取游标 */
export interface TaskLog {
  id: number
  task_id: number
  instance_id: number
  seq: number
  level: LogLevel | string
  /** 毫秒时间戳 */
  log_time: number
  content: string
  create_time: string
}

// ---------- 统计总览 ----------

export interface WorkerStat {
  address: string
  app_id: string
  sdk_type: SdkType | string
  version: string
  cpu_load: number
  memory_used: number
}

export interface AppStat {
  app_id: string
  app_name: string
  job_count: number
  worker_count: number
  create_time: string
}

export interface JobInstanceBrief {
  instance_id: number
  job_id: number
  app_id: string
  job_name: string
  status: InstanceStatus | string
  source: string
  create_time: string
  modify_time: string
}

export interface StatsOverview {
  total_users: number
  total_apps: number
  total_jobs: number
  enabled_jobs: number
  online_workers: number
  /** 配置的对外访问地址 */
  server_address: string
  /** sqlite | mysql | postgres */
  db_driver: string
  today_total: number
  today_succeed: number
  today_failed: number
  today_running: number
  /** 百分比 */
  success_rate: number
  workers: WorkerStat[]
  apps: AppStat[]
  recent_instances: JobInstanceBrief[]
}
