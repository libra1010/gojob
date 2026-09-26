-- gojob 数据库初始化脚本 (MySQL 8.0+)
-- 用法: mysql -u root -p < scripts/ddl/mysql.sql
-- 说明:
--   * 表结构与 GORM AutoMigrate 完全一致；server 启动时也会自动建表，
--     本脚本用于"模拟安装"或由 DBA 预先建库的场景。
--   * 管理员账号无需 SQL 初始化：首次启动由 server 按 GOJOB_ADMIN_USER /
--     GOJOB_ADMIN_PASSWORD（或 -admin-user/-admin-password）自动播种（bcrypt）。
--   * App（项目）通过控制台或 /openapi/app/create 创建，不在此预置。

CREATE DATABASE IF NOT EXISTS gojob DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE gojob;

-- 应用（兼容协议语义中的 namespace/app）
CREATE TABLE IF NOT EXISTS app_infos (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  app_id      VARCHAR(64)  NOT NULL,
  token_epoch BIGINT NOT NULL DEFAULT 0,  -- worker/OpenAPI 令牌的吊销基线，见 README「Server 集群」
  app_name    VARCHAR(128) NOT NULL DEFAULT '',
  password    VARCHAR(128) NOT NULL DEFAULT '',  -- bcrypt(明文)，不再存明文
  create_time DATETIME(3)  NULL,
  modify_time DATETIME(3)  NULL,
  UNIQUE KEY uni_app_infos_app_id (app_id)
) ENGINE = InnoDB;

-- 项目调度归属租约（多副本）：同一个 app 同一时刻只有一个副本在调度。
-- 接管时 epoch 递增，所以"旧主进程还活着"也写不进库（条件 UPDATE 只认当前 holder+epoch）。
CREATE TABLE IF NOT EXISTS app_leases (
  id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  app_id       VARCHAR(64)  NOT NULL,
  holder       VARCHAR(128) NOT NULL DEFAULT '',  -- 持有者副本标识 hostname-pid-startNanos
  epoch        BIGINT       NOT NULL DEFAULT 0,   -- 只在接管时 +1
  lease_expire DATETIME(3)  NULL,                 -- 超过此刻即可被别的副本抢占
  renew_count  BIGINT       NOT NULL DEFAULT 0,
  create_time  DATETIME(3)  NULL,
  modify_time  DATETIME(3)  NULL,
  UNIQUE KEY idx_app_leases_app_id (app_id),
  KEY idx_app_leases_holder (holder),
  KEY idx_app_leases_lease_expire (lease_expire)
) ENGINE = InnoDB;

-- Worker 在线节点表：心跳落库，所有副本共用同一份候选集（分发策略与节点监控都读它）
CREATE TABLE IF NOT EXISTS worker_nodes (
  id             BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  app_id         VARCHAR(64)  NOT NULL,
  instance_id    VARCHAR(64)  NOT NULL,
  address        VARCHAR(128) NOT NULL DEFAULT '',
  cpu_load       DOUBLE       NOT NULL DEFAULT 0,
  cpu_processors INT          NOT NULL DEFAULT 0,
  memory_used    DOUBLE       NOT NULL DEFAULT 0,
  memory_max     DOUBLE       NOT NULL DEFAULT 0,
  tag            VARCHAR(128) NOT NULL DEFAULT '',
  version        VARCHAR(64)  NOT NULL DEFAULT '',
  sdk_type       VARCHAR(32)  NOT NULL DEFAULT '',
  cpu_percent    DOUBLE       NOT NULL DEFAULT 0,
  gc_count       BIGINT       NOT NULL DEFAULT 0,
  gc_pause_ms    DOUBLE       NOT NULL DEFAULT 0,
  rss_mb         DOUBLE       NOT NULL DEFAULT 0,  -- 0 = 平台取不到，页面显示"—"
  threads        INT          NOT NULL DEFAULT 0,
  open_fds       INT          NOT NULL DEFAULT 0,
  running_tasks  INT          NOT NULL DEFAULT 0,
  last_heartbeat DATETIME(3)  NULL,
  create_time    DATETIME(3)  NULL,
  modify_time    DATETIME(3)  NULL,
  UNIQUE KEY uk_worker_node (app_id, instance_id),
  KEY idx_worker_nodes_last_heartbeat (last_heartbeat)
) ENGINE = InnoDB;

