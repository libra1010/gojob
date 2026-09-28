// Package protocol 定义 Server 与 Worker 之间的通信协议（HTTP + JSON）。
// 字段一律显式 json tag，兼容 encoding/json/v2 的大小写敏感匹配语义。
package protocol

// ---------- 管理面（Dashboard / OpenAPI -> Server） ----------

type CreateAppReq struct {
	AppId    string `json:"app_id"`
	AppName  string `json:"app_name"`
	Password string `json:"password"`
}

type CreateJobReq struct {
	AppId              string `json:"app_id"`
	JobName            string `json:"job_name"`
	JobDescription     string `json:"job_description"`
	Processor          string `json:"processor"`            // worker 侧注册的处理器名（Java SDK 用类全名）
	ProcessorType      string `json:"processor_type"`       // 兼容协议的 ProcessorType 枚举：BUILT_IN | EXTERNAL，默认 BUILT_IN
	ExecuteType        string `json:"execute_type"`         // 一期仅 STANDALONE 真实生效（SHARDING/BROADCAST 保留字段）
	TimeExpressionType string `json:"time_expression_type"` // CRON | FIXED_RATE | FIXED_DELAY | DAILY_TIME_INTERVAL | API
	TimeExpression     string `json:"time_expression"`      // CRON=cron 表达式；FIXED_RATE/FIXED_DELAY=周期毫秒数；API 留空
	JobParams          string `json:"job_params"`           // 处理器收到的静态参数（JSON 字符串）
	InstanceParams     string `json:"instance_params"`      // 每次触发的默认运行时参数（兼容协议的 instanceParams 语义）
	// 这四个用指针：0 都是合法取值（不重试 / 不限实例数），值类型分不出"没传"和"传了 0"。
	MaxRetryTimes          *int   `json:"max_retry_times"` // 任务级重试次数
	InstanceRetryNum       *int   `json:"instance_retry_num"`
	Concurrency            *int   `json:"concurrency"`              // 单实例并发执行数
	InstanceNumLimit       *int   `json:"instance_num_limit"`       // 同时运行的实例上限，0=不限制
	SkipWhenRunning        bool   `json:"skip_when_running"`        // 执行中则不发起新任务
	DesignatedWorkers      string `json:"designated_workers"`       // 逗号分隔 ip:port，空=任意在线机器
	MaxWorkerCount         int    `json:"max_worker_count"`         // 0=不限
	DispatchStrategy       int    `json:"dispatch_strategy"`        // 见 Dispatch* 常量，0=健康优先
	DispatchStrategyConfig string `json:"dispatch_strategy_config"` // 仅 SPECIFY 使用
	UpstreamJobId          uint64 `json:"upstream_job_id"`          // 依赖触发：上游任务成功即触发本任务，0=无上游
	LogLevel               string `json:"log_level"`                // DEBUG/INFO/WARN/ERROR/OFF，默认 INFO
}

type ModifyJobReq struct {
	JobId                  uint64  `json:"job_id"`
	JobName                *string `json:"job_name"`
	JobDescription         *string `json:"job_description"`
	Processor              *string `json:"processor"`
	ProcessorType          *string `json:"processor_type"`
	ExecuteType            *string `json:"execute_type"`
	TimeExpressionType     *string `json:"time_expression_type"`
	TimeExpression         *string `json:"time_expression"`
	JobParams              *string `json:"job_params"`
	InstanceParams         *string `json:"instance_params"`
	MaxRetryTimes          *int    `json:"max_retry_times"`
	InstanceRetryNum       *int    `json:"instance_retry_num"`
	Concurrency            *int    `json:"concurrency"`
	InstanceNumLimit       *int    `json:"instance_num_limit"`
	SkipWhenRunning        *bool   `json:"skip_when_running"`
	DesignatedWorkers      *string `json:"designated_workers"`
	MaxWorkerCount         *int    `json:"max_worker_count"`
	DispatchStrategy       *int    `json:"dispatch_strategy"`
	DispatchStrategyConfig *string `json:"dispatch_strategy_config"`
	UpstreamJobId          *uint64 `json:"upstream_job_id"` // 传 0 = 取消依赖触发；不传 = 保持原值
	LogLevel               *string `json:"log_level"`
}

