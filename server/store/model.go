package store

import "time"

type AppInfo struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	// TokenEpoch 是该项目数据面/OpenAPI 令牌的吊销基线：无状态签名令牌里带着签发时的代号，
	// 改口令或删项目时代号 +1，此前签发的全部令牌即刻失效（各副本读同一列，无需共享存储）。
	TokenEpoch int64     `json:"token_epoch"`
	AppId      string    `gorm:"uniqueIndex;size:64" json:"app_id"`
	AppName    string    `gorm:"size:128" json:"app_name"`
	Password   string    `gorm:"size:128" json:"-"` // bcrypt(明文)：worker 数据面、控制台与兼容协议的 authApp 都要校验它
	CreateTime time.Time `json:"create_time"`
	ModifyTime time.Time `json:"modify_time"`
}

// AppLease 项目级的调度归属租约：同一时刻只有一个 Server 副本能调度某个 app 的任务，
// 这就是集群下"一个 job 一次触发只生成一个实例"的结构性保证（沿用兼容协议的
// app_info.current_server 归属模型，区别是我们用租约 TTL 判活，而不是它靠 worker 心跳反向 PING）。
//
// Epoch 只在**接管**时递增（续约不动），配合 holder 构成 fencing：
// 拿不到当前 (holder, epoch) 的写会被条件 UPDATE 拒掉，所以"旧主没死透还在调度"不可能写库。
// RenewCount 只增不减：既是 mysql 下 RowsAffected 恒为 1 的手段（改值才算 affected），
// 也是"这个副本到底续了多少跳"的观测量。
type AppLease struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	AppId       string    `gorm:"uniqueIndex;size:64" json:"app_id"`
	Holder      string    `gorm:"size:128;index" json:"holder"` // 持有者：server 副本 node_id（含启动纳秒，重启即新身份）
	Epoch       int64     `json:"epoch"`
	LeaseExpire time.Time `gorm:"index" json:"lease_expire"` // 超过此刻即视为无人持有，可被接管
	RenewCount  int64     `json:"renew_count"`
	CreateTime  time.Time `json:"create_time"`
	ModifyTime  time.Time `json:"modify_time"`
}

// WorkerNode 是 worker 心跳形成的在线节点表。落 DB 而不是留在单进程内存，
// 是为了让**每个副本看到的候选集一致**：分发策略（健康优先/指定机器/轮询）与节点监控
// 若只看"轮询过我的那些节点"，多副本 LB 后面会退化成残缺视图。
type WorkerNode struct {
	ID         uint64  `gorm:"primaryKey;autoIncrement" json:"id"`
	AppId      string  `gorm:"uniqueIndex:uk_worker_node;size:64" json:"app_id"`
	InstanceId string  `gorm:"uniqueIndex:uk_worker_node;size:64" json:"instance_id"`
	Address    string  `gorm:"size:128" json:"address"`
	CpuLoad    float64 `json:"cpu_load"`
	// 以下数值列同样不加 default 标签（见 ServerNode 注释）：0 都是合法读数。
	CpuProcessors int     `json:"cpu_processors"`
	MemoryUsed    float64 `json:"memory_used"`
	MemoryMax     float64 `json:"memory_max"`
	Tag           string  `gorm:"size:128" json:"tag"`
	Version       string  `gorm:"size:64" json:"version"`
	SdkType       string  `gorm:"size:32" json:"sdk_type"`
	CpuPercent    float64 `json:"cpu_percent"`
	GcCount       int64   `json:"gc_count"`
	GcPauseMs     float64 `json:"gc_pause_ms"`
	RssMb         float64 `json:"rss_mb"`
	Threads       int     `json:"threads"`
	OpenFDs       int     `gorm:"column:open_fds" json:"open_fds"` // 显式列名：gorm 会把 OpenFDs 切成 open_f_ds
	RunningTasks  int     `json:"running_tasks"`
	// AcceptTasks = 这个节点当前是否接收新任务。只有控制台的按钮会写它。
	// 选节点只看它=1 的节点，所以停接单不会让在跑的任务失败，只是不再派新任务给它。
	// 这一列要 default:true（与上面那些"0 也是合法读数"的指标列相反）：存量行必须被认成接单中。
	// worker 侧完全不知道这个开关的存在（不需要协议字段）：它照常长轮询，
	// 只是认领 SQL 永远筛不到属于它的任务，于是空转到超时。
	AcceptTasks   bool      `gorm:"default:true" json:"accept_tasks"`
	LastHeartbeat time.Time `gorm:"index" json:"last_heartbeat"`
	CreateTime    time.Time `json:"create_time"`
	ModifyTime    time.Time `json:"modify_time"`
}