-- 跨副本唤醒信号：单行计数器。入队/重排/停止意图 seq+1，各副本 1s 轮询到变化即唤醒本机长轮询
CREATE TABLE IF NOT EXISTS cluster_signals (
  id          BIGINT UNSIGNED NOT NULL PRIMARY KEY,  -- 恒为 1
  seq         BIGINT      NOT NULL DEFAULT 0,
  create_time DATETIME(3) NULL,
  modify_time DATETIME(3) NULL
) ENGINE = InnoDB;

-- 已消费 jti 表：① 登出黑名单（无状态会话票无法凭内存作废单张）② 一次性注册 challenge 的消费记录
-- （主键冲突即"已用过"，跨副本生效）。随对应票过期，由维护循环清理
CREATE TABLE IF NOT EXISTS revoked_tokens (
  jti       VARCHAR(64) NOT NULL PRIMARY KEY,
  purpose   VARCHAR(16) NOT NULL DEFAULT '',
  subject   VARCHAR(64) NOT NULL DEFAULT '',
  expire_at DATETIME(3) NULL,
  KEY idx_revoked_tokens_expire_at (expire_at)
) ENGINE = InnoDB;

-- 调度中心节点心跳：每个 server 副本每 5s upsert 自己一行（node_id 含进程启动纳秒），
-- 控制台「系统管理 → 节点监控」跨副本展示 CPU/内存/GC/HTTP 连接。
CREATE TABLE IF NOT EXISTS server_nodes (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  node_id       VARCHAR(128) NOT NULL,
  hostname      VARCHAR(128) NOT NULL DEFAULT '',
  address       VARCHAR(64)  NOT NULL DEFAULT '',  -- 监听地址
  public_url    VARCHAR(256) NOT NULL DEFAULT '',  -- 对外访问地址
  boot_time     DATETIME(3)  NULL,
  sample_time   DATETIME(3)  NULL,                 -- 最后心跳，超 15s 判离线
  cpu_percent   DOUBLE       NOT NULL DEFAULT 0,   -- 100 = 吃满一个核
  rss_mb        DOUBLE       NOT NULL DEFAULT 0,   -- 0 = 平台取不到
  heap_inuse_mb DOUBLE       NOT NULL DEFAULT 0,
  goroutines    INT          NOT NULL DEFAULT 0,
  gc_count      BIGINT       NOT NULL DEFAULT 0,
  gc_pause_ms   DOUBLE       NOT NULL DEFAULT 0,   -- 启动以来累计 GC 暂停
  conns_active  INT          NOT NULL DEFAULT 0,
  conns_idle    INT          NOT NULL DEFAULT 0,
  poll_inflight INT          NOT NULL DEFAULT 0,   -- worker 长轮询在途数
  req_inflight  INT          NOT NULL DEFAULT 0,
  req_per_sec   DOUBLE       NOT NULL DEFAULT 0,
  samples       TEXT         NULL,                 -- 最近 10 分钟采样序列（JSON）
  create_time   DATETIME(3)  NULL,
  modify_time   DATETIME(3)  NULL,
  UNIQUE KEY uni_server_nodes_node_id (node_id),
  KEY idx_server_nodes_sample_time (sample_time)
) ENGINE = InnoDB;