// 分发策略，取值与兼容协议的 DispatchStrategy 枚举同值同名。
// 21（轮询）是 gojob 扩展：兼容协议里没有 ROUND_ROBIN，其三种策略均无严格轮流语义。
const (
	DispatchHealthFirst = 1  // 健康度优先（默认）
	DispatchRandom      = 2  // 随机
	DispatchSpecify     = 11 // 指定机器（tagEquals:/tagIn:/逗号分隔 IP·tag）
	DispatchRoundRobin  = 21 // 按任务轮询分发（gojob 扩展）
)

type OperateJobReq struct {
	JobId  uint64 `json:"job_id"`
	Action string `json:"action"` // pause / resume / trigger
	// InstanceParams 仅 trigger 用：控制台「参数运行」传本次运行的实例参数，空=用任务里配的默认值。
	InstanceParams string `json:"instance_params"`
}

// PageResult 列表接口的分页包装，与兼容协议的会话接口 PageResult 语义一致：
// index 从 0 起；page_size<=0 表示不分页（一次给全，total_pages=1），供需要全量候选的下拉用。
type PageResult[T any] struct {
	Index      int   `json:"index"`
	PageSize   int   `json:"page_size"`
	TotalItems int64 `json:"total_items"`
	TotalPages int   `json:"total_pages"`
	Data       []T   `json:"data"`
}

type Resp[T any] struct {
	Code    int    `json:"code"` // 0 成功
	Message string `json:"message"`
	Data    T      `json:"data"`
}

const (
	CodeOK       = 0
	CodeBadReq   = 400
	CodeAuthErr  = 401
	CodeNotFound = 404
	CodeConflict = 409
	CodeErr      = 500
	// CodeChallengeRequired "要加密，但你发了明文"：既不是拒绝也不是口令错，
	// 而是"按 data 里的 challenge 重来一次"。故意不复用 401——SDK 把 401 当作
	// "票过期了，去重新注册"，用它当握手信号会转成注册死循环。
	CodeChallengeRequired = 428
)

// ---------- 数据面（Worker -> Server） ----------

type WorkerInfo struct {
	InstanceId    string  `json:"instance_id"` // 同一 address 重启后变化，用于识别失联节点
	AppId         string  `json:"app_id"`
	Address       string  `json:"address"` // ip:port
	CpuLoad       float64 `json:"cpu_load"`
	CpuProcessors int     `json:"cpu_processors"` // 可用核数，健康分 = (max-used)*2 + (核数-负载)
	MemoryUsed    float64 `json:"memory_used"`    // MB
	MemoryMax     float64 `json:"memory_max"`     // MB，0=未知（健康分退化为按已用内存升序）
	Tag           string  `json:"tag"`            // SPECIFY 策略的机器标签
	Version       string  `json:"version"`
	SdkType       string  `json:"sdk_type"` // GO | JAVA | JAVA_SPRINGBOOT | JAVA_QUARKUS

	// runtime 指标：节点监控页用。平台取不到的一律报 0（页面显示"—"），不猜数。
	// memory_used/memory_max 已经表达"堆已用/堆上限"，这里只补它表达不了的项。
	CpuPercent   float64 `json:"cpu_percent"`   // 本进程 CPU 占用，100=吃满一个核
	GcCount      int64   `json:"gc_count"`      // Go GC 次数 / JVM GC 次数合计
	GcPauseMs    float64 `json:"gc_pause_ms"`   // 启动以来累计 GC 暂停
	RssMb        float64 `json:"rss_mb"`        // 进程常驻物理内存
	Threads      int     `json:"threads"`       // goroutine 数 / JVM 线程数
	OpenFDs      int     `json:"open_fds"`      // 打开的 fd（含 socket）
	RunningTasks int     `json:"running_tasks"` // 正在执行的任务数（=并发额度占用）

}

type HeartbeatReq struct {
	WorkerInfo
	Token string `json:"token"` // /worker/register 签发的短周期凭证，不再携带项目口令
}

