-- gojob 数据库初始化脚本 (PostgreSQL 14+)
-- 用法: psql -U postgres -f scripts/ddl/postgres.sql
-- 说明:
--   * 表结构与 GORM AutoMigrate 完全一致；server 启动时也会自动建表，
--     本脚本用于"模拟安装"或由 DBA 预先建库的场景。
--   * DSN 示例: postgres://gojob:gojob123@127.0.0.1:5432/gojob?sslmode=disable
--   * 管理员账号无需 SQL 初始化：首次启动由 server 按 GOJOB_ADMIN_USER /
--     GOJOB_ADMIN_PASSWORD（或 -admin-user/-admin-password）自动播种（bcrypt）。

CREATE DATABASE gojob;

\connect gojob

-- 应用（PowerJob 语义中的 namespace/app）
CREATE TABLE IF NOT EXISTS app_infos (
  id          BIGSERIAL PRIMARY KEY,
  app_id      VARCHAR(64)  NOT NULL,
  token_epoch BIGINT NOT NULL DEFAULT 0,  -- worker/OpenAPI 令牌的吊销基线，见 README「Server 集群」
  app_name    VARCHAR(128) NOT NULL DEFAULT '',
  password    VARCHAR(128) NOT NULL DEFAULT '',  -- bcrypt(明文)，不再存明文
  create_time TIMESTAMPTZ(3) NULL,
  modify_time TIMESTAMPTZ(3) NULL,
  CONSTRAINT uni_app_infos_app_id UNIQUE (app_id)
);

-- 调度中心节点心跳：每个 server 副本每 5s upsert 自己一行（node_id 含进程启动纳秒），
-- 控制台「系统管理 → 节点监控」跨副本展示 CPU/内存/GC/HTTP 连接。
CREATE TABLE IF NOT EXISTS server_nodes (
  id            BIGSERIAL PRIMARY KEY,
  node_id       VARCHAR(128) NOT NULL,
  hostname      VARCHAR(128) NOT NULL DEFAULT '',
  address       VARCHAR(64)  NOT NULL DEFAULT '',
  public_url    VARCHAR(256) NOT NULL DEFAULT '',
  boot_time     TIMESTAMPTZ(3) NULL,
  sample_time   TIMESTAMPTZ(3) NULL,  -- 最后心跳，超 15s 判离线
  cpu_percent   DOUBLE PRECISION NOT NULL DEFAULT 0,  -- 100 = 吃满一个核
  rss_mb        DOUBLE PRECISION NOT NULL DEFAULT 0,  -- 0 = 平台取不到
  heap_inuse_mb DOUBLE PRECISION NOT NULL DEFAULT 0,
  goroutines    INTEGER NOT NULL DEFAULT 0,
  gc_count      BIGINT  NOT NULL DEFAULT 0,
  gc_pause_ms   DOUBLE PRECISION NOT NULL DEFAULT 0,
  conns_active  INTEGER NOT NULL DEFAULT 0,
  conns_idle    INTEGER NOT NULL DEFAULT 0,
  poll_inflight INTEGER NOT NULL DEFAULT 0,  -- worker 长轮询在途数
  req_inflight  INTEGER NOT NULL DEFAULT 0,
  req_per_sec   DOUBLE PRECISION NOT NULL DEFAULT 0,
  samples       TEXT NULL,  -- 最近 10 分钟采样序列（JSON）
  create_time   TIMESTAMPTZ(3) NULL,
  modify_time   TIMESTAMPTZ(3) NULL,
  CONSTRAINT uni_server_nodes_node_id UNIQUE (node_id)
);
CREATE INDEX IF NOT EXISTS idx_server_nodes_sample_time ON server_nodes (sample_time);