// TokenRevoked 已消费的 jti：两类用途共用这一张表，语义是"这个 jti 用过了"。
//   - 登出：无状态令牌无法凭内存作废单张票，而退出又必须真的生效；
//   - 一次性注册 challenge：主键冲突 = 这张票已被用过，于是"只用一次"跨副本成立
//     （见 auth.Challenges.Redeemed）。真插入失败（查库报错）时按"没用过"返回错误，
//     宁可不放行，也不能让同一张 challenge 用两次。
//
// 行随对应令牌一起过期，由后台维护循环清理。
type TokenRevoked struct {
	Jti      string    `gorm:"primaryKey;size:64" json:"jti"`
	Purpose  string    `gorm:"size:16" json:"purpose"`
	Subject  string    `gorm:"size:64" json:"subject"`
	ExpireAt time.Time `gorm:"index" json:"expire_at"`
}

// TableName 显式命名：gorm 会把 TokenRevoked 复数成 token_revokeds，读起来不像张真表。
func (TokenRevoked) TableName() string { return "revoked_tokens" }

// ClusterSignal 单行计数器（id 恒为 SignalRowID）：任务入队/重排/停止意图 seq+1，
// 各副本轮询到变化就唤醒本机长轮询。pull 模型里"新任务"是写在共享库中的事实，
// 需要广播的只是"别等了，快去看一眼"。
type ClusterSignal struct {
	ID         uint64    `gorm:"primaryKey" json:"id"`
	Seq        int64     `json:"seq"`
	CreateTime time.Time `json:"create_time"`
	ModifyTime time.Time `json:"modify_time"`
}

// SignalRowID ClusterSignal 那唯一一行的主键。
const SignalRowID = 1

// ServerNode 调度中心进程的心跳表：每个 server 副本每 5s upsert 自己那一行，
// 控制台「节点监控」据此跨副本展示 CPU/内存/GC/HTTP 连接。
// node_id 含进程启动纳秒，所以重启即新行，旧行靠 sample_time 判离线而不是被覆盖。
//
// 注意：数值列一律不加 default 标签——上报的 0（空闲进程 CPU 就是 0）必须能落库，
// gorm 对带 default 的字段在 Create 时会跳过零值。
type ServerNode struct {
	ID           uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	NodeId       string    `gorm:"uniqueIndex;size:128" json:"node_id"`
	Hostname     string    `gorm:"size:128" json:"hostname"`
	Address      string    `gorm:"size:64" json:"address"`     // 监听地址
	PublicURL    string    `gorm:"size:256" json:"public_url"` // 对外访问地址
	BootTime     time.Time `json:"boot_time"`
	SampleTime   time.Time `gorm:"index" json:"sample_time"` // 最后心跳，判活用
	CPUPercent   float64   `json:"cpu_percent"`              // 100 = 吃满一个核
	RssMb        float64   `json:"rss_mb"`
	HeapInuseMb  float64   `json:"heap_inuse_mb"`
	Goroutines   int       `json:"goroutines"`
	GcCount      uint32    `json:"gc_count"`
	GcPauseMs    float64   `json:"gc_pause_ms"` // 启动以来累计 GC 暂停
	ConnsActive  int       `json:"conns_active"`
	ConnsIdle    int       `json:"conns_idle"`
	PollInflight int       `json:"poll_inflight"` // worker 长轮询在途数
	ReqInflight  int       `json:"req_inflight"`
	ReqPerSec    float64   `json:"req_per_sec"`
	Samples      string    `gorm:"type:text" json:"-"` // 最近 10 分钟采样序列（JSON）
	CreateTime   time.Time `json:"create_time"`
	ModifyTime   time.Time `json:"modify_time"`
}