-- 控制台登录用户
CREATE TABLE IF NOT EXISTS user_infos (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  username    VARCHAR(64)  NOT NULL,
  token_epoch BIGINT NOT NULL DEFAULT 0,  -- 控制台会话的吊销基线
  password    VARCHAR(128) NOT NULL DEFAULT '',  -- bcrypt hash
  nickname    VARCHAR(64)  NOT NULL DEFAULT '',
  role        VARCHAR(16)  NOT NULL DEFAULT 'READONLY', -- SUPER_ADMIN / ADMIN / READONLY
  status      VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE',   -- ACTIVE / DISABLED
  app_scopes  VARCHAR(1024) NOT NULL DEFAULT '',        -- 逗号分隔 app_id；'*'=全部；仅 ADMIN 生效
  create_time DATETIME(3)  NULL,
  modify_time DATETIME(3)  NULL,
  UNIQUE KEY uni_user_infos_username (username)
) ENGINE = InnoDB;

-- 登录审计
CREATE TABLE IF NOT EXISTS login_logs (
  id         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  username   VARCHAR(64)  NOT NULL DEFAULT '',
  ip         VARCHAR(64)  NOT NULL DEFAULT '',
  user_agent VARCHAR(256) NOT NULL DEFAULT '',
  success    TINYINT(1)   NOT NULL DEFAULT 0,
  reason     VARCHAR(128) NOT NULL DEFAULT '',
  login_time DATETIME(3)  NULL,
  KEY idx_login_logs_username (username),
  KEY idx_login_logs_login_time (login_time)
) ENGINE = InnoDB;

-- 操作审计（管理面写操作）
CREATE TABLE IF NOT EXISTS operation_logs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  username    VARCHAR(64)  NOT NULL DEFAULT '',
  role        VARCHAR(16)  NOT NULL DEFAULT '',
  action      VARCHAR(64)  NOT NULL DEFAULT '',
  app_id      VARCHAR(64)  NOT NULL DEFAULT '',
  method      VARCHAR(8)   NOT NULL DEFAULT '',
  path        VARCHAR(128) NOT NULL DEFAULT '',
  params      TEXT         NULL,
  result_code INT          NOT NULL DEFAULT 0,
  success     TINYINT(1)   NOT NULL DEFAULT 0,
  ip          VARCHAR(64)  NOT NULL DEFAULT '',
  create_time DATETIME(3)  NULL,
  KEY idx_operation_logs_username (username),
  KEY idx_operation_logs_action (action),
  KEY idx_operation_logs_app_id (app_id),
  KEY idx_operation_logs_create_time (create_time)
) ENGINE = InnoDB;