-- 项目调度归属租约（多副本）：同一个 app 同一时刻只有一个副本在调度。
-- 接管时 epoch 递增，所以"旧主进程还活着"也写不进库（条件 UPDATE 只认当前 holder+epoch）。
CREATE TABLE IF NOT EXISTS app_leases (
  id           BIGSERIAL PRIMARY KEY,
  app_id       VARCHAR(64)  NOT NULL,
  holder       VARCHAR(128) NOT NULL DEFAULT '',  -- 持有者副本标识 hostname-pid-startNanos
  epoch        BIGINT       NOT NULL DEFAULT 0,   -- 只在接管时 +1
  lease_expire TIMESTAMPTZ(3) NULL,               -- 超过此刻即可被别的副本抢占
  renew_count  BIGINT       NOT NULL DEFAULT 0,
  create_time  TIMESTAMPTZ(3) NULL,
  modify_time  TIMESTAMPTZ(3) NULL,
  CONSTRAINT idx_app_leases_app_id UNIQUE (app_id)
);
CREATE INDEX IF NOT EXISTS idx_app_leases_holder ON app_leases (holder);
CREATE INDEX IF NOT EXISTS idx_app_leases_lease_expire ON app_leases (lease_expire);

-- Worker 在线节点表：心跳落库，所有副本共用同一份候选集（分发策略与节点监控都读它）
CREATE TABLE IF NOT EXISTS worker_nodes (
  id             BIGSERIAL PRIMARY KEY,
  app_id         VARCHAR(64)  NOT NULL,
  instance_id    VARCHAR(64)  NOT NULL,
  address        VARCHAR(128) NOT NULL DEFAULT '',
  cpu_load       DOUBLE PRECISION NOT NULL DEFAULT 0,
  cpu_processors INTEGER NOT NULL DEFAULT 0,
  memory_used    DOUBLE PRECISION NOT NULL DEFAULT 0,
  memory_max     DOUBLE PRECISION NOT NULL DEFAULT 0,
  tag            VARCHAR(128) NOT NULL DEFAULT '',
  version        VARCHAR(64)  NOT NULL DEFAULT '',
  sdk_type       VARCHAR(32)  NOT NULL DEFAULT '',
  cpu_percent    DOUBLE PRECISION NOT NULL DEFAULT 0,
  gc_count       BIGINT NOT NULL DEFAULT 0,
  gc_pause_ms    DOUBLE PRECISION NOT NULL DEFAULT 0,
  rss_mb         DOUBLE PRECISION NOT NULL DEFAULT 0,  -- 0 = 平台取不到，页面显示"—"
  threads        INTEGER NOT NULL DEFAULT 0,
  open_fds       INTEGER NOT NULL DEFAULT 0,
  running_tasks  INTEGER NOT NULL DEFAULT 0,
  last_heartbeat TIMESTAMPTZ(3) NULL,
  create_time    TIMESTAMPTZ(3) NULL,
  modify_time    TIMESTAMPTZ(3) NULL,
  CONSTRAINT uk_worker_node UNIQUE (app_id, instance_id)
);
CREATE INDEX IF NOT EXISTS idx_worker_nodes_last_heartbeat ON worker_nodes (last_heartbeat);

-- 跨副本唤醒信号：单行计数器。入队/重排/停止意图 seq+1，各副本 1s 轮询到变化即唤醒本机长轮询
CREATE TABLE IF NOT EXISTS cluster_signals (
  id          BIGINT PRIMARY KEY,  -- 恒为 1
  seq         BIGINT      NOT NULL DEFAULT 0,
  create_time TIMESTAMPTZ(3) NULL,
  modify_time TIMESTAMPTZ(3) NULL
);

-- 登出黑名单：无状态会话票无法凭内存作废单张，退出登录落这张小表（随票过期，由维护循环清理）
CREATE TABLE IF NOT EXISTS revoked_tokens (
  jti       VARCHAR(64) NOT NULL PRIMARY KEY,
  purpose   VARCHAR(16) NOT NULL DEFAULT '',
  subject   VARCHAR(64) NOT NULL DEFAULT '',
  expire_at TIMESTAMPTZ(3) NULL
);
CREATE INDEX IF NOT EXISTS idx_revoked_tokens_expire_at ON revoked_tokens (expire_at);