type JobInfo struct {
	ID                 uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	AppId              string `gorm:"index:idx_app_enable;size:64" json:"app_id"`
	JobName            string `gorm:"size:128" json:"job_name"`
	JobDescription     string `gorm:"size:256" json:"job_description"`
	Processor          string `gorm:"size:128" json:"processor"`
	ProcessorType      string `gorm:"size:32;default:'BUILT_IN'" json:"processor_type"`   // 兼容协议的 ProcessorType 枚举：BUILT_IN / EXTERNAL（SHELL/PYTHON 导入时已改写）
	ExecuteType        string `gorm:"size:16;default:'STANDALONE'" json:"execute_type"`   // 一期仅 STANDALONE 生效
	TimeExpressionType string `gorm:"size:24;default:'CRON'" json:"time_expression_type"` // CRON / FIXED_RATE / API
	TimeExpression     string `gorm:"size:64" json:"time_expression"`
	JobParams          string `gorm:"type:text" json:"job_params"`
	InstanceParams     string `gorm:"type:text" json:"instance_params"` // 每次触发默认运行时参数
	// 这三列不带 default 标签：0 都是有意义的配置（不重试 / 不限实例数），而 gorm 的 Create
	// 会跳过"带 default 标签且值为零"的列，把 0 交给库里的 DEFAULT 3 —— 显式配置的 0 就再也写
	// 不进库了（而且读回来的结构体里仍是 0，静默不一致）。默认值由 API 层填（见 orUnset）。
	MaxRetryTimes          int       `json:"max_retry_times"`
	InstanceRetryNum       int       `json:"instance_retry_num"`
	Concurrency            int       `gorm:"default:5" json:"concurrency"` // 0 无意义（API 兜底为 5，下限 1）
	InstanceNumLimit       int       `json:"instance_num_limit"`
	SkipWhenRunning        bool      `gorm:"default:false" json:"skip_when_running"`   // 执行中不发起新任务
	DesignatedWorkers      string    `gorm:"size:512" json:"designated_workers"`       // 逗号分隔 ip:port，空=任意
	MaxWorkerCount         int       `gorm:"default:0" json:"max_worker_count"`        // 0=不限
	DispatchStrategy       int       `gorm:"default:1" json:"dispatch_strategy"`       // 1健康优先(默认) 2随机 11指定 21轮询(扩展)
	DispatchStrategyConfig string    `gorm:"size:255" json:"dispatch_strategy_config"` // 仅 SPECIFY 使用
	UpstreamJobId          uint64    `gorm:"index;default:0" json:"upstream_job_id"`   // 依赖触发：上游任务 ID，0=无上游（兼容协议用工作流表达依赖，此为 gojob 扩展）
	LogLevel               string    `gorm:"size:8;default:'INFO'" json:"log_level"`
	Enable                 bool      `gorm:"index:idx_app_enable;default:true" json:"enable"`
	NextTriggerTime        time.Time `gorm:"index" json:"next_trigger_time"`
	LifeCycle              int       `gorm:"default:0" json:"life_cycle"` // 实例保留天数，0=不清理
	CreateTime             time.Time `json:"create_time"`
	ModifyTime             time.Time `json:"modify_time"`
}

type UserInfo struct {
	ID       uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	Username string `gorm:"uniqueIndex;size:64" json:"username"`
	Password string `gorm:"size:128" json:"-"` // bcrypt hash
	// TokenEpoch 控制台会话的吊销基线，语义同 AppInfo.TokenEpoch：
	// 改密/重置密码/禁用/删除账号时 +1，此前签发的会话即刻失效（跨副本一致，因为是库里的列）。
	TokenEpoch int64     `json:"token_epoch"`
	Nickname   string    `gorm:"size:64" json:"nickname"`
	Role       string    `gorm:"size:16;default:'READONLY'" json:"role"` // SUPER_ADMIN / ADMIN / READONLY
	Status     string    `gorm:"size:16;default:'ACTIVE'" json:"status"` // ACTIVE / DISABLED
	AppScopes  string    `gorm:"size:1024" json:"app_scopes"`            // 逗号分隔 app_id；"*"=全部；仅 ADMIN 生效
	CreateTime time.Time `json:"create_time"`
	ModifyTime time.Time `json:"modify_time"`
}