-- 任务定义
CREATE TABLE IF NOT EXISTS job_infos (
  id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  app_id             VARCHAR(64)  NOT NULL DEFAULT '',
  job_name           VARCHAR(128) NOT NULL DEFAULT '',
  processor          VARCHAR(128) NOT NULL DEFAULT '',
  job_description    VARCHAR(256) NOT NULL DEFAULT '',
  processor_type     VARCHAR(32)  NOT NULL DEFAULT 'BUILT_IN',   -- 兼容协议 ProcessorType：BUILT_IN/EXTERNAL（SHELL/PYTHON 已废弃，导入时改写）
  execute_type       VARCHAR(16)  NOT NULL DEFAULT 'STANDALONE',
  time_expression_type VARCHAR(24) NOT NULL DEFAULT 'CRON',       -- CRON/FIXED_RATE/FIXED_DELAY/DAILY_TIME_INTERVAL/API/WORKFLOW/DEPENDENCY(依赖触发,gojob 扩展)
  time_expression    VARCHAR(64)  NOT NULL DEFAULT '',
  job_params         TEXT,
  instance_params    TEXT,                                     -- 运行时固定参数（兼容协议 instanceParams）
  max_retry_times    INT          NOT NULL DEFAULT 3,
  instance_retry_num INT          NOT NULL DEFAULT 3,
  concurrency        INT          NOT NULL DEFAULT 5,
  instance_num_limit INT          NOT NULL DEFAULT 3,
  skip_when_running  TINYINT(1)   NOT NULL DEFAULT 0,   -- 执行中不再发起新实例
  designated_workers VARCHAR(512) NOT NULL DEFAULT '',  -- 指定执行机器（逗号分隔 address）
  max_worker_count   INT          NOT NULL DEFAULT 0,   -- 参与执行机器数上限，0=不限
  dispatch_strategy  INT          NOT NULL DEFAULT 1,   -- 1健康优先(默认)/2随机/11指定/21轮询(gojob 扩展)
  dispatch_strategy_config VARCHAR(255) NOT NULL DEFAULT '', -- 仅 SPECIFY：tagEquals:/tagIn:/逗号分隔 IP·tag
  upstream_job_id    BIGINT UNSIGNED NOT NULL DEFAULT 0, -- 依赖触发(gojob 扩展)：上游任务 ID，0=无上游
  log_level          VARCHAR(8)   NOT NULL DEFAULT 'INFO',
  enable             TINYINT(1)   NOT NULL DEFAULT 1,
  next_trigger_time  DATETIME(3)  NULL,
  life_cycle         INT          NOT NULL DEFAULT 0,   -- 实例保留天数,0=不清理
  create_time        DATETIME(3)  NULL,
  modify_time        DATETIME(3)  NULL,
  KEY idx_app_enable (app_id, enable),
  KEY idx_job_infos_next_trigger_time (next_trigger_time),
  KEY idx_job_infos_upstream_job_id (upstream_job_id)
) ENGINE = InnoDB;

-- 任务实例（一次触发 = 一个实例）
CREATE TABLE IF NOT EXISTS job_instances (
  id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  job_id          BIGINT UNSIGNED NOT NULL DEFAULT 0,
  -- 依赖触发：由哪个上游实例拉起；(job_id, upstream_instance_id) 唯一 = 幂等键，NULL=非依赖触发
  upstream_instance_id BIGINT UNSIGNED NULL,
  app_id          VARCHAR(64)  NOT NULL DEFAULT '',
  job_name        VARCHAR(128) NOT NULL DEFAULT '',
  processor       VARCHAR(128) NOT NULL DEFAULT '',
  job_params      TEXT,
  status          VARCHAR(16)  NOT NULL DEFAULT '',  -- WAITING/RUNNING/STOPPING/STOPPED/SUCCEED/FAILED
  fixed_params    TEXT,                              -- runJob instanceParams
  execute_type    VARCHAR(16)  NOT NULL DEFAULT 'STANDALONE', -- 建单时从 job_infos 抄：一实例一任务还是一棵任务树
  result          TEXT,                              -- 实例级结果（兼容协议 InstanceInfoDO.result）：reduce 或根任务的返回值
  running_times   INT          NOT NULL DEFAULT 0,   -- 未结束任务数
  try_times       INT          NOT NULL DEFAULT 0,
  total_task_num  INT          NOT NULL DEFAULT 0,
  failed_task_num INT          NOT NULL DEFAULT 0,
  source          VARCHAR(16)  NOT NULL DEFAULT '',  -- CRON/API/DEPENDENCY
  create_time     DATETIME(3)  NULL,
  modify_time     DATETIME(3)  NULL,
  KEY idx_job_instances_job_id (job_id),
  KEY idx_job_instances_app_id (app_id),
  KEY idx_job_instances_status (status),
  KEY idx_job_instances_create_time (create_time),
  UNIQUE KEY idx_job_upstream_instance (job_id, upstream_instance_id)
) ENGINE = InnoDB;