// RegisterReq：worker 用项目口令换取数据面 token 的唯一入口。
// 口令只在注册（以及过期后重新注册）时走一次，其余数据面请求只带 token。
// RegisterReq 注册。口令有两种送法，由 auth_scheme 区分（见 server/auth/registercipher.go）：
//   - ""/plain：口令直接放 password（服务端未配 AES key 时的唯一方式）；
//   - aes-gcm ：password 留空，口令在 ciphertext 里，challenge 必须是刚领到且未用过的那张。
//
// 加密方式解决的只是"口令不上线 + 抓到的注册包不可重用"，链路本身仍是明文。
type RegisterReq struct {
	InstanceId string `json:"instance_id"`
	AppId      string `json:"app_id"`
	Password   string `json:"password"`
	Address    string `json:"address"`
	Version    string `json:"version"`
	SdkType    string `json:"sdk_type"`

	AuthScheme string `json:"auth_scheme,omitempty"` // "" | plain | aes-gcm
	Challenge  string `json:"challenge,omitempty"`   // 一次性凭证原文
	Kid        string `json:"kid,omitempty"`         // 用哪把 key（key 自身指纹）
	Iv         string `json:"iv,omitempty"`          // AES-GCM nonce，base64，12 字节
	Ciphertext string `json:"ciphertext,omitempty"`  // base64，认证标签附在尾部（JCE 同约定）
}

// ChallengeReq 领一次性 challenge（加密握手第一步）。worker 侧两个字段都要；
// OpenAPI 兼容层的 /openApi/challenge 用 appName，见 server/api/registercipher_api.go。
type ChallengeReq struct {
	InstanceId string `json:"instance_id"`
	AppId      string `json:"app_id"`
}

// ChallengeResp 领一次性 challenge。schemes 让客户端"看得见"服务端接受什么方式，
// 错配时报得出原因，而不是撞一个含混的 401。
type ChallengeResp struct {
	Challenge string   `json:"challenge"`
	ExpiresIn int      `json:"expires_in"` // 秒
	Schemes   []string `json:"schemes"`    // 例如 ["plain","aes-gcm"] 或只有 ["aes-gcm"]
}

type RegisterResp struct {
	Token      string `json:"token"`
	TtlSeconds int    `json:"ttl_seconds"` // 每次成功调用数据面接口都会滑动续期，静默超过该时长才失效
}

// CancelTasks 下行指令载体：pull 模型唯一的"服务端→worker"通道就是响应夹带。
// 停止指令持久化在 instance_tasks.stop_request_time 上，因此 server 重启不丢指令。
type CancelTasks struct {
	CancelTaskIds []uint64 `json:"cancel_task_ids"`
}

// ClusterHint 集群路由提示，随数据面响应下发（与兼容协议的 app_info.current_server 语义对应）。
//
// 与兼容目标的关键差别：它那里 current_server 是**唯一通路**（server 主动 push 任务，
// worker 找不到 owner 就饿死，所以必须配 PING 判活、选举锁、连续失联就自杀本地秒级任务）；
// 我们是 pull 模型 + 在线表在 DB，任何副本都能发活，所以 owner 只是**首选地址的提示**——
// 猜错、猜晚、甚至完全拿不到，代价只是一次跨副本信号，不会让任务失败。
type ClusterHint struct {
	Servers   []string `json:"servers"`    // 集群各副本的 worker 可达地址（含本副本）
	Owner     string   `json:"owner"`      // 本 app 调度归属副本的可达地址；空=没有可用提示（单机/租约无人持有/地址未配）
	OwnerNode string   `json:"owner_node"` // 归属副本的 node_id，只用于日志与排障，不参与路由判断
}

type HeartbeatResp struct {
	CancelTasks             // 内联：保持 cancel_task_ids 在 data 顶层，老 SDK 与只解 CancelTasks 的调用方不受影响
	Cluster     ClusterHint `json:"cluster"`
}

