# gojob-server
> 本文的「兼容协议」= gojob 控制面刻意对齐的那套外部接口语义；对齐范围与逐条差异见根 README 的「兼容性说明」。


gojob 调度中心服务端，单一二进制内含 HTTP API、调度器、注册中心、派发器与存储层。技术栈：Go 1.27 + `gin` + `gorm` + `encoding/json/v2`。

## 分层

| 包 | 职责 |
| --- | --- |
| `api` | gin 路由与 handler：控制台 `/openapi/*`、Worker `/worker/*`、OpenAPI 兼容层 `/openApi/*`；`registercipher_api.go` 注册握手的接口层（`/worker/challenge`、`/openApi/challenge`、428 与拒绝文案的统一出口），`logs.go` 日志上报/查询，`stats.go` 统计总览，`system.go` 节点监控聚合（`GET /openapi/system/nodes`，副本表 + worker 表 + 项目调度归属），`openapi_compat.go` 兼容层（`authApp` 支持明文与 gojob 加密握手两种口令形态），`import.go` 兼容协议任务导入/导出（`GET /job/export` 回兼容协议 JobDTO，可直接粘回导入框往返），`account.go` 账号/RBAC，`audit.go` 操作日志与**项目归属判定**。任务列表 `GET /job/list` 语义对齐兼容协议会话接口：`index` 从 0 起 + `job_id` 精确 + `keyword` 模糊，返回 `PageResult{index,page_size,total_items,total_pages,data}`；`page_size<=0` 一次给全量（建单抽屉的上游候选要完整集合）。`job/operate` 的 `trigger` 可带 `instance_params` = 兼容协议「参数运行」 |
| `auth` | **无状态签名令牌**（`token.go`：`payload.signature`，HMAC-SHA256 + 集群密钥 `GOJOB_CLUSTER_SECRET`，用途分域 `console`/`worker`/`openapi`，跨域使用在验签阶段即被拒）+ 吊销基线（`revoke.go`：`user_infos.token_epoch`/`app_infos.token_epoch` +1 让此前签发的全部票即刻作废，各副本读同一列故跨副本生效；`revoked_tokens` 小表只用于"退出登录"这种单票吊销）。三个 Manager：`auth.go` 控制台会话（`X-Gojob-Token`，剩余不足一半时经 `X-GoJob-Token-Renewed` 响应头换发新票）、`workertoken.go` worker 数据面票（与 `app_id + instance_id` 绑定，TTL 10min，改项目口令/删项目走 `RevokeApp` 整片吊销）、`set.go` OpenAPI 票与 bcrypt 口令、`authApp` 鉴权。未配集群密钥时用本进程随机密钥并打 WARNING（仅单副本可用）。`registercipher.go` 是注册口令加密的服务端一侧（`KeyRing` 按 kid 选 key、`Challenges` 发/消费一次性凭证、`Clear` 把一次注册还原成明文口令），字节口径本身在 `internal/registercrypto`；**先消费 challenge 再解密**这个顺序是刻意的——反过来就等于拿一张票可以无限试 key |
| `cluster` | 多副本归属与协作：`lease.go` app 级租约（`app_leases`，TTL 10s / 3s 续约；**续约不改 epoch、接管才 +1**，`Fence` 在写库事务内以 `holder + epoch` 条件 UPDATE 挡下分区里的僵尸主）+ `Summarize` 归属视图（控制台「项目调度归属」表与 `/app/list` 的 `current_server` 都吃它）；`signal.go` 跨副本"队列变了"广播（`cluster_signals` 单行计数器 `seq`，各副本 1s 轮询唤醒本机长轮询，把跨副本新任务可见延迟从 ≤5s 压到 ≤1s）；`routing.go` 集群路由提示的数据源（`Owner` 联查 `app_leases` + `server_nodes`：**租约没过期 + 持有者心跳没掉线 + 配了对外地址**三条同时成立才给出 owner 地址，否则宁缺毋滥；`AliveServers` 给出在线副本地址列表），随 `/worker/heartbeat` 响应下发，worker 只当首选地址用 |
| `store` | gorm 建模与三种驱动接入；sqlite 走 `AutoMigrate`，DSN 追加 `_txlock=immediate`+WAL+`busy_timeout` |
| `scheduler` | 两层调度：粗扫线程（5s 装载窗口内到期 Job）+ 内存时间轮 `Wheel`（tick 10ms）精确触发，支持亚秒级周期；`NextByType`/`Preview` 按 `CRON/FIXED_RATE/FIXED_DELAY/DAILY_TIME_INTERVAL/API/DEPENDENCY` 计算（后两者无自动调度时间），固定频率与固定延迟单位为**毫秒**且上限 120000ms（对齐兼容协议）；`DAILY_TIME_INTERVAL` 解析兼容协议的 JSON 表达式并做时间窗/生效星期吸附（`dailyinterval.go`）；管理面改动经 `Reschedule` 即时重挂轮；`TriggerDependency` 在依赖上游实例成功后拉起下游实例（任务的时间类型与实例 `source` 均为 `DEPENDENCY`，幂等键 `(job_id, upstream_instance_id)`）。**多副本**：粗扫只装载本机持有租约的项目（`leases.Owns`），失去归属经 `OnLost`→`DropApp` 立刻撤轮；`fire` 有三道防线——装载过滤、`UPDATE job_infos ... WHERE id=? AND next_trigger_time=?` 的 CAS（抢输的一方整个事务回滚，返回 `ErrTriggerTaken`）、事务内的 `leases.Fence`（僵尸主哪怕 CAS 能赢也写不进库）；手动触发与依赖触发不推进计划时间，因而不要求归属，任意副本都能接。`leases` 传 nil 即单机模式，行为与引入集群前完全一致 |
| `registry` | Worker 心跳形成的**在线节点表**（`worker_nodes`）：`Alive` upsert 心跳、`Workers(appId)` 给分发策略按 address 排序的同一份候选集、`HasWorkers` 走热路径的行数判断、`All()` 给出带 `last_heartbeat/alive` 的快照供节点监控页用（勿与分发用的 `Workers(appId)` 混用）、`Evict` 清 10 个 TTL 没心跳的节点。落 DB 而不是留在进程内存，是为了多副本下每个副本看到同一份候选集 |
| `node` | 本进程自监控：`node.go` 采集（`getrusage` 进程 CPU、`runtime/debug` GC、`ConnState` 连接计数、长轮询/请求在途、QPS，环形缓冲保留最近 120 个采样点）；`report.go` 每 5s 按 `node_id` upsert `server_nodes` + gin 中间件 + 曲线 JSON；`identity.go` 生成 `hostname-pid-startNanos` 副本标识。指标原语在 `internal/proc`（与 Go SDK 共用） |
| `dispatch` | `fetchTask` 长轮询认领（先取 `target_worker_id` 指向自己的、剩余名额再取自由任务；候选 SELECT 与状态改写之间没有行锁，所以以逐行条件 UPDATE 的 `RowsAffected==1` 为认领成功依据，抢不到的行不下发 ⇒ 同一任务不会被两台机器各跑一次）、`report` 幂等回报（**只接受认领该任务的那台 worker**，越权/迟到回报静默忽略）、失败重入队、`ReassignLostTasks`（5min 回收失联，被请求停止的任务不复活）、`ReleaseStaleTargets`（60s 释放预选）；`selector.go` 移植兼容协议分发策略（健康优先/随机/指定机器 + 自建轮询）；`StopInstance` 把停止意图写库，靠 `fetchTask`/`heartbeat` 响应的 `cancel_task_ids` 下发。清单是"未 ack 前恒非空"的，所以长轮询只为 **2s 内新下达**的指令提前结束（`hasFreshCancels`）——拿"有没有 pending cancel"当早退条件会退化成忙等（实机：一个 30s 任务被重复下发 53 次停止指令，每轮两条查询 + 一个 HTTP 往返，还挤住 sqlite 单写者把停止意图自身的写入拖慢）；不早退≠不下发，响应体始终带全量清单，心跳 5s 一跳兜底。等待顺序是**先 `watchCh()` 取本轮要等的唤醒 channel、再 `claim()`**：反过来会在"认领返回空"与"取 channel"之间留下丢唤醒窗口（那次 Notify close 的是即将被丢弃的旧 channel），只能靠 ≤5s 兜底复查救回，200 节点压测里表现为整批任务统一迟到 ~1s。容量的实测数字与承重项（每份任务的认领都是一次写事务，sqlite 单写者 ⇒ ≈0.3ms/节点 的排队成本）见根目录 `README.md`「容量实机数字」，复现脚本 `scripts/verify-capacity.sh` |
| `cleanup` | 历史实例回收（`server/cleanup/cleanup.go`）：按任务 `life_cycle`（无则按 `GOJOB_RETENTION_DAYS`）删除超期的**终态**实例，并在一个短事务里按"任务日志 → 子任务 → 实例"级联删掉；每 10 分钟一轮、每批 20 个实例，单任务单轮上限 25 批，绝不做一次性大事务（sqlite 单写者 / MySQL 长事务会卡住热路径认领）。多副本只清本机持有归属的项目；两道闸门都不配时**什么都不删**并在启动时警告。顺带回收"任务已删除"的孤儿实例（仅在全局保留天数生效时） |