// LoginLog 登录审计：成功与失败都记录，用于排查爆破与异常登录。
type LoginLog struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	Username  string    `gorm:"index;size:64" json:"username"`
	IP        string    `gorm:"size:64" json:"ip"`
	UserAgent string    `gorm:"size:256" json:"user_agent"`
	Success   bool      `gorm:"default:false" json:"success"`
	Reason    string    `gorm:"size:128" json:"reason"` // 失败原因，成功时为空
	LoginTime time.Time `gorm:"index" json:"login_time"`
}

// OperationLog 操作审计：仅记录管理面写操作（POST /openapi/*）。
type OperationLog struct {
	ID         uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	Username   string    `gorm:"index;size:64" json:"username"`
	Role       string    `gorm:"size:16" json:"role"`
	Action     string    `gorm:"index;size:64" json:"action"` // 如 job/create
	AppId      string    `gorm:"index;size:64" json:"app_id"`
	Method     string    `gorm:"size:8" json:"method"`
	Path       string    `gorm:"size:128" json:"path"`
	Params     string    `gorm:"type:text" json:"params"` // 请求体摘要（已脱敏）
	ResultCode int       `json:"result_code"`             // 业务 Resp.code
	Success    bool      `gorm:"default:false" json:"success"`
	IP         string    `gorm:"size:64" json:"ip"`
	CreateTime time.Time `gorm:"index" json:"create_time"`
}

type JobInstance struct {
	ID    uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	JobId uint64 `gorm:"index;uniqueIndex:idx_job_upstream_instance" json:"job_id"`
	// UpstreamInstanceId 记录"由哪个上游实例的成功触发"。(job_id, upstream_instance_id)
	// 唯一索引 = 依赖触发的幂等键：上游回报重试/并发不会重复拉起下游实例。
	// 非依赖触发的实例该列为 NULL，三库的唯一索引都放行 NULL，故定时/手动实例不受约束。
	UpstreamInstanceId *uint64 `gorm:"uniqueIndex:idx_job_upstream_instance" json:"upstream_instance_id"`
	AppId              string  `gorm:"index;size:64" json:"app_id"`
	JobName            string  `gorm:"size:128" json:"job_name"`
	Processor          string  `gorm:"size:128" json:"processor"`
	JobParams          string  `gorm:"type:text" json:"job_params"`
	Status             string  `gorm:"size:16;index" json:"status"`
	// ExecuteType 建单时从 JobInfo 抄下来：决定这个实例是"一实例一任务"还是"一棵任务树"，
	// 以及收敛时按 MAP（任一子任务失败即失败）还是 MAP_REDUCE（reduce 的返回值说了算）。
	// 抄而不 JOIN：任务改过执行类型不该追溯已产生的实例。
	ExecuteType string `gorm:"size:16;default:'STANDALONE'" json:"execute_type"`
	// Result 实例级结果，与兼容协议的 InstanceInfoDO.result 同义：非 MAP_REDUCE 取根任务的返回值，
	// MAP_REDUCE 取 reduce 任务（OMS_LAST_TASK）的返回值。
	Result        string    `gorm:"type:text" json:"result"`
	FixedParams   string    `gorm:"type:text" json:"fixed_params"`  // 工作流上下文，M5 使用
	RunningTimes  int       `gorm:"default:0" json:"running_times"` // 未结束的任务数
	TryTimes      int       `gorm:"default:0" json:"try_times"`
	TotalTaskNum  int       `gorm:"default:0" json:"total_task_num"`
	FailedTaskNum int       `gorm:"default:0" json:"failed_task_num"`
	Source        string    `gorm:"size:16" json:"source"` // CRON / API / DEPENDENCY
	CreateTime    time.Time `gorm:"index" json:"create_time"`
	ModifyTime    time.Time `json:"modify_time"`
}