// FetchTaskReq 长轮询拉取任务；WaitSeconds 为服务端最大阻塞时间。
type FetchTaskReq struct {
	InstanceId  string `json:"instance_id"`
	AppId       string `json:"app_id"`
	Token       string `json:"token"`
	Address     string `json:"address"`
	MaxTaskNum  int    `json:"max_task_num"`
	WaitSeconds int    `json:"wait_seconds"`
}

// 处理器类型：兼容协议 ProcessorType 枚举的字符串值。SHELL/PYTHON 在兼容目标的 5.x 已
// @Deprecated 且其 worker 侧没有任何 ProcessorFactory 支持它们，脚本任务的正解是
// BUILT_IN + 官方脚本处理器的类全名（见 OfficialShellProcessor）。
const (
	ProcessorBuiltIn  = "BUILT_IN"
	ProcessorExternal = "EXTERNAL" // 兼容协议的 JAR 动态容器：容器ID#类名
)

// OfficialShellProcessor 是官方脚本处理器的类全名，即兼容协议里的
// tech.powerjob.official.processors.impl.script.ShellProcessor。兼容目标的
// "Shell 任务"就是 BUILT_IN + 这个类名 + 脚本文本放执行参数，gojob-agent 注册同名处理器，
// 于是同一份任务配置在两边都能跑；导入旧的 SHELL 类型任务时也改写成它。
const OfficialShellProcessor = "tech.powerjob.official.processors.impl.script.ShellProcessor"

// 执行类型：兼容协议 ExecuteType 枚举的字符串值。STANDALONE 一实例一任务；BROADCAST 由
// server 按在线节点枚举、每台一份；MAP 允许处理器在运行期派生任意多子任务；MAP_REDUCE 在
// MAP 之上多一个 reduce 任务（子树排空后执行，它的返回值决定实例终态与结果）。
// WORKFLOW 不在本期范围（gojob 用依赖触发表达前后置关系）。
const (
	ExecuteStandalone = "STANDALONE"
	ExecuteBroadcast  = "BROADCAST"
	ExecuteMap        = "MAP"
	ExecuteMapReduce  = "MAP_REDUCE"
	ExecuteWorkflow   = "WORKFLOW"
)

// 保留任务名：兼容协议 OmsTask 的三个内部任务名。用户在 map() 里用这三个名字会被拒，
// 否则收敛判定会把用户任务误当内部任务。
const (
	RootTaskName      = "OMS_ROOT_TASK"
	LastTaskName      = "OMS_LAST_TASK"
	BroadcastTaskName = "OMS_BROADCAST_TASK"
	RootTaskKey       = "0"
	LastTaskKey       = "9999"
)

// CanMap 只有 MAP/MAP_REDUCE 的实例里允许处理器派生子任务——兼容协议同语义：
// STANDALONE 里调 map 直接抛异常，而不是静默忽略。
func CanMap(executeType string) bool {
	return executeType == ExecuteMap || executeType == ExecuteMapReduce
}

type TaskDTO struct {
	TaskId     uint64 `json:"task_id"`
	JobId      uint64 `json:"job_id"`
	InstanceId uint64 `json:"instance_id"`
	AppId      string `json:"app_id"`
	Processor  string `json:"processor"`
	// ProcessorType 处理器类型（与兼容协议 ServerScheduleJobReq.processorType 同义）：worker 用它
	// 区分"按注册名/类名找实现"（BUILT_IN）与"要动态加载容器"（EXTERNAL，gojob 未实现，
	// 明确报错比"没注册"好查），对应兼容协议 ProcessorFactory.supportTypes 的分派。
	ProcessorType string `json:"processor_type"`
	ExecuteType   string `json:"execute_type"`
	// TaskKey/TaskName/SubTaskContent 是多任务形态的三件套：根任务 TaskKey="0"、负载为空，
	// map 出来的子任务带自己的层次 ID、用户起的 taskName 和 JSON 负载。
	// 对应兼容协议 TaskContext 的 getTaskId()/getTaskName()/getSubTask()。
	TaskKey        string `json:"task_key"`
	TaskName       string `json:"task_name"`
	SubTaskContent string `json:"sub_task_content"`
	JobParams      string `json:"job_params"`
	FixedParams    string `json:"fixed_params"` // 单次触发的运行时参数（兼容协议的 instanceParams 语义）
	Attempts       int    `json:"attempts"`     // 第几次尝试，从 1 开始
	RetryTimes     int    `json:"retry_times"`
	LogLevel       string `json:"log_level"` // 任务配置的日志级别，worker 侧过滤
}