## 运行

```bash
go run ./cmd/gojob-server -addr :7800 -driver sqlite -dsn ./gojob.db
```

| flag | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `-addr` | `GOJOB_ADDR` | `:7800` | 监听地址 |
| `-driver` | `GOJOB_DB_DRIVER` | `sqlite` | `sqlite` \| `mysql` \| `postgres` |
| `-dsn` | `GOJOB_DB_DSN` | `gojob.db` | 对应驱动 DSN，格式见下「数据库 DSN」 |
| `-admin-user` | `GOJOB_ADMIN_USER` | `admin` | 首启播种管理员（`user_infos` 为空时） |
| `-admin-password` | `GOJOB_ADMIN_PASSWORD` | `admin123` | 播种密码（bcrypt 存储） |
| `-public-url` | `GOJOB_PUBLIC_URL` | 自动推导 | 本副本的**对外可达地址**：随心跳下发给 worker 当长轮询目标（`cluster_hint`），并用于统计总览展示。多副本时各副本填自己那台"worker 打得通"的地址 |
| `-cluster-secret` | `GOJOB_CLUSTER_SECRET` | 空（本进程随机） | 令牌签名密钥。**多副本必须各节点配同一个值**，否则 A 签的票打到 B 就验不过（登录随机掉线、worker 随机 401）；留空只在单副本下可用，启动日志会打 WARNING。轮换该值等于让所有在野令牌即刻失效 |
| `-retention-days` | `GOJOB_RETENTION_DAYS` | 0（永不清理） | 历史实例保留天数：终态实例超过它就连同 `instance_tasks`、`instance_task_logs` 一起回收，并顺带清理"任务已删除"的孤儿实例。任务上配的 `life_cycle` 优先于它。**两边都不配则三张历史表只增不减**，启动日志会警告。删除按 10min 一轮、每批 20 个实例的短事务推进（`server/cleanup`） |
| `-db-max-open-conns` | `GOJOB_DB_MAX_OPEN_CONNS` | mysql/pg 20 · sqlite 10 | 数据库连接上限。`database/sql` 的默认是无限 + 只保留 2 条空闲，worker 数量一大就会一路新建连接顶到 MySQL `max_connections`（默认 151）；多副本按「副本数 × 本值 ≤ max_connections − 预留」估算 |
| `-db-max-idle-conns` | `GOJOB_DB_MAX_IDLE_CONNS` | 跟随上面的最大值 | 空闲连接保留数 |
| `-db-conn-max-lifetime` | `GOJOB_DB_CONN_MAX_LIFETIME` | `30m` | 单条连接最长存活时间（Go 时长字面量）。数据库主备切换后可调小到 `5m` 加快甩掉陈旧连接 |
| `-worker-aes-keys` | `GOJOB_WORKER_AES_KEYS` | 空（不启用） | 注册口令加密的 AES-256 key，逗号分隔 64 位 hex（多把只为轮换期并存）。配了之后 `/worker/register` 与 `/openApi/authApp` 接受 `auth_scheme=aes-gcm`：客户端先 `/worker/challenge` 领一张 30s、一次性（消费记录在 `revoked_tokens` 主键上，跨副本互斥）的凭证，再把 `{v,app_id,instance_id,challenge,password}` 用 HKDF 派生 key + AES-256-GCM 送上。线格式只有一份实现（`internal/registercrypto`），Go SDK 与它同源，Java SDK 由黄金向量测试钉住 |
| `-worker-aes-required` | `GOJOB_WORKER_AES_REQUIRED` | `false` | 要求所有注册都必须加密，明文回 `428 CodeChallengeRequired` + 一张现领的 challenge。**刻意不用 401**：SDK 把 401 当"票过期→重注册"，用 401 会形成死循环。只配 REQUIRED 不配 key 会启动失败（没有客户端能注册上来） |