// InstanceTask 调度执行分离的关键：Server 触发后写入本表，Worker 长轮询拉取。
type InstanceTask struct {
	ID             uint64 `gorm:"primaryKey;autoIncrement" json:"id"`
	JobId          uint64 `gorm:"index" json:"job_id"`
	InstanceId     uint64 `gorm:"index" json:"instance_id"`
	AppId          string `gorm:"index:idx_task_claim,priority:1;size:64" json:"app_id"` // (app_id,status,dispatch_time) 已覆盖旧的 (app_id,status) 前缀索引，不再单独建
	Processor      string `gorm:"size:128" json:"processor"`
	ProcessorType  string `gorm:"size:32;default:'BUILT_IN'" json:"processor_type"` // 建单时从 JobInfo 抄下来，认领时随 TaskDTO 下发
	JobParams      string `gorm:"type:text" json:"job_params"`
	FixedParams    string `gorm:"type:text" json:"fixed_params"` // 运行时参数（runJob instanceParams）
	Status         string `gorm:"index:idx_task_claim,priority:2;index:idx_task_lost,priority:1;index:idx_task_parent_state,priority:2;size:16" json:"status"`
	WorkerId       string `gorm:"size:64" json:"worker_id"`        // 认领该任务 worker instance_id
	TargetWorkerId string `gorm:"size:64" json:"target_worker_id"` // 分发策略预选的机器，空=任意在线节点可认领
	// —— 多任务执行类型（BROADCAST/MAP/MAP_REDUCE）：一个实例是一棵树，根任务 parent_task_id=0 ——
	// 兼容协议把这些行放在 TaskTracker 节点的本地 task_info 表里、server 零落库；gojob 是 pull
	// 模型且 worker 之间不通信，子任务只能经 server 中转落库。附带好处：实例结束后子任务仍可查。
	ParentTaskId uint64 `gorm:"index:idx_task_parent_state,priority:1;default:0" json:"parent_task_id"`
	// MapBatch/MapAttempt 一起构成派生幂等键：(父任务, 第几次尝试, 第几批 map)。
	// 必须带上尝试次数——父任务重跑时它会用同样的批次号重新 map 一遍，不带尝试次数的话
	// 上一批里已成功的行会被误判成"这批建过了"，新任务一条都不建，实例在一堆旧行上静默收敛。
	MapBatch    int    `gorm:"default:0" json:"map_batch"`
	MapAttempt  int    `gorm:"default:0" json:"map_attempt"`
	ExecuteType string `gorm:"size:16;default:'STANDALONE'" json:"execute_type"`
	TaskKey     string `gorm:"size:64" json:"task_key"`   // 层次 ID："0" / "0.3" / reduce 固定 "9999"
	TaskName    string `gorm:"size:256" json:"task_name"` // map() 的第二参；内部任务用 OMS_* 保留名
	// SubTaskContent 子任务负载。兼容协议用 worker 侧可插拔序列化器（默认 JDK 序列化），
	// gojob 固定 JSON：我们的 worker 池是 Go/Java 混布的，JDK 序列化的字节 Go 侧读不了。
	SubTaskContent  string     `gorm:"type:text" json:"sub_task_content"`
	StopRequestTime *time.Time `json:"stop_request_time"` // 停止意图，NULL=未请求；落库所以 server 重启不丢指令
	Attempts        int        `gorm:"default:1" json:"attempts"`
	MaxAttempts     int        `gorm:"default:1" json:"max_attempts"`
	LogLevel        string     `gorm:"size:8;default:'INFO'" json:"log_level"`
	Result          string     `gorm:"type:text" json:"result"`
	// DispatchTime 既做"到点才可认领"的范围过滤，又是认领顺序的排序列，所以必须进复合索引末位（免 filesort）
	DispatchTime time.Time `gorm:"index;index:idx_task_claim,priority:3;index:idx_task_lost,priority:2" json:"dispatch_time"`
	CreateTime   time.Time `json:"create_time"`
	ModifyTime   time.Time `json:"modify_time"`
}

// InstanceTaskLog 任务执行日志。日志通道为 at-least-once 传输，
// (task_id, seq) 唯一索引做幂等去重，重复上报直接忽略。
type InstanceTaskLog struct {
	ID         uint64    `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskId     uint64    `gorm:"uniqueIndex:uk_task_seq" json:"task_id"`
	InstanceId uint64    `gorm:"index" json:"instance_id"`
	Seq        uint64    `gorm:"uniqueIndex:uk_task_seq" json:"seq"`
	Level      string    `gorm:"size:8" json:"level"`
	LogTime    int64     `gorm:"index" json:"log_time"` // 毫秒时间戳
	Content    string    `gorm:"type:text" json:"content"`
	CreateTime time.Time `json:"create_time"`
}