type FetchTaskResp struct {
	Tasks       []TaskDTO `json:"tasks"`
	CancelTasks           // 内联：无新任务时靠它把停止指令带回 worker
}

// ReportTaskReq Worker 回报任务执行结果。
type ReportTaskReq struct {
	InstanceId string `json:"instance_id"`
	AppId      string `json:"app_id"`
	Token      string `json:"token"`
	TaskId     uint64 `json:"task_id"`
	Success    bool   `json:"success"`
	Status     string `json:"status"` // 可选 STOPPED/SUCCEED/FAILED；留空按 success 推导（兼容旧 SDK）
	Result     string `json:"result"` // 处理器返回值或错误信息
}

// MapTaskReq 处理器在运行期派生子任务（兼容协议的 ProcessorMapTaskRequest 对位）。
//
// 兼容协议里这一步是 worker 内部 ask 给本机 TaskTracker、写进该节点的本地 task_info 表；
// gojob 是 pull 模型且 worker 之间不通信，所以子任务必须经 server 落库后再走正常认领通道。
// BatchNo 是"本任务第几次调 map"（SDK 侧自增，HTTP 重放时保持不变），与 TaskId 一起构成
// 幂等键：重放同一批只会拿到已建好的任务 ID，不会重复派生。
type MapTaskReq struct {
	InstanceId string   `json:"instance_id"` // worker instance id，与 Report 同源，用于校验任务归属
	AppId      string   `json:"app_id"`
	Token      string   `json:"token"`
	TaskId     uint64   `json:"task_id"`
	TaskName   string   `json:"task_name"`
	BatchNo    int      `json:"batch_no"`
	SubTasks   []string `json:"sub_tasks"` // 每个子任务的 JSON 负载
}

type MapTaskResp struct {
	TaskKeys []string `json:"task_keys"`
	Created  int      `json:"created"` // 本次真正新建了几条（重放同一批时为 0）
}

// TaskResultsReq 取实例下已终态子任务的结果清单，供 reduce 使用。
// 分页而不是一次塞进 TaskDTO：一万个子任务、每个 8KB 结果时，塞进拉取响应会先撑爆内存。
type TaskResultsReq struct {
	InstanceId string `json:"instance_id"`
	AppId      string `json:"app_id"`
	Token      string `json:"token"`
	TaskId     uint64 `json:"task_id"` // 必须是 reduce 任务本身，server 据此校验归属
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
}

// TaskResult 单个子任务的执行结果（兼容协议的 TaskResult：taskId + success + result）。
type TaskResult struct {
	TaskKey  string `json:"task_key"`
	TaskName string `json:"task_name"`
	Success  bool   `json:"success"`
	Result   string `json:"result"`
}

type TaskResultsResp struct {
	Results  []TaskResult `json:"results"`
	Total    int          `json:"total"`
	HasMore  bool         `json:"has_more"`
	Finished bool         `json:"finished"` // 该实例的子树是否已全部终态（reduce 的进入条件）
}

// InstanceDetailReq 实例详情弹窗（与兼容协议的 /instance/detailPlus 同位）。
// CustomQuery 是控制台里那句 "select * from task_info where ___ limit 10" 的中间部分，
// 服务端只认白名单语法、绝不拼进 SQL（见 server/api/instance_detail.go）。
type InstanceDetailReq struct {
	InstanceId  uint64 `json:"instance_id"`
	AppId       string `json:"app_id"`
	CustomQuery string `json:"custom_query"`
}

// StopInstanceReq 停止实例（与兼容协议的 stopInstance 语义一致）。
// AppId 仅用于 RBAC 校验（非超管需按项目授权）；SUPER_ADMIN 可省略。
type StopInstanceReq struct {
	InstanceId uint64 `json:"instance_id"`
	AppId      string `json:"app_id"`
}