优先级：环境变量 > flag > 默认值。

### 数据库 DSN

| 驱动 | 示例 |
| --- | --- |
| `sqlite` | `./gojob.db`（文件路径；DSN 里没有 `_pragma` 时自动补 WAL + `busy_timeout(5000)` + `_txlock=immediate`） |
| `mysql` | `gojob:gojob123@tcp(127.0.0.1:3306)/gojob?charset=utf8mb4&parseTime=True&loc=Local` |
| `postgres` | `postgres://gojob:gojob123@127.0.0.1:5432/gojob?sslmode=disable`，或 `host=127.0.0.1 port=5432 user=gojob password=gojob123 dbname=gojob sslmode=disable` |

MySQL 必须带 `parseTime=True`；`pgsql` / `postgresql` 与 `postgres` 等价；驱动留空按 sqlite。TLS 分别用 `tls=true|skip-verify` 与 `sslmode=require|verify-full`。完整说明见根目录 `README.md`「数据库 DSN 格式」。

## 数据库初始化

- sqlite：无需建表脚本，启动即 `AutoMigrate`（新列/新索引自动补齐）。
- **列的一条硬约定：凡是"0 是合法配置"的列，model 上不许出现 `default:` 标签。** gorm 的 `Create` 会跳过"带 `default` 标签且值为零"的列，把 0 交给库里的 `DEFAULT`——`job_infos.max_retry_times`(0=不重试)、`instance_retry_num`(同上)、`instance_num_limit`(0=不限制) 就因此被静默顶成 3，而且 `Create` 之后内存里的结构体仍是请求值，只有读回库里才看得见（实机表现：一个 0 重试的任务跑满 4 次尝试）。默认值统一由 API 层 `orUnset` 填；DDL 脚本里的 `DEFAULT 3` 保留，只服务外部导入，存量库不需要改。
- mysql：首次部署导入 `../scripts/ddl/mysql.sql`。
- postgres：首次部署导入 `../scripts/ddl/postgres.sql`。
- **存量库升级**（已有 mysql/pgsql 部署）：DDL 脚本只在初始化时执行，跟版本需要手工 ALTER，然后重启进程即可（`auth.UpgradeAppPasswords` 会在首启把明文项目密码升级成密文）：