-- 调度执行分离的核心队列：Server 写入，Worker 长轮询拉取
CREATE TABLE IF NOT EXISTS instance_tasks (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  job_id        BIGINT UNSIGNED NOT NULL DEFAULT 0,
  instance_id   BIGINT UNSIGNED NOT NULL DEFAULT 0,
  app_id        VARCHAR(64)  NOT NULL DEFAULT '',
  processor     VARCHAR(128) NOT NULL DEFAULT '',
  processor_type  VARCHAR(32) NOT NULL DEFAULT 'BUILT_IN', -- 建单时从 job_infos 抄下来，随 TaskDTO 下发给 worker
  job_params    TEXT,
  fixed_params  TEXT,
  log_level     VARCHAR(8)   NOT NULL DEFAULT 'INFO',
  status        VARCHAR(16)  NOT NULL DEFAULT '',  -- WAITING/RUNNING/STOPPING/STOPPED/SUCCEED/FAILED
  worker_id     VARCHAR(64)  NOT NULL DEFAULT '',  -- 认领该任务的 worker
  target_worker_id VARCHAR(64) NOT NULL DEFAULT '', -- 分发策略预选机器，空=任意在线节点可认领
  -- 多任务执行类型（BROADCAST/MAP/MAP_REDUCE）：一个实例是一棵树，根任务 parent_task_id=0。
  -- 兼容协议把这些行放在 TaskTracker 节点的本地 task_info 表里，gojob 落库（pull 模型下 worker 不互访）。
  parent_task_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  map_batch   INT          NOT NULL DEFAULT 0,      -- 父任务第几次调 map
  map_attempt INT          NOT NULL DEFAULT 0,      -- 与 (parent_task_id, map_batch) 一起构成派生幂等键
  execute_type VARCHAR(16) NOT NULL DEFAULT 'STANDALONE', -- 从 job_instances 抄，随 TaskDTO 下发（worker 据此判断能否 map）
  task_key    VARCHAR(64)  NOT NULL DEFAULT '',     -- 层次 ID："0" / "0.3" / reduce 固定 "9999"
  task_name   VARCHAR(256) NOT NULL DEFAULT '',     -- map() 的第二参；内部任务用 OMS_* 保留名
  sub_task_content TEXT,                            -- 子任务负载（JSON，跨 Go/Java worker 必须可互读）
  stop_request_time DATETIME(3) NULL,                -- 停止意图（NULL=未请求），落库故 server 重启不丢
  attempts      INT          NOT NULL DEFAULT 1,
  max_attempts  INT          NOT NULL DEFAULT 1,
  result        TEXT,
  dispatch_time DATETIME(3)  NULL,                 -- 进入待拉取队列时间
  create_time   DATETIME(3)  NULL,
  modify_time   DATETIME(3)  NULL,
  KEY idx_instance_tasks_job_id (job_id),
  KEY idx_instance_tasks_instance_id (instance_id),
  KEY idx_instance_tasks_dispatch_time (dispatch_time),
  KEY idx_task_claim (app_id, status, dispatch_time),  -- 热认领：等值+范围+排序一条索引吃完，免 filesort
  KEY idx_task_lost (status, dispatch_time),           -- 失联回收巡检用
  KEY idx_task_parent_state (parent_task_id, status)   -- 派生幂等判定与父任务重派时的后代级联作废
) ENGINE = InnoDB;

-- Worker 上报的任务执行日志：uk(task_id, seq) 保证 at-least-once 重发不产生重复
CREATE TABLE IF NOT EXISTS instance_task_logs (
  id          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  task_id     BIGINT UNSIGNED NOT NULL DEFAULT 0,
  instance_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  seq         BIGINT UNSIGNED NOT NULL DEFAULT 0,   -- 任务内单调递增序号（worker 侧分配）
  level       VARCHAR(8)   NOT NULL DEFAULT '',
  log_time    BIGINT       NOT NULL DEFAULT 0,      -- epoch 毫秒
  content     TEXT,
  create_time DATETIME(3)  NULL,
  UNIQUE KEY uk_task_seq (task_id, seq),
  KEY idx_instance_task_logs_instance_id (instance_id),
  KEY idx_instance_task_logs_log_time (log_time)
) ENGINE = InnoDB;