// ---------- 日志上报（Worker -> Server，批量 + ack 语义） ----------
// 可靠性约定：entries 携带任务内单调递增 seq；Server 按 (task_id, seq) 唯一索引幂等去重；
// Worker 收到 code=0 的 ack 才出队，失败/超时持续退避重发 —— at-least-once 传输 + 去重。

type TaskLogEntry struct {
	TaskId     uint64 `json:"task_id"`
	InstanceId uint64 `json:"instance_id"`
	Seq        uint64 `json:"seq"`  // 任务内从 1 递增
	Time       int64  `json:"time"` // 毫秒时间戳
	Level      string `json:"level"`
	Content    string `json:"content"`
}

type ReportLogsReq struct {
	InstanceId string         `json:"instance_id"` // worker instance id
	AppId      string         `json:"app_id"`
	Token      string         `json:"token"`
	Entries    []TaskLogEntry `json:"entries"`
}

// WorkerTokenBearer 供 SDK 统一给数据面请求填 token：
// 收到 401 时重新注册、填上新 token 再重放一次。
type WorkerTokenBearer interface {
	SetWorkerToken(string)
}

func (r *HeartbeatReq) SetWorkerToken(tk string)   { r.Token = tk }
func (r *FetchTaskReq) SetWorkerToken(tk string)   { r.Token = tk }
func (r *ReportTaskReq) SetWorkerToken(tk string)  { r.Token = tk }
func (r *ReportLogsReq) SetWorkerToken(tk string)  { r.Token = tk }
func (r *MapTaskReq) SetWorkerToken(tk string)     { r.Token = tk }
func (r *TaskResultsReq) SetWorkerToken(tk string) { r.Token = tk }

// ---------- 统计概览（Dashboard 首页） ----------

type WorkerStat struct {
	Address    string  `json:"address"`
	AppId      string  `json:"app_id"`
	SdkType    string  `json:"sdk_type"`
	Version    string  `json:"version"`
	CpuLoad    float64 `json:"cpu_load"`
	MemoryUsed float64 `json:"memory_used"`
}

type AppStat struct {
	AppId       string `json:"app_id"`
	AppName     string `json:"app_name"`
	JobCount    int64  `json:"job_count"`
	WorkerCount int64  `json:"worker_count"`
	CreateTime  string `json:"create_time"`
}

type StatsOverviewResp struct {
	TotalUsers      int64              `json:"total_users"`
	TotalApps       int64              `json:"total_apps"`
	TotalJobs       int64              `json:"total_jobs"`
	EnabledJobs     int64              `json:"enabled_jobs"`
	OnlineWorkers   int64              `json:"online_workers"`
	ServerAddress   string             `json:"server_address"` // 对外访问地址
	DbDriver        string             `json:"db_driver"`
	TodayTotal      int64              `json:"today_total"`
	TodaySucceed    int64              `json:"today_succeed"`
	TodayFailed     int64              `json:"today_failed"`
	TodayRunning    int64              `json:"today_running"`
	SuccessRate     float64            `json:"success_rate"`
	Workers         []WorkerStat       `json:"workers"`
	Apps            []AppStat          `json:"apps"`
	RecentInstances []JobInstanceBrief `json:"recent_instances"`
}

type JobInstanceBrief struct {
	InstanceId uint64 `json:"instance_id"`
	JobId      uint64 `json:"job_id"`
	AppId      string `json:"app_id"`
	JobName    string `json:"job_name"`
	Status     string `json:"status"`
	Source     string `json:"source"`
	CreateTime string `json:"create_time"`
	ModifyTime string `json:"modify_time"`
}

// 任务/实例状态机
const (
	InstanceWaiting  = "WAITING"
	InstanceRunning  = "RUNNING"
	InstanceStopping = "STOPPING" // 已下达停止指令，等待 worker 协作退出
	InstanceStopped  = "STOPPED"
	InstanceSucceed  = "SUCCEED"
	InstanceFailed   = "FAILED"

	TaskWaiting = "WAITING"
	TaskRunning = "RUNNING"
	TaskStopped = "STOPPED" // 被停止指令终止，不计入失败、不重试
	TaskSucceed = "SUCCEED"
	TaskFailed  = "FAILED"
	// TaskDiscarded 父任务被重派时，它尚未收敛的后代子任务被作废（重跑会重新 map 出新的子任务，
	// 保留旧的会双跑）。这是 gojob 的终态，既不算失败也不算用户停止，所以不参与实例终态判定。
	TaskDiscarded = "DISCARDED"
)