```sql
ALTER TABLE instance_tasks ADD INDEX idx_task_claim (app_id, status, dispatch_time);
ALTER TABLE instance_tasks ADD INDEX idx_task_lost (status, dispatch_time);
ALTER TABLE instance_tasks DROP INDEX idx_app_state;   -- 已被 idx_task_claim 前缀覆盖
ALTER TABLE job_instances  ADD COLUMN upstream_instance_id BIGINT UNSIGNED NULL;
ALTER TABLE job_instances  ADD UNIQUE INDEX idx_job_upstream_instance (job_id, upstream_instance_id);
ALTER TABLE job_infos      ADD COLUMN upstream_job_id BIGINT UNSIGNED NOT NULL DEFAULT 0;
ALTER TABLE app_infos      MODIFY COLUMN password VARCHAR(128) NOT NULL DEFAULT '';
-- 只有按旧版脚本建过库时才需要；password_md5 已废弃（库里不再存任何明文口令等价物）
ALTER TABLE app_infos      DROP COLUMN password_md5;
-- 节点监控：新表，直接执行 scripts/ddl/*.sql 里的 CREATE TABLE IF NOT EXISTS server_nodes 段
-- 集群（多副本）新增 4 张表：app_leases / worker_nodes / cluster_signals / revoked_tokens
--   （同样直接取 scripts/ddl/*.sql 里的同名 CREATE 段；cluster_signals 的 id=1 那一行不用手工插）
-- 另需两个吊销基线列（无状态签名令牌靠它们做跨副本即刻吊销）：
ALTER TABLE app_infos  ADD COLUMN token_epoch BIGINT NOT NULL DEFAULT 0;
ALTER TABLE user_infos ADD COLUMN token_epoch BIGINT NOT NULL DEFAULT 0;
-- 脚本执行链：任务分单时要把处理器类型带给 worker，别按"没这列"去查 EXTERNAL 报错
ALTER TABLE instance_tasks ADD COLUMN processor_type VARCHAR(32) NOT NULL DEFAULT 'BUILT_IN';
-- 多任务执行类型（BROADCAST/MAP/MAP_REDUCE）：一个实例是一棵任务树，子任务落库、由 server 当 TaskTracker
ALTER TABLE job_instances  ADD COLUMN execute_type VARCHAR(16) NOT NULL DEFAULT 'STANDALONE';
ALTER TABLE job_instances  ADD COLUMN result TEXT;
ALTER TABLE instance_tasks ADD COLUMN parent_task_id   BIGINT UNSIGNED NOT NULL DEFAULT 0;
ALTER TABLE instance_tasks ADD COLUMN map_batch        INT             NOT NULL DEFAULT 0;
ALTER TABLE instance_tasks ADD COLUMN map_attempt      INT             NOT NULL DEFAULT 0;
ALTER TABLE instance_tasks ADD COLUMN execute_type     VARCHAR(16)     NOT NULL DEFAULT 'STANDALONE';
ALTER TABLE instance_tasks ADD COLUMN task_key         VARCHAR(64)     NOT NULL DEFAULT '';
ALTER TABLE instance_tasks ADD COLUMN task_name        VARCHAR(256)    NOT NULL DEFAULT '';
ALTER TABLE instance_tasks ADD COLUMN sub_task_content TEXT;
-- 派生幂等判定与父任务重派时的后代级联作废都靠它；漏建不报错，只是大树上每次 map 走全表扫
ALTER TABLE instance_tasks ADD INDEX idx_task_parent_state (parent_task_id, status);
```