-- 控制台登录用户
CREATE TABLE IF NOT EXISTS user_infos (
  id          BIGSERIAL PRIMARY KEY,
  username    VARCHAR(64)  NOT NULL,
  token_epoch BIGINT NOT NULL DEFAULT 0,  -- 控制台会话的吊销基线
  password    VARCHAR(128) NOT NULL DEFAULT '',  -- bcrypt hash
  nickname    VARCHAR(64)  NOT NULL DEFAULT '',
  role        VARCHAR(16)  NOT NULL DEFAULT 'READONLY', -- SUPER_ADMIN / ADMIN / READONLY
  status      VARCHAR(16)  NOT NULL DEFAULT 'ACTIVE',   -- ACTIVE / DISABLED
  app_scopes  VARCHAR(1024) NOT NULL DEFAULT '',        -- 逗号分隔 app_id；'*'=全部；仅 ADMIN 生效
  create_time TIMESTAMPTZ(3) NULL,
  modify_time TIMESTAMPTZ(3) NULL,
  CONSTRAINT uni_user_infos_username UNIQUE (username)
);

-- 任务定义
CREATE TABLE IF NOT EXISTS job_infos (
  id                 BIGSERIAL PRIMARY KEY,
  app_id             VARCHAR(64)  NOT NULL DEFAULT '',
  job_name           VARCHAR(128) NOT NULL DEFAULT '',
  processor          VARCHAR(128) NOT NULL DEFAULT '',
  job_description    VARCHAR(256) NOT NULL DEFAULT '',
  processor_type     VARCHAR(32)  NOT NULL DEFAULT 'BUILT_IN',   -- PowerJob ProcessorType：BUILT_IN/EXTERNAL（SHELL/PYTHON 已废弃，导入时改写）
  execute_type       VARCHAR(16)  NOT NULL DEFAULT 'STANDALONE',
  time_expression_type VARCHAR(24) NOT NULL DEFAULT 'CRON',      -- CRON/FIXED_RATE/FIXED_DELAY/DAILY_TIME_INTERVAL/API/WORKFLOW/DEPENDENCY(依赖触发,gojob 扩展)
  time_expression    VARCHAR(64)  NOT NULL DEFAULT '',
  job_params         TEXT,
  instance_params    TEXT,                                     -- 运行时固定参数（PowerJob instanceParams）
  max_retry_times    INT          NOT NULL DEFAULT 3,
  instance_retry_num INT          NOT NULL DEFAULT 3,
  concurrency        INT          NOT NULL DEFAULT 5,
  instance_num_limit INT          NOT NULL DEFAULT 3,
  skip_when_running  BOOLEAN      NOT NULL DEFAULT FALSE,  -- 执行中不再发起新实例
  designated_workers VARCHAR(512) NOT NULL DEFAULT '',  -- 指定执行机器（逗号分隔 address）
  max_worker_count   INT          NOT NULL DEFAULT 0,   -- 参与执行机器数上限，0=不限
  dispatch_strategy  INT          NOT NULL DEFAULT 1,   -- 1健康优先(默认)/2随机/11指定/21轮询(gojob 扩展)
  dispatch_strategy_config VARCHAR(255) NOT NULL DEFAULT '', -- 仅 SPECIFY：tagEquals:/tagIn:/逗号分隔 IP·tag
  upstream_job_id    BIGINT       NOT NULL DEFAULT 0,   -- 依赖触发(gojob 扩展)：上游任务 ID，0=无上游
  log_level          VARCHAR(8)   NOT NULL DEFAULT 'INFO',
  enable             BOOLEAN      NOT NULL DEFAULT TRUE,
  next_trigger_time  TIMESTAMPTZ(3) NULL,
  life_cycle         INT          NOT NULL DEFAULT 0,      -- 实例保留天数,0=不清理
  create_time        TIMESTAMPTZ(3) NULL,
  modify_time        TIMESTAMPTZ(3) NULL
);
CREATE INDEX IF NOT EXISTS idx_app_enable ON job_infos (app_id, enable);
CREATE INDEX IF NOT EXISTS idx_job_infos_next_trigger_time ON job_infos (next_trigger_time);
CREATE INDEX IF NOT EXISTS idx_job_infos_upstream_job_id ON job_infos (upstream_job_id);

