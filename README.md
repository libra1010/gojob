# gojob

轻量级分布式任务调度平台：单二进制 Server + 一个数据库就是一套调度中心（MySQL / PostgreSQL / SQLite，零中间件），Go / Java Worker 走 HTTP 长轮询接入，广播与 MapReduce 内置；200 个执行节点在线时，一次广播 200 份任务在 150ms 内全部派发到位（口径与复现见「容量实机数字」）。

一个 Server 进程 + 一个数据库就能起整套调度中心：调度与执行彻底分离，Server 只管"什么时候触发、派给谁、跑到哪一步了"，业务代码跑在 Worker 侧。不需要 ZooKeeper、不需要 Kafka/RocketMQ、不引入任何 RPC 框架，Worker 与 Server 之间只有 HTTP；要多副本就在同一套库上起 N 个进程（app 级租约归属 + fencing epoch，见「Server 集群（多副本）」节）。

能力：控制台（Vue3）· 六种时间类型（CRON / 固定频率 / 固定延迟 / 每日固定间隔 / API / 依赖触发，后两种支持亚秒级）· 四种执行类型（单机、广播、Map、MapReduce）· 四种分发策略（健康优先 / 随机 / 标签定向 / 轮询）· 零代码脚本节点 `gojob-agent`（控制台有专门的脚本录入入口）· 协作式停止实例 · 任务日志不丢不重 · 三级角色 RBAC + 登录/操作审计 · 任务包导入导出（可批量、可整项目）· 每个副本与每个 Worker 的运行时指标监控。