漏列的后果要说清楚：sqlite 由 `AutoMigrate` 自动补齐，mysql/pgsql 不会。`job_instances` 少 `execute_type`/`result` 时，**每一次触发（包括原有的 STANDALONE 任务）的 INSERT 都会报 `Unknown column 'execute_type'`** —— 实例根本建不出来，是硬失败而不是"新功能不可用、旧功能照常"。`instance_tasks` 少那七列同理。所以这批 ALTER 必须整批执行，不能挑着做。pg 方言的列类型（`BIGINT`/`TEXT`）与 mysql 不同，直接从 `../scripts/ddl/postgres.sql` 里取同名列那几行，别照抄上面的语句。

`cluster_signals` 那行 `id = 1` 的记录漏插了不会报错，只是跨副本唤醒退化为长轮询自身的 ≤5s 兜底重查（`Signal.Bump` 发现一行都没有时会自愈重建）。

## 多副本部署

同一套库上起 N 个本进程即可，副本之间不通信，一切协调都在库里：

- **归属**：`app_leases` 按项目划分调度所有权（TTL 10s、每 3s 续一跳），接管最坏 ≈15s；进程优雅退出会主动交出租约，接管方不必等满 TTL。
- **不双跑**：装载过滤 + `next_trigger_time` CAS + 事务内 epoch 围栏，分区里的僵尸主写不进库。
- **令牌**：三类票都由集群密钥签发，任一副本互认（`GOJOB_CLUSTER_SECRET` 必须一致）。
- **在线节点**：`worker_nodes` 落库，各副本看到的候选集一致。
- **worker 连哪台**：数据面不绑归属，任何副本都能收单/回报/收日志。心跳响应额外带 `cluster_hint`：`servers` = 在线副本对外地址（`server_nodes.public_url`），`owner` = 本 app 租约持有者的对外地址。worker 把它当**首选地址**（连续 2 跳确认才迁、连不上就退回列表），所以 worker 侧不需要 LB/VIP；这一步不增加任何 QPS，也不新增端点（对比兼容协议的 `/server/acquire` 每 10s 一次 + 每次发现都对 owner 发 PING）。
- **对外地址**：`-public-url` 就是本副本对 worker 的可达地址（复用，没有引入新的 `-advertise-address`）。它只影响下发内容，配错时 worker 跳一次就退回，不影响正确性。

副本请**滚动启动**（两进程同时跑首启建表会撞 DDL 锁）。上面每一条都有实机断言：`scripts/verify-cluster.sh`（双副本：令牌互认、归属唯一、接管与僵尸主）与 `scripts/verify-owner-routing.sh`（双副本 + 10 个 worker：owner 收敛、接管迁移、对外地址配错时不承重）。

## 健康检查

`GET /healthz` → `{code:0,data:"ok"}`。