-- 任务实例（一次触发 = 一个实例）
CREATE TABLE IF NOT EXISTS job_instances (
  id              BIGSERIAL PRIMARY KEY,
  job_id          BIGINT       NOT NULL DEFAULT 0,
  -- 依赖触发：由哪个上游实例拉起；(job_id, upstream_instance_id) 唯一 = 幂等键，NULL=非依赖触发
  upstream_instance_id BIGINT  NULL,
  app_id          VARCHAR(64)  NOT NULL DEFAULT '',
  job_name        VARCHAR(128) NOT NULL DEFAULT '',
  processor       VARCHAR(128) NOT NULL DEFAULT '',
  job_params      TEXT,
  status          VARCHAR(16)  NOT NULL DEFAULT '',  -- WAITING/RUNNING/STOPPING/STOPPED/SUCCEED/FAILED
  fixed_params    TEXT,                              -- runJob instanceParams
  execute_type    VARCHAR(16)  NOT NULL DEFAULT 'STANDALONE', -- 建单时从 job_infos 抄：一实例一任务还是一棵任务树
  result          TEXT,                              -- 实例级结果（PowerJob InstanceInfoDO.result）：reduce 或根任务的返回值
  running_times   INT          NOT NULL DEFAULT 0,   -- 未结束任务数
  try_times       INT          NOT NULL DEFAULT 0,
  total_task_num  INT          NOT NULL DEFAULT 0,
  failed_task_num INT          NOT NULL DEFAULT 0,
  source          VARCHAR(16)  NOT NULL DEFAULT '',  -- CRON/FIXED_RATE/FIXED_DELAY/DAILY_TIME_INTERVAL/API/WORKFLOW/DEPENDENCY
  create_time     TIMESTAMPTZ(3) NULL,
  modify_time     TIMESTAMPTZ(3) NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_job_upstream_instance ON job_instances (job_id, upstream_instance_id);
CREATE INDEX IF NOT EXISTS idx_job_instances_job_id ON job_instances (job_id);
CREATE INDEX IF NOT EXISTS idx_job_instances_app_id ON job_instances (app_id);
CREATE INDEX IF NOT EXISTS idx_job_instances_status ON job_instances (status);
CREATE INDEX IF NOT EXISTS idx_job_instances_create_time ON job_instances (create_time);

-- 调度执行分离的核心队列：Server 写入，Worker 长轮询拉取
CREATE TABLE IF NOT EXISTS instance_tasks (
  id            BIGSERIAL PRIMARY KEY,
  job_id        BIGINT       NOT NULL DEFAULT 0,
  instance_id   BIGINT       NOT NULL DEFAULT 0,
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
  -- PowerJob 把这些行放在 TaskTracker 节点的本地 task_info 表里，gojob 落库（pull 模型下 worker 不互访）。
  parent_task_id BIGINT       NOT NULL DEFAULT 0,
  map_batch   INT          NOT NULL DEFAULT 0,      -- 父任务第几次调 map
  map_attempt INT          NOT NULL DEFAULT 0,      -- 与 (parent_task_id, map_batch) 一起构成派生幂等键
  execute_type VARCHAR(16) NOT NULL DEFAULT 'STANDALONE', -- 从 job_instances 抄，随 TaskDTO 下发
  task_key    VARCHAR(64)  NOT NULL DEFAULT '',     -- 层次 ID："0" / "0.3" / reduce 固定 "9999"
  task_name   VARCHAR(256) NOT NULL DEFAULT '',     -- map() 的第二参；内部任务用 OMS_* 保留名
  sub_task_content TEXT,                            -- 子任务负载（JSON，跨 Go/Java worker 必须可互读）
  stop_request_time TIMESTAMPTZ(3) NULL,              -- 停止意图（NULL=未请求），落库故 server 重启不丢
  attempts      INT          NOT NULL DEFAULT 1,
  max_attempts  INT          NOT NULL DEFAULT 1,
  result        TEXT,
  dispatch_time TIMESTAMPTZ(3) NULL,               -- 进入待拉取队列时间
  create_time   TIMESTAMPTZ(3) NULL,
  modify_time   TIMESTAMPTZ(3) NULL
);
CREATE INDEX IF NOT EXISTS idx_instance_tasks_job_id ON instance_tasks (job_id);
CREATE INDEX IF NOT EXISTS idx_instance_tasks_instance_id ON instance_tasks (instance_id);
CREATE INDEX IF NOT EXISTS idx_task_claim ON instance_tasks (app_id, status, dispatch_time); -- 热认领：等值+范围+排序
CREATE INDEX IF NOT EXISTS idx_task_lost ON instance_tasks (status, dispatch_time);         -- 失联回收巡检
CREATE INDEX IF NOT EXISTS idx_task_parent_state ON instance_tasks (parent_task_id, status); -- 派生幂等判定 + 父任务重派时级联作废后代
CREATE INDEX IF NOT EXISTS idx_instance_tasks_dispatch_time ON instance_tasks (dispatch_time);

-- Worker 上报的任务执行日志：uk(task_id, seq) 保证 at-least-once 重发不产生重复
CREATE TABLE IF NOT EXISTS instance_task_logs (
  id          BIGSERIAL PRIMARY KEY,
  task_id     BIGINT       NOT NULL DEFAULT 0,
  instance_id BIGINT       NOT NULL DEFAULT 0,
  seq         BIGINT       NOT NULL DEFAULT 0,   -- 任务内单调递增序号（worker 侧分配）
  level       VARCHAR(8)   NOT NULL DEFAULT '',
  log_time    BIGINT       NOT NULL DEFAULT 0,   -- epoch 毫秒
  content     TEXT,
  create_time TIMESTAMPTZ(3) NULL,
  CONSTRAINT uk_task_seq UNIQUE (task_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_instance_task_logs_instance_id ON instance_task_logs (instance_id);
CREATE INDEX IF NOT EXISTS idx_instance_task_logs_log_time ON instance_task_logs (log_time);

-- 登录审计
CREATE TABLE IF NOT EXISTS login_logs (
  id         BIGSERIAL PRIMARY KEY,
  username   VARCHAR(64)  NOT NULL DEFAULT '',
  ip         VARCHAR(64)  NOT NULL DEFAULT '',
  user_agent VARCHAR(256) NOT NULL DEFAULT '',
  success    BOOLEAN      NOT NULL DEFAULT FALSE,
  reason     VARCHAR(128) NOT NULL DEFAULT '',
  login_time TIMESTAMPTZ(3) NULL
);
CREATE INDEX IF NOT EXISTS idx_login_logs_username ON login_logs (username);
CREATE INDEX IF NOT EXISTS idx_login_logs_login_time ON login_logs (login_time);

-- 操作审计（管理面写操作）
CREATE TABLE IF NOT EXISTS operation_logs (
  id          BIGSERIAL PRIMARY KEY,
  username    VARCHAR(64)  NOT NULL DEFAULT '',
  role        VARCHAR(16)  NOT NULL DEFAULT '',
  action      VARCHAR(64)  NOT NULL DEFAULT '',
  app_id      VARCHAR(64)  NOT NULL DEFAULT '',
  method      VARCHAR(8)   NOT NULL DEFAULT '',
  path        VARCHAR(128) NOT NULL DEFAULT '',
  params      TEXT,
  result_code INT          NOT NULL DEFAULT 0,
  success     BOOLEAN      NOT NULL DEFAULT FALSE,
  ip          VARCHAR(64)  NOT NULL DEFAULT '',
  create_time TIMESTAMPTZ(3) NULL
);
CREATE INDEX IF NOT EXISTS idx_operation_logs_username ON operation_logs (username);
CREATE INDEX IF NOT EXISTS idx_operation_logs_action ON operation_logs (action);
CREATE INDEX IF NOT EXISTS idx_operation_logs_app_id ON operation_logs (app_id);
CREATE INDEX IF NOT EXISTS idx_operation_logs_create_time ON operation_logs (create_time);