暂不含工作流（Workflow，规划在 M5）。控制面协议、控制台交互与任务导入导出格式与 [PowerJob](https://github.com/PowerJob/PowerJob) 对齐，可以直接吃它导出的任务包；Worker 数据面是 gojob 自有的 HTTP 长轮询协议，不兼容 PowerJob 的 Worker（逐条差异见文末「兼容性说明」）。

## 架构

```
                    ┌───────────────────────────┐
   浏览器 ──HTTP──▶ │  dashboard (Vue3 + Element)│  控制台
                    └──────────────┬────────────┘
                                   │ /openapi/*  (X-Gojob-Token)
                                   ▼
   OpenAPI 客户端 ─▶ ┌───────────────────────────┐ ◀── Go/Java Worker
   /openApi/*  ────▶ │        gojob-server        │     /worker/*  长轮询
                      │  gin + gorm + encoding/json/v2 │
                      └──┬──────────┬──────────┬───┘
                         │          │          │
                     scheduler   registry   dispatch
                     (cron tick) (心跳/在线) (派发/回报/日志)
                         │          │          │
                         └──────┬───┴──────────┘
                                ▼
                     sqlite / mysql / postgres (gorm)
```

- **调度器 scheduler**：**两层调度**——粗扫线程每 5s 把 `next_trigger_time` 落在扫描窗口内的启用 Job 装载进**内存时间轮**（`Wheel`，tick 10ms），到点由时间轮精确触发，因此支持**亚秒级**周期；触发后生成 `job_instance` + `instance_task` 并原子推进下次时间。支持 skip_when_running（执行中不再发起新实例）。
- **无在线节点时的触发**：与兼容协议 `DispatchService` 同结论——触发那一刻项目一个在线节点都没有时，实例与根任务**当场记 `FAILED`**（结果 `无可用 worker：该项目当前没有在线节点`），计划时间照常推进，**不留在队列里等节点上线后补跑**；零节点广播仍是例外（根任务即终态，记 `SUCCEED` + 结果说明）。兜底巡检每 30s 一次，把排队超过 1 分钟、项目仍无在线节点的 `WAITING` 任务判 `FAILED`，覆盖"触发后节点立刻掉线"；节点在线但都忙时任务继续排队，由 worker 空闲额度自然背压。全停接单的项目也仍然排队（接单开关只暂停派发）。
- **时间类型**：`CRON` / `FIXED_RATE`（周期**毫秒**，以上次计划时间为基准、不累积漂移）/ `FIXED_DELAY`（周期**毫秒**，从本次实际触发时刻再等一个周期）/ `DAILY_TIME_INTERVAL`（每日固定间隔：表达式为 `DailyTimeIntervalExpress` 形状的 JSON，`{interval,intervalUnit,startTimeOfDay,endTimeOfDay,daysOfWeek}`，周一=1…周日=7，空=每天；只在时间窗内按间隔触发，窗口外顺延到下一有效日的窗口起点）/ `DEPENDENCY`（**依赖触发**，见下条）/ `API`（仅手动/OpenAPI 触发）。固定频率与固定延迟周期上限 **120000ms**，与兼容协议的 `FREQUENT_JOB_MAX_INTERVAL` 一致。控制台建单可选类型并**预览最近 N 次触发时间**（毫秒精度，三种毫秒级类型的预览带 `.000` 毫秒位）。实例的 `source` 字段记录触发它的调度类型。
- **注册中心 registry**：Worker 心跳上报地址、内存、`sdk_type`（GO / JAVA / JAVA_SPRINGBOOT / JAVA_QUARKUS）与 runtime 指标，在线节点表**落在 `worker_nodes` 表里**（多副本下每个副本看到的候选集一致）。
- **集群归属 cluster**：多个 Server 副本共用一套库时，按项目（app）划分调度所有权——`app_leases` 租约（TTL 10s / 3s 续约）决定谁调度，`epoch` + 事务内条件 UPDATE 挡住分区里的僵尸主，`cluster_signals` 单行计数器负责跨副本唤醒。详见「Server 集群（多副本）」节。
- **节点监控 node**：每个 Server 副本每 5s 采样自己的进程指标（进程 CPU%、RSS/堆、协程数、GC 次数与累计暂停、活跃/空闲 HTTP 连接、长轮询在途、请求 QPS），按 `node_id` upsert 进 `server_nodes` 表，控制台聚合展示所有副本；Worker 的同类 runtime 指标随 `heartbeat` 上报，与节点信息一起进 `worker_nodes` 表。取不到的指标一律报 0、页面显示"—"，不用 0 冒充"很空闲"。详见「节点监控」节。
- **停止实例**：`POST /openapi/instance/stop`（OpenAPI 兼容层 `stopInstance` 同一实现）把停止意图写进 `instance_tasks.stop_request_time`，WAITING 任务当场置 `STOPPED`，RUNNING 任务由下一次 `fetchTask`/`heartbeat` 响应以 `cancel_task_ids` 捎带给 worker，SDK cancel 该任务的 ctx（Java 为 `Thread.interrupt`）后回报 `STOPPED`。因为意图在数据库里而不在连接里，**server 在下达指令后立刻宕机、稍后重启，停止依然会送达**（实测：worker 全程冻结 → 停止 → server SIGKILL 并重启 → 恢复 worker 后 12 秒内任务被取消，实例 `STOPPED`）。取消是协作式的：处理器必须响应 `ctx.Done()`／`InterruptedException`，否则会跑到自然结束——但**记账仍然是 `STOPPED`**（worker 侧只要收到过停止指令就按停止回报，不看处理器的返回值；实机验证：一个吞掉中断、跑满全程并返回成功的任务，实例与任务都是 `STOPPED`，结果列留着它自己那句"跑满了 30 秒"）。所以准确的表述是：**状态总是准的，副作用不一定停得住**。送达时延取决于有没有空闲并发额度：额度没占满时长轮询在飞，`StopInstance` 提交即唤醒、秒级送达；额度占满时 SDK 不再发 `fetchTask`，只能靠 5s 一跳的心跳捎带（实测从下达停止到任务收尾 4.92s）。
- **分发策略**：入队时（`fire` / 手动触发）按 `dispatch_strategy` 预选执行节点，语义与兼容协议的 `TaskTrackerSelector` 同构 —— `1 HEALTH_FIRST`（默认，打分公式 `(内存上限−已用)×2 + (可用核数−CPU负载)`）、`2 RANDOM`、`11 SPECIFY`（`dispatch_strategy_config`：`tagEquals:x` / `tagIn:x` / 逗号分隔 `ip:port`·tag，落空降级随机并告警）、`21 ROUND_ROBIN`（**gojob 扩展**，per-job 游标严格 A→B→A 轮流）。候选集先按 `designated_workers` 收窄；预选节点宕机/长时间不取件时，任务在 60s 后由 `ReleaseStaleTargets` 退回自由池，避免饿死。
- **依赖触发（一种调度方式）**：把任务的「时间类型」选成 `DEPENDENCY`，再选一个「上游任务」（`upstream_job_id`）——上游的一次执行**成功**收尾后，Server 立刻为下游创建实例（`source=DEPENDENCY`，实例上记录 `upstream_instance_id`），下游按自身的分发策略与参数独立执行；上游失败或被停止不传导。幂等靠 `job_instances` 的 `(job_id, upstream_instance_id)` 唯一索引——worker 回报重试、server 重启后重放都只会触发一次。该类型没有 cron/周期表达式、不会被时间驱动；反之其他时间类型不允许残留 `upstream_job_id`，所以依赖关系只可能来自这一种调度方式，任务列表的「时间表达式」列会直接显示它在等哪个上游。保存期校验：必须选上游、上游须同项目、非自身、不成环；仍有下游依赖它的任务不允许删除。
- **派发 dispatch**：Worker 长轮询 `fetchTask` 认领任务（只取 `target_worker_id` 为空或指向自己的）→ 执行 → `report` 回报；失败重入队列重试（重试与回收都会清空预选）；`ReassignLostTasks`（5 分钟）回收失联任务；回报幂等（已终态的 task 忽略重复回报）。
- **任务树（BROADCAST / MAP / MAP_REDUCE）**：一个实例是一棵 `instance_tasks` 树，根任务 `task_key='0'`、`parent_task_id=0`。`BROADCAST` 由 Server 在触发那一刻按在线节点快照展开，每台一行、各自钉在自己的 `target_worker_id` 上；`MAP`/`MAP_REDUCE` 由处理器运行期调 `map()` 经 `POST /worker/mapTask` 派生（派生幂等键 = `(父任务, 第几次尝试, 第几批)`，所以父任务重跑能重新派出一批，而旧一代未收敛的后代被置 `DISCARDED` 不重复执行副作用）。实例是否收敛**不靠计数器递减**，而是每次现算 `COUNT(status IN (WAITING,RUNNING))`，崩溃在两步之间也不会把计数打歪；`MAP_REDUCE` 在树排空的那一刻才补出 reduce 行（`OMS_LAST_TASK` / `9999`），由它经 `POST /worker/taskResults` 分页取回全部子任务结果，它的返回值就是实例结果。控制台的实例详情走 `POST /openapi/instance/detailPlus`（`Task 统计` + 子任务分页，那句 `select * from task_info where …` 只认白名单语法）。详见「兼容性说明」里的两处刻意偏离。
- **日志通道 logs**：Worker 采集执行日志批量上报 `reportLogs`，见下文「任务执行日志」。

## 目录

| 路径 | 说明 |
| --- | --- |
| `cmd/gojob-server` | Server 可执行程序入口 |
| `cmd/gojob-demo` | Go SDK 示例 Worker（hello / slow / failOnce 处理器） |
| `cmd/gojob-agent` | 零代码执行节点：不写代码，只跑脚本库里预置的 Shell 脚本，见 `worker/README.md` |
| `cmd/gojob-loadgen` | 容量与派发延迟的节点发生器（一台机器起 N 个真 worker 节点），供 `scripts/verify-capacity.sh` 用，见「容量实机数字」 |
| `server/` | Server 内部分层：`api` `auth` `store` `scheduler` `registry` `dispatch` `cleanup`（历史实例回收）`cluster`（副本归属租约 + 跨副本唤醒信号）`node`（进程自监控）`webui`（单镜像构建时内嵌控制台） |
| `worker/` | Go Worker SDK（供业务进程内嵌） |
| `internal/proc` | 跨平台进程指标原语（CPU 时间 / RSS / 打开 fd），Server 与 Go SDK 共用 |
| `internal/agent` | Shell 脚本处理器（官方脚本处理器的对位实现）：脚本库解析、参数、超时、进程组取消 |
| `internal/protocol` | Server ↔ Worker / 控制台共用协议与数据模型 |
| `dashboard/` | Vue3 + Element Plus 控制台 |
| `sdk-java/` | Java Worker SDK（core + Spring Boot starter + Quarkus 示例） |
| `scripts/ddl` | MySQL / PostgreSQL 建表脚本 |
| `deploy/` | compose 初始化脚本、nginx 反代模板、pg 初始化 SQL |

## 快速开始

### 方式一：Docker Compose（推荐）

```bash
cp .env.example .env          # 按需修改密码等
docker compose up -d          # 默认 sqlite，起 server + demo worker + dashboard
# 使用 MySQL：
docker compose --profile mysql up -d
# 使用 PostgreSQL：
docker compose --profile postgres up -d
# 切换数据库还要在 .env 里指定驱动与 DSN（格式见「数据库 DSN 格式」）
# 再加一个零代码脚本节点（默认不起，安全含义见 worker/README）：
GOJOB_SCRIPT_LIB=./scripts-lib docker compose --profile agent up -d
```

**docker.io 拉不动的机器**（国内）改用镜像版 Dockerfile：基础镜像是华为云 SWR 的搬运副本，服务端构建期模块代理走 goproxy.cn，dashboard 的 pnpm 本体与依赖走 npmmirror，其余与默认版一致。

```bash
GOJOB_DOCKERFILE=Dockerfile.cn GOJOB_DASHBOARD_DOCKERFILE=dashboard/Dockerfile.cn \
  docker compose up -d --build
# 或单独构建服务端 / 控制台镜像：
docker build -f Dockerfile.cn --build-arg BUILD_TARGET=gojob-server -t gojob/gojob-server:latest .
docker build -f dashboard/Dockerfile.cn -t gojob/dashboard:latest .
```

这两个开关只覆盖我们自己构建的四个镜像（server / demo / agent / dashboard）；`mysql`、`postgres`、`curlimages/curl` 是上游镜像，要换源得各自改 `image:` 或配 docker daemon 的 `registry-mirrors`。SWR 的 tag 由对方仓库同步进度决定，撞上"还没同步这个 tag"时用 `--build-arg ALPINE_IMAGE=…/alpine:3.20`（dashboard 用 `NODE_IMAGE` / `NGINX_IMAGE`）换一个即可，不必改文件。

启动后：
- 控制台 <http://localhost:8080>，默认账号 `admin / admin123`（**上线第一件事就是改密码**）
- Server API <http://localhost:7800>，健康探针 `/healthz`
- 数据落在 `gojob-sqlite` 卷里；compose 默认给 `GOJOB_RETENTION_DAYS=30`，历史实例会自动回收

### 方式二：二进制 / go run

需要 **Go 1.27**（用到标准库 `encoding/json/v2`）。发布产物也可以直接下载解压，不需要 Go 工具链。

```bash
cd /path/to/gojob
# 1) 起 Server（默认 sqlite，自动建表）
go run ./cmd/gojob-server -addr :7800 -driver sqlite -dsn ./gojob.db

# 2) 起示例 Worker（另开终端）
go run ./cmd/gojob-demo   # 读取 GOJOB_WORKER_* 环境变量或默认值

# 3) 起控制台（另开终端）
cd dashboard && pnpm install && pnpm dev   # http://localhost:5173
```

### 方式三：单镜像（控制台内嵌）

把控制台产物经 `go:embed` 编进 gojob-server，一个容器、一个端口同时提供页面与 API，不用再管 nginx 反代和 dashboard 镜像。

```bash
# 默认版（基础镜像取自 docker.io）
docker build -f Dockerfile.all -t gojob/gojob-all:latest .

# 国内网络：基础镜像走华为云 SWR，pnpm 本体与依赖走 npmmirror，模块代理走 goproxy.cn
docker build -f Dockerfile.all.cn -t gojob/gojob-all:latest .

docker run -d --name gojob -p 7800:7800 -v gojob-data:/app/data gojob/gojob-all:latest
```

- 控制台与 API 同在 <http://localhost:7800>，默认账号 `admin / admin123`（首启播种）
- 健康探针 `/healthz`；sqlite 数据落在 `gojob-data` 卷的 `/app/data` 下
- 换 MySQL / PostgreSQL：追加 `-e GOJOB_DB_DRIVER=mysql -e GOJOB_DB_DSN='…'`（DSN 格式见「数据库 DSN 格式」）

内嵌只发生在带 `-tags dashboard` 的构建里（两个 `Dockerfile.all*` 已带上）；`go build ./cmd/gojob-server`、`go run` 和 compose 多镜像跑法不受影响。`server/webui/dist` 不进仓库，改完控制台重新构建镜像即可。

## 接入自己的应用

调度中心只负责"什么时候触发、派给谁、跑到哪一步"，业务代码跑在你自己的进程里。三种接法：

**Go：嵌进现有进程**（`worker/README.md` 有完整清单）

```go
type hello struct{}

func (hello) Process(ctx *worker.TaskContext) error {
    select {
    case <-ctx.Done():          // 被停止：Go 侧靠内嵌的 context（TaskContext 内嵌 context.Context）
        return ctx.Err()
    default:
    }
    ctx.SetResult("hello")      // 实例结果 / reduce 的输入
    return nil
}

func main() {
    worker.Register("hello", hello{})           // 名字对应任务配置里的"处理器类名"
    w := worker.New(worker.Config{
        ServerAddress: "http://127.0.0.1:7800",  // 多副本用 ServerAddresses，worker 自己容错
        AppId:         "demo_app", Password: "demo123",
    })
    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()
    log.Fatal(w.Run(ctx))                        // 阻塞；ctx 取消时先 drain（等在跑任务回报完）再退出
}
```

处理器就是一个实现 `Process(*worker.TaskContext) error` 的类型：返回 `nil` 即成功，失败直接返回 `error`（Go 侧刻意不套一层 `ProcessResult`）。

**Java：三种形态**（`sdk-java/README.md`）——裸 `GoJobWorker`、Spring Boot starter（`@GoJobProcessor` 注解注册）、Quarkus 示例。停止检查用 `ctx.checkStopped()` / `ctx.isStopped()`，别只依赖线程中断标志。

**不写代码：起一个 `gojob-agent`**，把脚本丢进脚本库，任务里填脚本文件名即可。默认只允许执行脚本库里的裸文件名（越界路径直接拒绝），内联脚本文本要显式开 `GOJOB_AGENT_ALLOW_INLINE_SCRIPTS`。这个进程等同于"拿到项目口令的人能在该主机执行命令"，请部署到隔离网段——完整边界写在 `worker/README.md`「零代码执行节点」。

任务在控制台里建，也可以走 OpenAPI，或导入兼容协议导出的任务包。Worker 侧只需要三样东西：Server 地址、项目 ID、项目口令（注册时换一张短期 token，之后数据面不再带口令）。

## 数据库初始化

- **SQLite**：无需脚本，Server 启动时 gorm `AutoMigrate` 自动建表（WAL、`busy_timeout`、`_txlock=immediate` 由 Server 自动追加，见「数据库 DSN 格式」）。
- **MySQL**：`scripts/ddl/mysql.sql`
- **PostgreSQL**：`scripts/ddl/postgres.sql`（compose 场景由 `deploy/postgres-compose.sql` 自动执行）

```bash
# MySQL
mysql -u root -p < scripts/ddl/mysql.sql
# PostgreSQL
psql -U postgres < scripts/ddl/postgres.sql
```

存量库升级（SQLite 由 AutoMigrate 自动建，无需处理；mysql/pgsql 只在初始化时跑脚本，升级要手工执行）：

| 期次 | 新增对象 |
| --- | --- |
| 节点监控 | `server_nodes` 表 |
| Server 集群 | `app_leases`、`worker_nodes`、`cluster_signals`、`revoked_tokens` 四张表，加 `app_infos.token_epoch` / `user_infos.token_epoch` 两个吊销基线列 |
| 脚本执行链 | `instance_tasks.processor_type` 一列（建单时从 `job_infos` 抄，随任务下发给 worker） |
| 多任务执行类型 | `job_instances` 加 `execute_type` / `result` 两列；`instance_tasks` 加 `parent_task_id` / `map_batch` / `map_attempt` / `execute_type` / `task_key` / `task_name` / `sub_task_content` 七列，以及复合索引 `idx_task_parent_state (parent_task_id, status)` |

从 `scripts/ddl/mysql.sql` / `scripts/ddl/postgres.sql` 里取出对应的 `CREATE TABLE IF NOT EXISTS ...` 段落执行（两种方言的列类型不同，勿混用），随后重启进程。`cluster_signals` 里那唯一一行 `id = 1` 不用手工插：进程首启的 `Migrate` 会补，运行期发现它被删了也会自愈重建。`server/README.md`「数据库初始化」节有可直接粘贴的 ALTER 清单。

## 实机验收脚本

`scripts/verify-owner-routing.sh`（**自己起进程**的 owner 路由实机验收，29 项断言，约 4 分钟）：两个副本 + **10 个 worker**（地址列表一半 A 优先、一半 B 优先，制造初始 5/5 分散），验证 ① 心跳下发的 `cluster_hint` 让 10/10 收敛到租约持有者、对端长轮询归零，且"首地址即 owner"的那 5 个一次都不迁、非 owner 的那 5 个恰好各迁 1 次（连续 2 跳确认的防抖生效）；② `SIGSTOP` 冻结 owner 后另一副本 TTL 内接管（epoch +1），worker 跟着搬到新 owner，冻结的 50s 窗口内仍新增 13 个实例、实例照样执行成功，`SIGCONT` 后旧主不抢回、全程无双跑；③ **把某副本的对外地址故意配成连不上的端口**，worker 跳过去发现不可达就退回另一副本继续收单回报，任务照常触发并完成（14 个实例、14 个成功）——这就是"owner 提示不承重"的证据。默认 sqlite，端口 `7851~7854`，可用 `NW/A_PORT/B_PORT/C_PORT/D_PORT/SECRET` 覆盖。

`scripts/verify-worker-token.sh`（对着运行中的 Server 跑，14 项断言）：换项目口令注册、数据面只认 token、口令直连被拒、冒用他人 `instance_id`/跨项目用票被拒、`authApp` 只收明文而 md5-only 被明确拒绝、以及库里不存在口令明文与 md5 摘要。默认打 `http://127.0.0.1:7789` 的 `demo_app/demo123`，可用 `BASE/APP/PW` 环境变量覆盖。

`scripts/verify-cluster.sh`（**自己起进程**的双副本实机验收，28 项断言，约 2 分钟）：编译并滚动起两个共用同一套库的 Server，逐项验证 A 签的会话票/worker 票在 B 可用、心跳打在 B 上的节点 A 也看得见、每个项目全集群只有一个副本声明归属、归属副本入队的任务由连在另一副本上的 worker 在 5s 内拿到、12s 内 3s 周期任务只新增约 4 个实例（双跑会是 8 个）且没有一对实例相差 500ms 以内、`SIGSTOP` 冻结归属副本后另一副本在 TTL 内接管且 epoch +1、`SIGCONT` 解冻旧主后它既不抢回归属也不双跑。默认用 sqlite（本机没有 docker 与 mysql/pgsql），端口 `7791/7792`，可用 `A_PORT/B_PORT/APP/PW/SECRET` 覆盖。

`scripts/verify-java-routing.sh`（Java SDK 侧的同一套验收，18 项断言，约 3 分钟）：`mvn -o test-compile` 出 `gojob-worker-core`，用 test 目录里的 `QuickStart` 起 **4 个 Java worker**（地址列表两两反向）打到两个副本上，验证收敛到 owner、"首地址即 owner 的不迁 / 非 owner 的恰好迁 1 次"、领任务与回报 SUCCEED、`SIGSTOP` 冻结 owner 后靠 15s 短超时认出僵尸副本并跟到新 owner（4/4）、冻结期间实例照样产生并完成、解冻不抢回、全程无双跑。端口 `7861/7862`。

`scripts/verify-shell-agent.sh`（**自己起进程**的零代码执行节点实机验收，38 项断言，约 45 秒）：编译 Server + `gojob-agent` + `gojob-demo`，一个 agent（带脚本库、tag=executor）与一个 Go 业务 worker 同时挂在一个项目下，验证 ① 脚本库里的脚本既能用短名 `shell` 也能用官方类全名当处理器信息来跑，任务参数按位置参数进脚本，stdout 进 INFO、stderr 进 WARN；② **内联脚本文本默认被拒**，失败原因里点名 `GOJOB_AGENT_ALLOW_INLINE_SCRIPTS`，改配置重启后确实能跑（并且启动时打了告警）；③ 非 0 退出码原样进结论、stderr 尾部被带回；④ `SPECIFY tagEquals:executor` 把任务锁在 agent 那一跳（用反向证据确认：没有第二条链路能让 demo worker 报出脚本里的句子），并如实记录"失败重试会清空预选、换节点重跑"这个既有行为；⑤ `EXTERNAL` 被两个 worker 都明确拒绝且报错点名 JAR 动态容器；⑥ 停止实例杀掉**整个进程组**（脚本自己起的后台孙子进程也一起没了，不是只杀 `/bin/sh`）；⑦ 导入老版任务包时 `SHELL` 任务被改写成 `BUILT_IN` + 官方脚本处理器（脚本正文搬到任务参数）并**真的能被 agent 跑起来**，`PYTHON`/`EXTERNAL` 逐条带原因被拒且不落库。默认 sqlite，端口 `7881~7883`，可用 `GO/PORT/APP/PW/SECRET` 覆盖。

`scripts/verify-register-encryption.sh`（**自己起进程**的注册口令加密实机验收，47 项断言，约 2 分钟）：按"未启用 → 已启用可明文 → 已启用强制"三档滚动重启同一个 Server，逐项验证 ① 未启用时明文注册与 `authApp` 行为不变、`/worker/challenge` 直接点名 `GOJOB_WORKER_AES_KEYS` 拒绝；② 加密注册换到的票照常能心跳、领任务、跑成功，注册体里搜不到口令明文；③ **同一个注册包重放被拒**（换副本也拒，一次性记录在库里）、把密文挪到别人的 `instance_id` 上冒用被拒、**同一口令两次注册的密文不同**；④ 两侧 key 不一致时报 `unknown key id` 且文案不含"password"（不去怀疑口令）、未知 `auth_scheme` 被拒；⑤ 强制档下明文回 **428** + 一张现领的 challenge，用它一次往返即成功，而这张票同样只用一次；没配 key 的 worker 启动即失败并指向 `worker.Config.AesKey`；⑥ `authApp` 三档同上，加密换来的兼容票读得到任务列表；⑦ **Go 与 Java 两个真 worker 进程各自走完整握手上线**（Java 侧还先跑完单测：用 Java 解开 Go 产出的黄金密文）；⑧ 整轮服务端日志里搜不到项目口令。手工构造请求体用 `scripts/register-seal`（与服务端同一份线格式实现，不用 SDK 自己证明自己）。默认 sqlite，端口 `7795`，`JAVA=0` 可跳过 Java 段。

`scripts/verify-map-broadcast.sh`（**自己起进程**的多任务执行类型实机验收，102 项断言，约 30 秒）：Server + 2 个 Go worker，把"一个实例一棵任务树"真跑一遍——① `BROADCAST` 恰好每台在线节点一份、两分落在两个不同节点上（预选==实际执行节点）、层次号 `0.1`/`0.2`，根任务自己不占节点；② 广播落在**零个**节点上时当场收敛，不会留一个永远等不到回报的实例；③ `MAP` 由根任务运行期派生子任务、实例任务数随之从 1 涨到 4（控制台"任务进度"列读的就是它），并支持多级派生（`0.1.2` 这样的层次号）；④ `MAP` 下任一子任务失败即实例失败，而 `MAP_REDUCE` 下 reduce 兜住部分失败就判成功——同一份"坏第 2 片"的输入走出两种结论，这才是两种类型的分界；⑤ `MAP_REDUCE` 的 reduce（`OMS_LAST_TASK`，层次号 `9999`）只在树排空后才补出来，它的返回值就是实例结果；⑥ 停止实例时未派发的当场终止、运行中的协作退出，而且**不会**再补一个 reduce 任务；⑦ 父任务被重派后，旧一代未收敛的后代置 `DISCARDED`（不重复执行副作用），新一代照常派得出来；⑧ `STANDALONE` 实例里调 `map` 被明确拒绝；⑨ 实例详情接口的 `Task 统计` 与那句 `select * from task_info where … limit 10`：白名单写法（含兼容协议的 `status` 序号）照常工作，`; drop table` / `or 1=1` / 注释符 / `union select` 全部被拒且**弹窗不白屏**（照常返回实例信息 + 拒绝原因）。默认 sqlite，端口 `7891~7893`，可用 `GO/PORT/APP/PW/SECRET` 覆盖。

`scripts/verify-capacity.sh`（**自己起进程**的容量实机验收，约 2 分钟/200 节点）：一台机器上用 Go SDK 起 `NODES` 个真 worker 节点（注册/换票/长轮询/回报全走 SDK 原路径），等在线数到位后连打 `ROUNDS` 轮 `BROADCAST`，逐轮打出派发延迟分位数与服务端开销（空闲态与压完态各一次），并把 SDK 侧的注册退避次数一并报出来。**脚本故意不对数字做门限判断**——阈值该由实测反过来定，不是先定再调；具体数字见「容量实机数字」节。节点发生器是 `cmd/gojob-loadgen`（`-nodes/-rounds/-concurrency/-work/-hold/-lead/-idle/-sdklog`，`-idle` 用来只测在线开销不触发）。默认 sqlite，端口 `7877`，`NODES/ROUNDS/CONC/PORT/DSN/LEAD` 可覆盖（跑 200 节点需要 `ulimit -n` ≥ 4096）。

## 配置

优先级：**`GOJOB_*` 环境变量 > 命令行 flag > 默认值**。完整清单见 `.env.example`。

| 变量 | flag | 默认 | 说明 |
| --- | --- | --- | --- |
| `GOJOB_ADDR` | `-addr` | `:7800` | HTTP 监听地址 |
| `GOJOB_DB_DRIVER` | `-driver` | `sqlite` | `sqlite` \| `mysql` \| `postgres` |
| `GOJOB_DB_DSN` | `-dsn` | `./gojob.db` | 各驱动的 DSN，格式见下「数据库 DSN 格式」 |
| `GOJOB_ADMIN_USER` | — | `admin` | 首次启动播种的管理员（仅 `user_infos` 为空时） |
| `GOJOB_ADMIN_PASSWORD` | — | `admin123` | 管理员密码（bcrypt 存储） |
| `GOJOB_PUBLIC_URL` | `-public-url` | 自动推导 | 本副本的**对外可达地址**：随心跳下发给 worker 作为长轮询目标（`cluster_hint`），同时在统计总览页展示。留空按监听地址推导（`0.0.0.0` 会换成本机出口 IP）；多副本部署时每副本各填自己"worker 打得通"的地址 |
| `GOJOB_CLUSTER_SECRET` | `-cluster-secret` | 空 → 本进程随机 | **令牌签名密钥**。多副本必须各节点配同一个值，否则跨节点互不认票（登录随机掉线、worker 随机 401）；留空只适用于单副本，启动日志会打 WARNING。轮换 = 所有在野令牌即刻失效。建议 `openssl rand -hex 32` |

| `GOJOB_WORKER_AES_KEYS` | `-worker-aes-keys` | 空 → 不启用 | **注册口令加密的 AES-256 key**（64 位 hex，逗号分隔多把只为轮换期并存）。配了它，注册走"一次性 challenge + AES-256-GCM"，项目口令不再明文上链路。多副本必须各节点配同一套；建议 `openssl rand -hex 32`。见「注册口令加密」 |
| `GOJOB_WORKER_AES_REQUIRED` | `-worker-aes-required` | `false` | 在上一行的基础上进一步要求**所有**注册都必须加密，明文注册直接拒（`428` + 一张现领的 challenge）。前提是所有 worker 都已配 key，否则它们全注册不上 |

| `GOJOB_RETENTION_DAYS` | `-retention-days` | 空 → 永不清理 | **历史实例保留天数**（运维必配）。终态实例超过它就连同子任务、任务日志一起删；两边都不配则三张历史表只增不减。见「数据保留与容量」 |
| `GOJOB_DB_MAX_OPEN_CONNS` | `-db-max-open-conns` | mysql/pg 20 · sqlite 10 | 数据库连接上限。不设等于无限，worker 惊群拉单时会一路新建连接并顶到 MySQL 默认 `max_connections=151`。多副本估算见下 |
| `GOJOB_DB_MAX_IDLE_CONNS` | `-db-max-idle-conns` | 跟随上面的最大值 | 空闲连接保留数 |

### 数据库 DSN 格式

`-driver` 决定 DSN 怎么解析，三种驱动写法不能混用：

| 驱动 | DSN 示例 |
| --- | --- |
| `sqlite` | `./gojob.db`（文件路径，不存在会自动创建；整段 DSN 里没有 `_pragma` 时 Server 自动补 `busy_timeout(5000)`、WAL、`_txlock=immediate`，自己写就要三项写全） |
| `mysql` | `gojob:gojob123@tcp(127.0.0.1:3306)/gojob?charset=utf8mb4&parseTime=True&loc=UTC` |
| `postgres` | `postgres://gojob:gojob123@127.0.0.1:5432/gojob?sslmode=disable`，或关键字写法 `host=127.0.0.1 port=5432 user=gojob password=gojob123 dbname=gojob sslmode=disable` |

- MySQL 必须带 `parseTime=True`，否则 `DATETIME` 读不回 `time.Time`；链路加密加 `tls=true` 或 `tls=skip-verify`。
- MySQL 的 `loc` 建议固定 `UTC`，别用 `Local`：`DATETIME` 存的是不带偏移的字面量，`loc` 决定按哪个时区读写它。`loc=Local` 会让存储跟着容器 `TZ` 漂——容器从 UTC 切到东八区时，存量行会被按新时区重读、整体偏 8 小时。固定 `UTC` 后库里永远是 UTC 时刻，日志/cron/前端再靠容器 `TZ=Asia/Shanghai` 显示北京时间，切换无损（SQLite 存带偏移的时刻，本就无损）。已有 `loc=Local` 且一直在 UTC 容器里跑的库，把 DSN 改成 `loc=UTC` 即可，存量不用迁。
- PostgreSQL 本机/容器内用 `sslmode=disable`，托管库按服务商要求用 `require` / `verify-full`；URL 写法里口令含特殊字符要先百分号编码，或改用关键字写法。
- `pgsql` / `postgresql` 等同 `postgres`，驱动留空按 `sqlite`。
- compose 用 `--profile mysql` / `--profile postgres` 时，主机名要换成服务名：`mysql:3306` / `postgres:5432`。例如 `.env` 里写 `GOJOB_DB_DRIVER=mysql`、`GOJOB_DB_DSN=gojob:gojob123@tcp(mysql:3306)/gojob?charset=utf8mb4&parseTime=True&loc=UTC`。

Worker 侧示例进程读取：`GOJOB_WORKER_SERVER` / `GOJOB_WORKER_APP` / `GOJOB_WORKER_PASSWORD`，以及 `GOJOB_WORKER_AES_KEY`（与服务端某一把 key 一致即可，两侧只配 key、不配 kid）。零代码执行节点 `gojob-agent` 另有 `GOJOB_AGENT_*` 六个变量（脚本库、内联开关、并发、工作目录、超时），清单与安全含义见 `worker/README.md`「零代码执行节点」与 `.env.example`。

## API 概览

三组接口，均返回 `{code, message, data}`（`code=0` 为成功）。

**控制台 `/openapi/*`**（除 login 外需 `X-Gojob-Token` 头）
`user/{login,logout,me,changePassword,list,create,modify,resetPassword,status,delete}` · `app/{create,modify,delete,list,workers}` · `worker/acceptTasks`（停/启某台节点接单，见「停止某台节点接单」）（`app/list` 每行带 `current_server` = 当前调度该项目的副本 `node_id`，列名沿用兼容协议的 currentServer） · `job/{create,modify,delete,get,list,operate,import,export,previewTime}`（`job/list` = `index`(0 基) + `page_size` + `job_id` + `keyword`，返回 `PageResult`；`job/export` 回通用任务包（JobDTO 数组/对象）；`job/operate` 的 `trigger` 可带 `instance_params`＝参数运行） · `instance/{list,get,tasks,logs,stop,detailPlus}`（`detailPlus` = 实例详情弹窗：实例行 + `Task 统计` + 一页子任务，`custom_query` 只认白名单语法） · `stats/overview` · `audit/{loginLogs,operationLogs}` · `system/nodes`（节点监控：副本指标 + worker 指标 + 项目调度归属表，`ADMIN`/`SUPER_ADMIN`）

**Worker 数据面 `/worker/*`**（gojob 自有协议，gojob SDK 使用）
`register`（用 `app_id + password` 换取 worker token，**数据面唯一需要口令的端点**；配了注册加密后口令改由 `challenge + kid + iv + ciphertext` 送上）· `challenge`（注册握手第一步：领一张 30s 过期、用过即废的凭证，未启用加密时明确拒绝）· `heartbeat` `fetchTask`（长轮询认领）`report`（回报，幂等）`reportLogs`（批量日志，去重）`mapTask`（`MAP`/`MAP_REDUCE` 的处理器运行期派生子任务）`taskResults`（reduce 分页取回子任务结果）——除 `register`/`challenge` 外这六个只带 token，不再携带项目口令。`mapTask` 与 `report` 同样校验任务归属（只有正在执行该任务的节点能为它派生/改判），`fetchTask` 与 `heartbeat` 的响应携带 `cancel_task_ids`：pull 模型没有反向连接，停止实例等下行指令只能靠响应夹带下发。
token 是**无状态签名票**（HMAC-SHA256，密钥 `GOJOB_CLUSTER_SECRET`），与签发时的 `app_id + instance_id` 绑定，有效期 10 分钟；因为票里带着"按哪个吊销基线签发"，任何副本都能独立校验，改项目口令/删项目会把该项目此前签发的全部票即刻作废（库里 `token_epoch` +1）。SDK 在有效期过半时主动重注册换票，遇 401（重启、密钥轮换、口令变更）也会自动重注册并重放当次请求，业务侧无感。

**OpenAPI 兼容层 `/openApi/*`**（对接外部调度平台的 4.x 客户端/OpenAPI，注意大小写）
`authApp`（`password` 明文，或 gojob 自己的加密握手 `authScheme=aes-gcm`；`encryptedPassword`=md5 形态按设计拒绝）· `challenge`（握手下票，`principal` 为空——调用方没有实例身份）· `saveJob` `fetchJob` `fetchAllJob` `queryJob` `exportJob` `copyJob` `disableJob` `enableJob` `deleteJob` `runJob` `runJob2` `stopInstance` `retryInstance` `fetchInstanceStatus` `fetchInstanceInfo` `queryInstance`（Workflow 系列返回「不支持，规划 M5」）

## 任务执行日志

目标：**Worker → Server 的日志上报基本不丢**。机制（Go / Java SDK 与 Server 三端一致）：

1. **每任务单调 `seq`**：从 1 递增，Server 侧 `instance_task_logs` 建 `UNIQUE(task_id, seq)`，插入用 `ON CONFLICT DO NOTHING` → 天然去重。
2. **确认后再出队**：Worker 批量发送，仅当 Server 返回 `code=0` 才把该批移出队列；失败则恢复到队首，指数退避无限重试（至少一次）。
3. **先刷日志、再回报结果**：任务执行结束先 `flush` 日志通道，成功后才 `report`，避免结果先到、日志丢失。
4. **有界缓冲**：队列上限 50000 条，超限丢最旧；优雅下线时做最后一次 flush。

至少一次投递 + 幂等去重 ⇒ 有效不丢、不重。控制台在「实例 → 日志」页可按 task 查看、自动跟随最新。

## 账号、权限与审计

> 本节是 gojob 的**自研能力**：三级角色 + 审计留痕（兼容目标只有应用级隔离，无 RBAC / 审计）。控制台「系统管理」菜单进入。

**角色（三级）**

| 角色 | 权限 |
| --- | --- |
| `SUPER_ADMIN` | 全部：系统管理（用户增删改、授权）、项目增删改、导入、任务读写 |
| `ADMIN` | 在 `AppScopes` 授权的项目内增删改任务/实例；不可管理用户与项目 |
| `READONLY` | 只读浏览，任何写操作被拒（后端 `401`，前端隐藏写按钮） |

- 首启按 `GOJOB_ADMIN_USER/PASSWORD` 播种一个 `SUPER_ADMIN`（仅 `user_infos` 为空时）。
- 改密：已登录用户在右上角「修改密码」自助修改（校验旧密码），成功后**旧 token 立即失效**需重登；超管可「重置密码」强制他人生效。
- 禁用 / 删除账号即时吊销其全部会话；被禁用账号无法登录。**最后一名启用的超管不可被禁用 / 删除 / 降级**，防自锁。
- 写接口鉴权集中在 `authorize` 中间件；写操作与登录（含失败原因）分别落 `operation_logs`、`login_logs`，参数中的 `password/old_password/new_password` 自动脱敏为 `***`。
- **ADMIN 的项目授权以"实物"为准，不信客户端自报**：判定用的 `app_id` 由服务端按请求里的 `job_id` / `instance_id` **反查库**得到（查不到才回退到请求体/查询串里的 `app_id`），所以一个只被授权 `app1` 的账号，即使把 `app_id` 写成 `app1` 也停不掉 `app2` 的任务——被拒的同时那条任务不会被改动。这条是后补的修正：早期 `app_id` 是在中间件 `c.Next()` **之后**才写进 context 的，`authorize` 永远读到空串，结果是「项目授权」对 ADMIN 完全不生效（每个写操作都被拒成 `no permission for app `），现在由 `server/api/scope_test.go` 用两组用例钉住（授权项目内可写、跨项目被拒且不留痕）。

**口令存储与安全边界**

- **用户密码**：bcrypt 摘要（`user_infos.password`，`json:"-"` 不外泄）。
- **项目（App）密码**：库里只有 bcrypt 摘要（`app_infos.password`，`json:"-"`），**不存在明文，也不存在任何"明文等价物"**。曾经为兼容 `authApp` 保留过 `password_md5 = lower(md5(明文))` 一列，该列已删除——md5 摘要对冒用者而言与明文无异，安全位阶高于协议兼容（代价见「兼容性说明」）。
- 老库升级：进程启动时 `auth.UpgradeAppPasswords` 把仍是明文的 `app_infos` 原地升级为 bcrypt（幂等，日志会打印升级条数）；MySQL/PG 存量库可手动 `ALTER TABLE app_infos DROP COLUMN password_md5;`（不删也不影响运行，只是留了一份没人读的旧摘要）。
- **Worker 数据面**：`POST /worker/register` 用 `app_id + password` 换一张 token，之后 `heartbeat`/`fetchTask`/`report`/`reportLogs` 四个端点**只带 token、不再带口令**（口令从"每个请求都出现"收缩为"只出现在注册那一刻"）。token 与签发时的 `app_id + instance_id` 绑定：拿着别人的 token 既领不到别人的任务、也改判不了别人的任务结果（服务端按 `identity mismatch` 记日志并回 401）；日志上报还会校验任务归属本 app；`report` 只接受**认领该任务的那台 worker** 的回报，越权/迟到回报被静默忽略（防同项目内互相改判、防失联 worker 复活污染已重派任务）。
- **换项目口令 = 立即踢人**：`app/modify` 改口令或 `app/delete` 删项目会吊销该项目全部 token，老节点必须用新口令重新注册才能继续收单（因此**改口令后需把 worker 的配置同步更新并重启**，否则它会在日志里持续报 `register rejected: wrong password`）。
- token 是**无状态签名票**（默认 10 分钟有效，无 DB 表、无控制台管理页）：SDK 在有效期过半时主动重注册换票，所以存活节点永不掉线；服务端重启不再让节点重新注册（票仍然验得过），换项目口令/删项目才会让它即刻失效。
- 边界（不粉饰）：① **注册口令可选加密**（见下一节「注册口令加密」，配 `GOJOB_WORKER_AES_KEYS` 后口令不再明文上线），但 token、任务参数、执行日志照旧是 HTTP 明文内容 ⇒ 整体仍需置于内网或前置 TLS；② 能注册（= 知道项目口令）就能收单，多节点共享同一份项目口令，更细的授权（按任务/按标签限流）不在一期范围；③ **token 一旦被窃取，在有效期内可冒用该节点身份**（只能用它绑定的那个 `instance_id`，不能换身份），且当前只能按项目整体吊销（改口令）不能单票吊销——所以 worker 主机本身仍按持有长期凭据对待。

## 注册口令加密

> 目标只有一个：**让项目口令本身不出现在链路上**，并且让抓到一个注册包的人无法复用它。
> 它**不是传输加密**——把这段当成"开了加密就安全了"是误用，见本节末尾的边界。
> 默认关闭（不配 key 时行为与引入本功能前完全一致）。

**握手三步**（`/worker/register` 与兼容协议的 `/openApi/authApp` 同一套口径，只差身份字段）：

```
1. POST /worker/challenge   {app_id, instance_id}          → {challenge, expires_in, schemes}
2. 本地 AES-256-GCM 加密 {v, app_id, instance_id, challenge, password}
3. POST /worker/register    {…, auth_scheme:"aes-gcm", kid, iv, challenge, ciphertext}
```

三件东西各管一件事，缺一个整套就白做：

| 环节 | 为什么必须有 |
| --- | --- |
| 一次性 `challenge`（服务端现发、30s 过期、**用过即废**） | 固定因子 + 固定 key 会得到确定性密文，那串密文就成了口令的替身，抓到即可永久冒用。消费记录落在 `revoked_tokens` 表的主键上，所以**跨副本也认**：A 副本用掉的票，B 副本不认第二次 |
| AES-**256-GCM**（不是 CBC） | CBC 没有完整性，解出垃圾时分不清"key 错 / 被改过 / 口令错"，把这几类返成不同文案就是 padding oracle 的入口。GCM 的 tag 校验失败只有一个结局，服务端也只回一条含糊文案 |
| `AAD = 版本 + kid + app_id`，明文中再带 `app_id`/`instance_id`/`challenge` | 防"拿 A 项目算好的密文配着 B 项目的表头来注册"、防把别人的注册包挪到别的 `instance_id` 名下冒用 |

密钥派生用 HKDF-SHA256，`salt` 里带 `app_id` ⇒ **同一把全局 key 在不同项目下派生出不同的 AES key**，跨项目冒用没有可乘之机。`kid` 由 key 单向派生（sha256 前 4 字节），两侧只配 key 就能互认，写不出错配的 kid。

**三档配置与升级顺序**（"是否加密"由人员两侧配置决定，程序不自动协商降级）：

| 服务端 | 客户端 | 结果 |
| --- | --- | --- |
| 未配 key | 任意 | 明文口令注册（现状不变）；`/worker/challenge` 直接拒绝并点名 `GOJOB_WORKER_AES_KEYS` |
| 配 `KEYS`、未开 `REQUIRED` | 配了 key | 走加密握手 |
| 配 `KEYS`、未开 `REQUIRED` | 没配 key | 仍走明文 ⇒ 这一档专门用于**分批升级**：先把服务端配好、逐个把 worker 换成配 key 的，最后再开强制 |
| 开 `REQUIRED` | 没配 key | 明文被拒，返回 **`428`**（不是 401）+ 一张现领的 challenge。用 401 会让 SDK 把它当"票过期"无限重注册；客户端按提示一次往返即可改加密。worker 侧的报错直接指向 `worker.Config.AesKey` / `gojob.worker.aes-key`，不去怀疑口令 |
| 配 `KEYS` | 配了另一把 key | 拒，并回 `unknown key id（服务端接受 <kids>）`——错配要说清楚是谁没对齐 |

**配了 key 就一定加密**：即使服务端没启用，也是**注册失败 + 指出服务端没配**，不会静默退回明文。静默降级等于给链路上的中间人一个"抹掉加密"的开关（它只要挡掉 `/worker/challenge`）。所以升级顺序固定为：**先配服务端 → 再全量配 worker → 最后开强制**。

`GOJOB_WORKER_AES_KEYS` 的运维等级与 `GOJOB_CLUSTER_SECRET` 相同：拿到即可自签注册包冒注任意 worker，轮换会让所有未同步的 worker 注册失败。因此**只放 env / flag，不进数据库、控制台也没有输入框**（服务端要能解密，就意味着库里那份是"知道即可冒用"的凭据等价物——与砍掉 `password_md5` 列同一个判断）。多把 key 只为轮换期并存，新旧 worker 可同时在线。

边界（不粉饰）：

- 它保护的是**口令这一个字段**的一次性传输。token、任务参数、实例参数、执行日志仍是 HTTP 明文内容；链路加密要靠内网或前置 TLS。
- 挑战/密文本身仍会被看到。抓包者解不出口令，但**能知道"某个节点在某一刻注册过"**（`app_id`、`instance_id`、`kid` 是明文表头）。
- 服务端只能凭"验不出合法 challenge"拒绝重放，做不到"识别出这是被重放的包"并告警。
- 口令泄露一次（比如运维把它打进了别处日志）仍然等于该项目被冒名，之后只能改口令 + 重启 worker。

跨语言口径由测试钉住：`internal/registercrypto` 的黄金向量（用 Node 的 OpenSSL 独立算出）同时被 Go 与 Java 单测断言，Java 侧还直接用 Go 产出的密文解密验证；两端 SDK 各自跑真进程的实机验收见 `scripts/verify-register-encryption.sh`（47 项，含强制档、重放、错配、Go/Java worker 上线）。

## 节点监控

> 同样是**自研能力**（兼容目标的控制台只有首页时间卡与项目下的 worker 机器表，没有进程级指标页）。入口：控制台「系统管理 → 节点监控」，接口 `GET /openapi/system/nodes`（`ADMIN` / `SUPER_ADMIN`）。

**两张表，两条上报链路**

| 对象 | 上报方式 | 存储 | 间隔 |
| --- | --- | --- | --- |
| 调度中心副本（gojob-server 进程） | 进程内 `node.Collector` 采样 → `node.Publisher` 按 `node_id` upsert `server_nodes` | DB（曲线保留最近 120 个点 ≈ 10 分钟） | 5s |
| Worker 节点（Go / Java SDK） | 随 `heartbeat` 的 `WorkerInfo` runtime 字段带上来 | DB（与在线节点同一行，`worker_nodes` 表） | worker 心跳周期 |

多副本各写自己那一行，因此列表看到的是**整个调度中心集群**的每个进程，而不是"处理本次请求的那个副本"——这也是它必须落库的原因。`node_id = hostname-pid-startNanos`，重启即换新行；查询时顺带清掉静默超过 7 天的行，防止反复重启攒垃圾。判活阈值 15s（3 个采样周期），过期行标红「离线」并显示距今多久。

**采了什么**

- Server 侧：进程 CPU%（`getrusage` 差值，100 = 吃满一个核）、RSS、Go 堆 inuse、协程数、GC 次数与累计暂停（`runtime/debug`，Go 1.27 已删除 `runtime.GCStats`）、`http.Server.ConnState` 统计的活跃/空闲连接、长轮询在途（`/worker/fetchTask` 单独计，它才是连接数居高不下的主因）、在途请求与 QPS。
- Worker 侧：`cpu_percent` / `gc_count` / `gc_pause_ms` / `rss_mb` / `threads`（Go 为协程数，JVM 为线程数）/ `open_fds` / `running_tasks`（= 已占用的并发额度）。Java 走 MXBean（`GarbageCollectorMXBean` + `ThreadMXBean` + `com.sun.management.UnixOperatingSystemMXBean`），三种接入形态共用同一份 `RuntimeMetrics`。

**口径与边界（不粉饰）**

- 取不到的指标报 0，页面渲染成"—"，**不猜数**：RSS 与 `open_fds` 依赖 Linux 的 `/proc`，macOS/Windows 本地开发恒为"—"；JVM 没有可移植的 per-process RSS 取法，Java worker 的 RSS 恒为"—"。
- 曲线只存在 `server_nodes.samples` 这一 TEXT 列里（按列存稀疏数值），是"最近 10 分钟"级别的趋势，不是时序库；要长期留存/告警请抓 `/openapi/system/nodes` 或接 Prometheus（未实现，见 roadmap）。
- Server 指标是**进程级**，不含机器级视角（整机 CPU/磁盘/网络）；worker 侧另有 `cpu_load`/`memory_*` 表达机器与额度。

## 停止某台节点接单

入口：控制台「项目 → 节点」每一行的「停止接单 / 恢复接单」，接口 `POST /openapi/worker/acceptTasks`（`{app_id, instance_id, accept}`）。走控制台会话票 + 该项目写权限 + 操作审计，与"停止实例"是两个不同的动作。

实现就是 `worker_nodes.accept_tasks` 这一列改成 0，生效靠**选节点时过滤**：`registry.Workers` 只返回接单中的节点，所以随机/健康优先/轮询选不到它，广播的节点集合快照也只数它。**worker 侧零改动、也不感知这个开关**（没有新的协议字段，Java SDK 同样不动）：那台节点继续心跳与长轮询，只是认领 SQL 永远筛不到属于它的任务，于是一直空转到超时。

- 在跑的任务不受影响，继续执行并回报；已经在队列里但指名给它的任务，由 `ReleaseStaleTargets`（60s）回收后重选节点；
- 开关在库里，所以**重启 Server 副本、请求打到别的副本、点的时候这台节点正好没心跳**都不会让它溜过去；
- 全部节点都停接单时任务留在队列里等运维恢复，而不是被判成"没有 worker 可用"直接失败（`HasWorkers` 数的是在线节点，不看开关）；
- 业务进程自己要下线，用 SDK 的 `worker.StopReceivingNewTasks()` / Ctrl-C 优雅退出，**不需要也不应该有一个对外开口的端口**。

> 实机数字（同一项目 3 个在线节点、停掉其中 1 台）：广播一次得到 `广播 2 个执行节点`，那 2 份子任务分别落在另外两台身上，被停的那台一份都没拿到；恢复接单后再触发一次，回到 `广播 3 个执行节点`。停掉之后让它再跳两跳心跳（10s），`accept_tasks` 仍是 0——心跳的 upsert 里不含这一列，运维的意图不会被 worker 自己刷回去（`server/registry/registry_test.go` 有这条回归）。

## Server 集群（多副本）

多个 `gojob-server` 进程可以指向同一套库横向铺开。归属模型：**按项目（app）划分调度所有权**，同一时刻只有一个副本调度某个项目的全部任务（兼容目标用 `app_info.current_server` 表达，gojob 用 `app_leases` 租约表达）。

| 关注点 | gojob 的做法 | 代码 |
| --- | --- | --- |
| 谁调度哪个项目 | `app_leases(app_id, holder, epoch, lease_expire, renew_count)`：TTL **10s**、每 **3s** 续一跳；只有"无行 / 已过期 / 持有者是自己"才允许抢占，绝不抢活着的租约 | `server/cluster/lease.go` |
| 接管时延 | 显式可算：最坏 `TTL(10s) + 粗扫(5s)` ≈ 15s 发现，`lease` 一跳(3s) 内接管。优雅退出会主动把 `lease_expire` 写成当下，接管方不必等满 TTL | 同上 |
| 僵尸主（分区后旧主仍在写） | 事务内 **fencing**：触发前先 `UPDATE app_leases SET renew_count=renew_count+1 WHERE app_id=? AND holder=? AND epoch=?`，`RowsAffected != 1` 即回滚本次触发。epoch **只在接管时 +1**，所以"换了主人"这件事永远看得出来 | `lease.go` `Fence` + `scheduler.fire` |
| 装载与触发的三道防线 | ① 粗扫只装载本机持有的项目；② `UPDATE job_infos SET next_trigger_time=? WHERE id=? AND next_trigger_time=?` 的 CAS 让同一计划时刻只有一个副本能消费；③ 上面那道 epoch 围栏 | `server/scheduler/scheduler.go` |
| 令牌跨副本 | 三类票（控制台 / worker 数据面 / OpenAPI 兼容层）都是 **HMAC-SHA256 无状态签名**，任一副本独立验签；吊销走库里的 `token_epoch` 基线 + `revoked_tokens` 单票黑名单 | `server/auth/` |
| Worker 在线节点 | 心跳 upsert 进 `worker_nodes` 表，所以**每个副本看到的候选集一致**（内存表时 LB 后面各副本只看得见轮询过自己的节点） | `server/registry/registry.go` |
| 跨副本唤醒 | `cluster_signals` 单行计数器：入队/停止意图 `seq+1`，各副本 1s 轮询到变化就唤醒本机长轮询。把"别的副本入队 → 我这里的 worker 拿到任务"从最多 5s 压到最多 1s | `server/cluster/signal.go` |
| Worker 连哪台副本 | **数据面不绑归属**：在线表在库里，所以 `/worker/*` 任何副本都能服务。心跳响应顺带下发 `cluster_hint`（`owner` = 该 app 租约持有者的可达地址，`servers` = 在线副本地址列表），worker 只把它当**首选地址**：连续 2 跳指向同一 owner 才迁移，迁过去连不上就退回列表里的其它副本继续跑。因此 worker 侧不需要 LB/VIP，配一份副本地址列表即可自寻址、自容错，且额外 QPS 为 0（兼容目标要为此每 10s 单独发一次 `/server/acquire`） | `server/cluster/routing.go`、`worker/worker.go` `applyHint` |

**部署要点**

1. 所有副本必须配同一个 `GOJOB_CLUSTER_SECRET`，否则令牌跨节点互不认（表现是登录随机掉线、worker 随机 401）。留空则本进程随机生成并打 WARNING——只适用于单副本。
2. 首启的建表/播种建议**滚动执行**：两进程同时对同一套库跑 DDL 会撞锁（sqlite 直接 `SQLITE_BUSY`）。管理员播种已改成撞唯一键即视为"别人播过了"，不会再有副本因此退出。
3. 副本之间不需要时钟同步到毫秒：租约判活比较的是"同一个库里两个时刻的差"，但 `now()` 取各自进程时钟，时钟漂移过大会缩短/拉长实际 TTL。
4. 手动触发（控制台"运行一次"、OpenAPI `runJob`）与依赖触发**不要求归属**：一次请求就是一次事实，任意副本都能接。它们也不推进 `next_trigger_time`，因此不参与双跑判定。
5. `GOJOB_PUBLIC_URL` 除统计页展示外，现在还是**本副本对 worker 的可达地址**（下发进 `cluster_hint`），多副本部署时每个副本必须各自配成"worker 打得通"的地址（`http://<该副本 IP 或域名>:<端口>`），不能填 `0.0.0.0`、也不能填只在集群内可解析的地址。配错不致命：worker 会退回列表里其它副本继续跑（见上表"Worker 连哪台副本"），只是白跳一次。
6. **worker 侧不再需要 LB/VIP**：把各副本地址填进 SDK 的 `ServerAddresses` 即可自寻址、自容错。**浏览器侧仍需要一个稳定 URL**（前端无法故障转移），可以指向任一副本、DNS 轮询或反代；它挂了调度不受影响，只是控制台要手动换地址重进。

**边界（不粉饰）**

- 归属只保证"一个计划时刻只生成一个实例"，不保证"同一时刻只有一个副本能写库"——围栏只覆盖触发这条写路径。管理面写操作（建任务/改任务）仍靠数据库行级约束，两个副本同时改同一行是后写覆盖。
- 租约不是共识协议：没有 quorum，也就无法防"库里 `now()` 被时钟回拨"这类病态场景。兼容目标同样没有 fencing（它的选举是先到先得 + 反向 PING 判活），gojob 用 TTL + epoch 把最常见的分区双跑堵死，不是做到严格一次。
- 集群密钥是**共享长期凭据**：拿到它就能自签任意身份的票（含 SUPER_ADMIN），必须与数据库口令同等对待；轮换会让所有在野令牌即刻失效（用户重登、worker 自动重注册）。
- 入队信号是尽力而为：`Bump` 失败只记日志，最坏退化成长轮询自身的 ≤5s 兜底重查，不会丢任务。

## 数据保留与容量

调度中心的三张表只增不减，长期运行必须给它们装闸门：`job_instances`（每个实例一行）、`instance_tasks`（每个实例至少一行，`MAP`/`BROADCAST` 会成百上千）、`instance_task_logs`（通常是最大的一张，处理器每输出一行就是一条）。

两道闸门，任务上配的优先：

| 配置 | 生效范围 | 语义 |
| --- | --- | --- |
| `GOJOB_RETENTION_DAYS`（或 `-retention-days`） | 全部任务 | 终态实例超过这个天数即回收；顺带回收"任务已被删除"的孤儿实例 |
| 任务的 `life_cycle` | 该任务 | 优先级更高。配了 365 天的任务不会被 30 天的全局值提前删掉 |

**两边都不配 = 永不清理**（存量部署升级后不会突然少数据），此时启动日志会如实打一条 `GOJOB_RETENTION_DAYS 未配置且任务未配 life_cycle ⇒ 历史实例与任务日志将无限增长`。

删除的行为特征：只动**终态**实例（成功/失败/已停止），运行中、停止中、等待派发的一律不碰；每 10 分钟扫一轮，一轮里每个任务最多删 25 批、每批 20 个实例，一个短事务内按"日志 → 子任务 → 实例"顺序级联删掉，绝不做大事务——sqlite 是单写者，MySQL 上的长事务会卡住热路径的任务认领。剩下没删完的下轮接着删，收敛即可。多副本部署时每个副本只清理**自己持有归属**的项目，不会 N 个副本抢同一批行做 N 倍无用功。

注意边界：`life_cycle` 目前只能经 OpenAPI 设置，**建单表单里还没有这一项**（连同其余几处表单差距一起记在 `CHANGELOG.md` 的 `[Unreleased]` 一节）。

## 容量实机数字

"支持多少 worker"这种话不用纸面换算，用 `scripts/verify-capacity.sh` 压：一台机器上用 SDK 起 N 个**真** worker 节点（各自注册、各自换票、各自长轮询、各自回报，只有处理器什么都不做），任务是一个 `BROADCAST`（一份/节点），量"从发出触发请求到该节点任务开跑"的派发延迟。**这是单机的联合数字**——Server 与 N 个节点挤在同一台机器上抢同一批核，生产上它们分开部署时两侧都比这里宽裕。

复现（macOS，10 核 / 24GB，sqlite 单文件 WAL；`NODES`/`ROUNDS`/`CONC`/`DSN` 可覆盖）：

```
NODES=200 ROUNDS=5 scripts/verify-capacity.sh
```

200 节点在线、一次广播 200 份，五轮全部 200/200 收齐：

| 指标 | 实测 |
| --- | --- |
| 派发延迟 p50 | 59–69ms |
| p90 | 90–99ms |
| p99 | 101–138ms |
| 单轮最大 | 114–152ms |
| 一轮 200 份全部开跑 | 114–162ms |
| 200 节点只挂机不触发 | Server 进程 CPU 3.7%、堆 14.1MB、协程 417、TCP 连接 200、长轮询在途 200 |
| 节点全部断开后 | CPU 0.3%、协程 13–17 |

同一台机器上 20 个节点时 p50 是 **5ms**、100 个节点时 **23ms**——延迟随节点数近乎线性，斜率约 **0.3ms/节点**（口径含管理面往返，脚本每轮会单独打出 `trigger_http`，要净派发延迟就减掉它）。

**这条斜率就是瓶颈本身**：pull 模型下每份任务的认领都是一次写事务（`WAITING → RUNNING` 的条件 UPDATE），而 sqlite 只有一个写者，200 份任务只能排队过独木桥，0.3ms 就是单条写事务的排队成本。要把这段压下去，换 MySQL / PostgreSQL（行级锁，认领可并行），别改协议。

**一个 worker 同时跑 200 个任务**：这取决于**每个节点自己的并发额度**，不是节点数。Go SDK `worker.Config.MaxConcurrency`（默认 **16**）、Java `maxConcurrency` / `gojob.worker.max-concurrency`、零代码节点 `GOJOB_AGENT_MAX_CONCURRENCY`。拉取时 SDK 把 `max_task_num` 报成"当时还剩多少额度"，服务端对这个数**没有上限**，所以一次长轮询的响应就能带回 200 条任务，`MaxConcurrency=200` 的节点当场起 200 个执行体。实测（同一个 Server、**1 个节点**、一个 `MAP` 实例派生 200 个子任务全落在这台节点上）：

| 场景 | 并发额度 200 | 并发额度 16（默认） |
| --- | --- | --- |
| 只量派发（任务不做事） | p50 20–23ms，200 份全部开跑 62–134ms | 修复前 max **11–12s**，修复后 94–160ms |
| 每个任务真跑 50ms | 200 份全部开跑 ≤30ms，整轮 74–88ms（串行要 10s） | 整轮 830–924ms ≈ 200/16 × 50ms |

额度不是免费的：一个执行体一条回报。200 并发适合 IO 等待型任务；CPU 密集型的天花板是核数，额度开到核数以上只是把排队从服务端挪进进程里。Java 侧是固定大小线程池（`maxConcurrency` 条线程），Go 侧是信号量 + 每任务一条 goroutine，所以同样开 200，Go 节点便宜得多。

复现：`NODES=1 CONC=200 SPLIT=200 EXEC=MAP ROUNDS=5 scripts/verify-capacity.sh`（`WORK=50` 给每个任务加 50ms 业务耗时）。

已定位并修掉的四个坑（都在 200 这一档才暴露）：

- **长轮询丢唤醒**：`Fetch` 原先先 `claim()` 再取唤醒 channel，两步之间到达的那次唤醒会被丢掉（channel 已被 close 并换掉），只能等 5s 兜底复查——表现为个别轮次里一整批任务统一迟到约 1 秒。现在先取 channel 再认领（close 过的 channel 永远可读，不会漏也不会多等）。
- **心跳齐射**：心跳是 worker 侧唯一高频写库的请求。整池节点同时冷启动后相位一致，每 5s 一起写，实测把单条心跳 upsert 顶到 237–338ms（服务端 `SLOW SQL` 直接打出来了），同一条写通道上的任务认领被排到 1 秒开外。SDK 的心跳间隔现在带 ±25% 相位抖动（Go / Java 两侧同步改），齐射摊成匀速。
- **满载时死睡 1 秒**：SDK 在"并发额度已满"时不发拉取，旧实现是固定睡 1 秒再看一眼 ⇒ 一台 16 额度的节点消化 200 条任务要白等 12 次秒级睡眠（实测整轮 11s）。改成**任务一释放额度就唤醒拉取循环**（1s 只留作兜底），同一场景降到 94–160ms；顺带让"额度占满时停止指令送达更慢"这条既有边界收紧了一档。Go / Java 两侧同改。
- **注册风暴会永久吃掉节点**：注册是整链路唯一要过 bcrypt 的请求（200 次并发 ≈ 12 秒 CPU），本机 `kern.ipc.somaxconn=128`，队列一满后到的连接直接 reset。旧行为是 SDK 就此退出——那部分节点**永久不上线**，而其余节点跑得好好的，最难发现的一类故障。现在"连不上"退避重试（200ms/1s/2s/5s + 抖动，2 分钟止损），"服务端明确拒绝"（口令错、要求加密注册、服务端没配 key）仍然立即失败。实测每轮有 0–24 个节点靠重试补上线，最终 200/200。生产上请同时抬高内核 accept 队列（Linux `net.core.somaxconn`、`nginx`/SLB 的 backlog），注册风暴只在冷启动那一刻出现。

还没吃掉的账：`wakeLocal` 会唤醒本进程**所有**长轮询（不分项目），单副本挂几千节点时一次触发会让所有节点各跑一次认领；每节点并发额度默认 16。这两项在 200 这一档都还没到压力点，暂不为此加复杂度。

## 连接池

Server 启动即显式设置连接池，不留 `database/sql` 的默认（`MaxOpenConns=无限` + `MaxIdleConns=2`）——默认值意味着上千 worker 惊群拉单时会一路新建 TCP 连接，一路顶到 MySQL 的 `max_connections`（默认 151）后开始报错，同时把握手成本摊进请求延迟。

| 驱动 | 最大连接 | 空闲保留 | 单条最长存活 |
| --- | --- | --- | --- |
| mysql / postgres | 20 | 20 | 30min |
| sqlite | 10 | 10 | 30min |

用 `GOJOB_DB_MAX_OPEN_CONNS` / `GOJOB_DB_MAX_IDLE_CONNS` / `GOJOB_DB_CONN_MAX_LIFETIME`（或同名 flag）调。多副本估算：**副本数 × 最大连接 ≤ 数据库 `max_connections` − 预留**（DBA 自己的连接、其他消费方）。3 副本 × 20 = 60，对默认 151 是安全的。数据库侧做过主备切换、或中间件会定期回收空闲连接时，把 `GOJOB_DB_CONN_MAX_LIFETIME` 调小（如 `5m`）能更快甩掉在野的陈旧连接。

## 兼容性说明

gojob 与 [PowerJob](https://github.com/PowerJob/PowerJob) **无隶属、无背书关系**，但控制面刻意与它的 4.x/5.x 接口语义对齐——既有的任务包、OpenAPI 调用方和运维习惯可以平移过来。下面逐条写清哪里对齐、哪里刻意偏离、为什么；下文把那个项目简称**兼容协议**。

- **Worker 数据面**：gojob 用自己的 HTTP 长轮询协议（`/worker/*`），不复刻兼容协议 Worker 的 Akka/PPTime 二进制协议。控制台、OpenAPI、任务/实例数据模型与字段对齐兼容协议的 4.x。数据面凭证也是自有设计：先 `register` 换 worker token，其余请求只带 token（兼容协议是每次请求带口令），详见「Worker 数据面」节。**这是破坏性协议变更**：旧版只带口令的 worker 会被回 `401`，需升级到含自动注册的 Go/Java SDK。
- **OpenAPI `authApp` 的口令形态（安全优先的刻意偏离）**：gojob 请求体为 `{appName, password}`，服务端用 bcrypt 校验；**不**支持兼容协议官方 SDK 的 `encryptedPassword=md5(明文)` + `encryptType=1` 形态，这类请求会被明确拒绝并提示原因。原因：校验 md5 摘要要求服务端存下同一份摘要，而 md5 值与明文等效（拿到即可冒用），等于库里存明文；gojob 选择只在库里保留 bcrypt 摘要。接入方把客户端配成提交明文口令即可（并部署在内网 / 前置 TLS）。另外 gojob 在这一层也提供自有的**加密握手**（`authScheme=aes-gcm` + `kid/iv/ciphertext/challenge`，见「注册口令加密」），不想让口令上线的调用方可以用它——它是 gojob 扩展，兼容协议客户端不会发这个字段，因此不影响既有明文接入。
- **无工作流**：一期不支持 Workflow。任务间的前后依赖用轻量的**依赖触发**表达（见下条），不引入 DAG 引擎。
- **依赖触发（gojob 扩展，时间类型 `DEPENDENCY`）**：任务的调度方式除 cron/频率/API 外，还可以是「等某个上游任务执行成功」，由 `job_info.upstream_job_id` 指定单个上游任务，上游实例**成功**收尾后自动为下游创建一个实例（`source=DEPENDENCY`、`job_instance.upstream_instance_id` 记录来源实例）。上游失败或被停止不传导；幂等靠 `(job_id, upstream_instance_id)` 唯一索引，回报重放/服务重启都不会重复拉起下游；保存期禁止自依赖、跨项目依赖与成环，且被依赖的上游任务不允许删除。兼容协议用 Workflow(DAG) 表达同一需求，gojob 不实现 DAG，只做「单上游、成功即触发」这一最高频形态。
- **执行类型 `BROADCAST` / `MAP` / `MAP_REDUCE` 已实现，语义与兼容协议一致，实现位置不同**：兼容协议把子任务树放在 TaskTracker 那台 worker 的**本地** `task_info` 表里，server 一行不落，控制台的实例详情是反向 ask 那台 worker 拿的；gojob 的 worker 之间不通信、而且是 pull 模型，所以子任务**落库**在 `instance_tasks`（`parent_task_id` 串成树），由 server 承担 TaskTracker 的角色。用户可见的语义保持一致：广播每台一份且**节点失联时那一份判失败、不换机重跑**（兼容协议 issue #1003 同结论）；`MAP` 任一子任务最终失败即实例失败；`MAP_REDUCE` 由 reduce（保留名 `OMS_LAST_TASK`、层次号 `9999`）的返回值决定成败，因此开发者可以在 reduce 里容忍部分失败；`map()` 的保留任务名、结果 8096 字节截断、多级 map、`instance_num_limit` 等都按兼容协议口径实现。四处刻意偏离，都需要知道：① **reduce 不钉回根任务那台机器**（兼容协议必须钉，因为结果在它本地表；我们的结果在库里，钉住等于多一个"那台机器挂了 reduce 永远没人跑"的失败模式）；② **广播的节点集合是触发那一刻的快照**，运行期新上线的节点不补（兼容协议的 `WorkerDetector` 会补，但那等于实例开跑后再凭空多一份，对幂等敏感的处理器更危险）；③ 子任务负载是 **JSON**，不是 JDK 原生序列化——我们的 worker 池是 Go/Java 混布的，JDK 序列化的字节 Go 侧读不了；④ 每个实例都有一行根任务（广播的根建出来就是终态，只负责记录"广播了几台"），这样"一个实例一棵树"在所有执行类型下形状统一。附带好处：实例结束、worker 下线之后子任务树依然可查。
- **`SHARDING` 不提供**：兼容协议 3.x 的分片执行在 4.x 起由 `MAP` 承担，gojob 直接只留 `MAP`；建单/导入/OpenAPI 遇到 `SHARDING` 会带原因拒绝并提示改用 `MAP`（`WORKFLOW` 同理，见上条「无工作流」）。控制台那个 `select * from task_info where … limit 10` 输入框照搬，但后端**不拼 SQL**：兼容协议只有关键字黑名单、而且是在自己 worker 的本地库上执行，越权面比在共享库上小得多；gojob 换成只认白名单语法片的解析器，认不出来就在弹窗里给出可用写法，而不是让整个请求失败。
- **脚本任务（Shell 已对齐，PYTHON 未实现）**：兼容协议的 `processorType=SHELL/PYTHON` 在 5.x 早已 `@Deprecated`，且 worker 侧**没有任何 `ProcessorFactory` 支持它们**——官方脚本任务的正解是 `BUILT_IN` + 官方处理器的类全名 `tech.powerjob.official.processors.impl.script.ShellProcessor`。gojob 按这个口径实现：`cmd/gojob-agent` 注册该处理器（别名短名 `shell`），行为对齐 `AbstractScriptProcessor`（实例参数优先取参、落盘 `official_script_processor/shell_{instanceId}.sh`、`chmod 755`、`/bin/sh` 执行、stdout/stderr 逐行进任务日志），并加了兼容协议没有的一道白名单闸与进程组取消，详见 `worker/README.md`「零代码执行节点」。导入老版任务包时 `SHELL` 类型会被**自动改写**成 `BUILT_IN` + 官方脚本处理器（脚本正文搬进任务参数），因此既有任务能原样跑起来；`PYTHON` 与 `EXTERNAL`（JAR 动态容器，形如 `容器ID#类名`）目前**明确拒绝**而不是静默入库成哑弹。处理器类型的控制台选项、占位文案与列表列均沿用兼容协议的 `BUILT_IN`（内建）/`EXTERNAL`（外置·动态加载）两值口径。
- **分发策略**：`1/2/11`（健康优先/随机/指定机器）与兼容协议 `DispatchStrategy` 同值同算法；额外提供 **`21 ROUND_ROBIN`（轮询）——兼容协议没有该策略**，属本项目自定义扩展，需严格轮流执行时才用。兼容协议的 overload 过滤（`filterOverloadWorker`）未移植，pull 模型下由 worker 并发额度自然背压。
- **无 worker 的触发语义（对齐）**：触发时项目没有任何在线节点，实例当场判 `FAILED`（`无可用 worker`），不排队等节点上线后补跑——与兼容协议 `DispatchService`「取消派发并置 FAILED」同结论。差别只在实现位置：兼容协议在 push 派发时判，gojob 是 pull 模型，所以在触发时探测 + 排队超时巡检双重兜底；「节点在线但过载」两边都保持排队（兼容协议由 10s 状态巡检重派，gojob 由 worker 空闲额度自然背压）。零节点广播是刻意例外：gojob 记 `SUCCEED` + 结果说明（不产生可执行任务），兼容协议会因无 worker 把实例判失败。
- **健康分指标**：心跳新增 `cpu_processors` / `memory_max` / `tag`。Linux（含容器）实测采集 cgroup 内存限额与 `/proc/loadavg`；Go 在 macOS/Windows 取不到机器内存与负载时报 0，健康分退化为「按已用内存排序」，Java SDK 用 JVM `-Xmx` 与 `getSystemLoadAverage()` 不受影响。
- **多副本归属：租约 + fencing（刻意不照抄兼容协议的一处）**：兼容协议用 `app_info.current_server` + 先到先得的选举表达归属（`ServerElectionService`），判活靠 worker 心跳反向 PING 旧主，并且**明确没有**防"网络分区后旧主仍在写"——它的锁是 JVM 本地的 `ReentrantLock`（`@UseCacheLock`），跨进程无效。gojob 归属同样按项目划分，但用带 TTL 的租约行（`app_leases`）判活、用 `holder + epoch` 的条件 UPDATE 挡僵尸主，代价是接管时延从"PING 到就换"变成"最多 TTL + 一跳"（≈13s），换来的是双跑在库里被堵死。
- **归属的"下发方式"沿用兼容协议，"承重方式"没沿用**：gojob 也把调度归属副本的地址随心跳下发给 worker（`cluster_hint.owner`，语义等同 `current_server`），worker 据此把长轮询收敛到归属副本，省掉一次跨副本信号。差别在于它是**可选优化**：兼容协议的 `current_server` 是唯一通路（server 主动 push），所以被迫配一整套续命机制——每 10s 一次的 `/server/acquire` 专用端点、每次发现都对 owner 发 RPC PING（其源码注释自陈"QPS 蛮爆表的"，只能靠 `oms.accurate.select.server.percentage` 抽样降压）、`server_elect_%d` 分布式锁抢主、连续 3 次找不到 owner 就**本地杀死秒级任务**。gojob 是 pull 模型 + 在线表落库，任何副本都能发活，因此以上五项一个都不需要，`owner` 提示给错或给不到都只是退化成一跳信号延迟（实测：故意把某副本的对外地址配成连不上的端口，任务照常触发与执行成功，无双跑）。
- 控制台的信息构成与交互沿用兼容协议官方控制台（1:1 对齐），仅保留少量定制项（登录后统计总览落地页、项目列表用卡片布局、新增「系统管理 → 节点监控」页，见上节）。

## 技术栈

- Server / Go SDK：Go 1.27、`gin`、`gorm`（sqlite/mysql/postgres 驱动）、`encoding/json/v2`
- 控制台：Vue 3 + TypeScript + Element Plus + Pinia + vue-router + axios，pnpm 管理
- Java SDK：Maven 多模块、OkHttp、Jackson、SLF4J；提供 plain Java / Spring Boot starter / Quarkus 三种接入形态

各子工程详见其目录内 README：`server/` `worker/` `dashboard/` `sdk-java/`。

## 贡献与开发

```bash
go build ./... && go vet ./... && go test -race ./...     # 后端
cd dashboard && pnpm install && pnpm build                # 控制台（vue-tsc 全量类型检查在 build 里）
cd sdk-java && mvn -o test-compile                        # Java SDK 三形态
scripts/verify-map-broadcast.sh                           # 实机验收：一个实例一棵任务树
scripts/verify-shell-agent.sh                             # 实机验收：脚本执行链与安全默认值
scripts/verify-worker-token.sh                            # 实机验收：注册换票与越权拒绝（需先起 :7789 的 server）
scripts/verify-cluster.sh                                 # 实机验收：双副本归属 / fencing / 接管
scripts/release.sh v0.2.0 --dry-run                       # 发布前全流程演练（校验 + 产物矩阵 + Release 说明）
```

改动涉及协议字段时，`internal/protocol` 是唯一权威：Go SDK、Java SDK、DDL（`scripts/ddl/{mysql,postgres}.sql`）与相关 README 必须同步改；存量库的升级 ALTER 语句集中写在 `server/README.md`「数据库初始化」。

## 许可与鸣谢

- 许可证：**Apache License 2.0**，全文见 `LICENSE`。
- 本项目与 PowerJob 官方**无隶属、无背书关系**，是其接口语义的 Go 重实现：控制面协议、控制台交互与任务导入/导出格式与之对齐，以便直接复用既有的任务包与调用方；Worker 数据面是 gojob 自有的 HTTP 长轮询协议，**不兼容 PowerJob 的 Worker**。逐条差异与"为什么这样偏离"集中在上一节，未实现的能力如实列在 `CHANGELOG.md` 的 `[Unreleased]` 一节——宁可写"明确拒绝"，不做静默降级。
- `PowerJob` 为其各自维护者的商标与项目名称。