// IsTerminalTask 任务是否已收敛。新增状态时必须回头检查这个函数：漏一个就会让实例提前定终态。
func IsTerminalTask(status string) bool {
	switch status {
	case TaskSucceed, TaskFailed, TaskStopped, TaskDiscarded:
		return true
	}
	return false
}

// 实例触发来源（job_instance.source）。DEPENDENCY 是 gojob 扩展：
// 兼容协议用工作流(DAG)表达任务依赖，这里允许下游任务直接挂一个上游任务，上游实例成功后自动触发。
const (
	SourceDependency = "DEPENDENCY"
)

// ---------- 用户与登录 ----------

type LoginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type LoginResp struct {
	Token    string `json:"token"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Role     string `json:"role"`
}

// ---------- App（项目）管理 ----------

type ModifyAppReq struct {
	AppId    string  `json:"app_id"`
	AppName  *string `json:"app_name"`
	Password *string `json:"password"` // 空串=不修改
}

type DeleteAppReq struct {
	AppId string `json:"app_id"`
}

// SetWorkerAcceptReq 管理面翻某台 worker 的接单开关（true=恢复接单，false=不再拉新任务）。
// 语义边界：在跑的任务不受影响，继续执行并回报；只是不再派新任务给它。
type SetWorkerAcceptReq struct {
	AppId      string `json:"app_id"`
	InstanceId string `json:"instance_id"`
	Accept     bool   `json:"accept"`
}

type DeleteJobReq struct {
	JobId uint64 `json:"job_id"`
}

// ---------- 兼容协议格式任务导入 ----------
// 字段与兼容目标控制台导出的 JobDTO JSON 对齐（camelCase），指针表示"缺省即忽略"。

type ImportJobsReq struct {
	AppId string   `json:"app_id"`
	Jobs  []JobDTO `json:"jobs"`
}

type JobDTO struct {
	JobName                *string `json:"jobName"`
	JobDescription         *string `json:"jobDescription"`
	TimeExpressionType     *string `json:"timeExpressionType"`
	TimeExpression         *string `json:"timeExpression"`
	ExecuteType            *string `json:"executeType"`
	ProcessorType          *string `json:"processorType"`
	ProcessorInfo          *string `json:"processorInfo"`
	JobParams              *string `json:"jobParams"`
	MaxInstanceNum         *int    `json:"maxInstanceNum"`
	Concurrency            *int    `json:"concurrency"`
	InstanceRetryNum       *int    `json:"instanceRetryNum"`
	TaskRetryNum           *int    `json:"taskRetryNum"`
	DesignatedWorkers      *string `json:"designatedWorkers"`
	MaxWorkerCount         *int    `json:"maxWorkerCount"`
	DispatchStrategy       *int    `json:"dispatchStrategy"`
	DispatchStrategyConfig *string `json:"dispatchStrategyConfig"`
	Enable                 *bool   `json:"enable"`
	// 以下是 gojob 扩展字段：兼容协议的导入方会忽略未知键，gojob 自己导出再导入则不丢配置。
	InstanceParams  *string `json:"instanceParams"`  // 每次触发的默认运行时参数
	SkipWhenRunning *bool   `json:"skipWhenRunning"` // 任务执行中则不发起新任务
	LogLevel        *string `json:"logLevel"`
}

type ImportJobItem struct {
	JobName string `json:"job_name"`
	JobId   uint64 `json:"job_id"`
	Success bool   `json:"success"`
	Reason  string `json:"reason"`
}

type ImportJobsResp struct {
	Total     int             `json:"total"`
	Succeeded int             `json:"succeeded"`
	Failed    int             `json:"failed"`
	Items     []ImportJobItem `json:"items"`
}
