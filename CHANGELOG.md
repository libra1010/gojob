# 更新记录

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### 修复

- **无节点时的触发不再积压历史实例**：触发时项目没有任何在线节点，实例与根任务当场记 `FAILED`（`无可用 worker`），不再留在队列里等节点上线后把几小时的陈旧实例按顺序跑一遍；计划时间照常推进，零节点广播保持 `SUCCEED` 语义。兜底巡检每 30s 把排队超 1 分钟且仍无在线节点的 `WAITING` 任务判失败，覆盖触发后节点立刻掉线
- **拉取与派生的停止守卫**：`fetchTask` 只发活动实例（`WAITING`/`RUNNING`）下、且没有停止意图的任务；停止与失联回收竞态产生的"排队中却已请求停止"的行不再被放回队列；停止中/已结束的实例拒绝再 `map()` 派生新子任务

## [0.2.0] - 2026-09-26

首个公开版本。调度内核、控制台、双语言 Worker SDK、零代码脚本节点与部署全套已可用；这一版里同时包含**注册口令加密**、**200 任务级容量的实机校准与四处修复**、**国内镜像版 Dockerfile**。行为断言由 7 支实机验收脚本守住（清单与各自覆盖范围见 README「实机验收脚本」），另有 `go test -race ./...`、`vue-tsc`、`mvn test` 三套单测。

### 新增

- **调度**：两层模型（粗扫线程 + 内存时间轮，tick 10ms），支持亚秒级周期；六种时间类型 `CRON` / `FIXED_RATE` / `FIXED_DELAY` / `DAILY_TIME_INTERVAL` / `API` / `DEPENDENCY`，其中固定频率与固定延迟单位为**毫秒**（上限 120000ms）；管理面改配置后毫秒级生效，不等扫描周期
- **执行类型**：`STANDALONE` / `BROADCAST`（每台一份）/ `MAP` / `MAP_REDUCE`，一个实例一棵任务树，子任务落库、Server 承担 TaskTracker，实例结束、worker 下线之后树依然可查
- **分发策略**：健康优先 / 随机 / 标签定向（`tagEquals:` `tagIn:` IP·tag）+ 自建的轮询策略；与 `designated_workers`、`max_worker_count` 组合生效
- **停止实例**：停止意图只存数据库，靠 `fetchTask`/`heartbeat` 响应夹带 `cancel_task_ids` 下发；协作式取消（Go 按任务 `context.WithCancel`，Java `Thread.interrupt` + `isStopped()`/`checkStopped()`），服务端在下达指令后宕机再起来停止依然送达
- **零代码执行节点 `gojob-agent`**：不写代码、把脚本放进脚本库即可被调度；默认只执行脚本库内的裸文件名，内联脚本文本要显式开 `GOJOB_AGENT_ALLOW_INLINE_SCRIPTS`；停止会杀掉整个进程组（含脚本自己起的子进程）
- **任务日志**：per-task 单调 `seq` + ack 后才出队 + 无限退避重试，服务端按 `(task_id, seq)` 唯一索引去重 ⇒ at-least-once 之后等效不丢不重
- **控制台**：任务全字段建单/编辑、实例列表与停止、实例详情（Task 统计 + 子任务树 + 结果）、日志页、统计总览、系统管理（用户/角色/登录日志/操作日志）、节点监控（每个 Server 副本与每个 Worker 的 CPU/内存/GC/连接数/线程数）
- **账号与权限**：三级角色 `SUPER_ADMIN` / `ADMIN` / `READONLY` + 项目级授权、改密/禁用即时吊销会话、登录与操作审计（口令字段脱敏）
- **多副本 Server**：app 级租约归属 + fencing epoch、无状态签名令牌 + 数据库吊销基线、worker 在线表落库、跨副本入队信号（新任务跨节点可见延迟从 ≤5s 压到 ≤1s）
- **OpenAPI 兼容层**：OpenAPI 会话接口、控制台交互与任务导入导出格式对齐，可直接吃兼容协议导出的任务包（`SHELL` 任务被改写成内建脚本处理器并真的能跑；`PYTHON`/`EXTERNAL` 逐条带原因拒绝）
- **数据保留**：`GOJOB_RETENTION_DAYS` / 任务 `life_cycle` 双闸门，按批回收超期的终态实例及其子任务与日志
- **运维**：数据库连接池显式参数化（此前是 `database/sql` 的"无限连接"默认）
- **注册口令加密（可选，默认关闭）**：配 `GOJOB_WORKER_AES_KEYS` 后，worker 注册与 OpenAPI `authApp` 的**项目口令不再明文出现在链路上**——客户端先领一张 30s 过期、用过即废的一次性 challenge，把 `{app_id, instance_id, challenge, password}` 用 HKDF 派生 key + AES-256-GCM 加密后送上，`AAD` 绑住版本/`kid`/项目。抓到一个注册包既还原不出口令也不能重放（消费记录写在库里的 `revoked_tokens` 主键上，跨副本互斥）。`GOJOB_WORKER_AES_REQUIRED=true` 进一步要求所有注册都走加密，明文回 `428` + 一张现领的 challenge（刻意不用 401，否则 SDK 会当成"票过期"无限重注册）。SDK 两侧同口径：Go `worker.Config.AesKey`、Java `aesKey` / `gojob.worker.aes-key` / `GOJOB_WORKER_AES_KEY`，**配了 key 就一定加密**，服务端没启用时报错并指出是哪一侧没配，不静默降级。它只保护口令这一个字段，token/任务参数/日志仍是 HTTP 明文内容，边界见 README「注册口令加密」；key 与集群密钥同级，只放 env/flag，不进数据库、控制台无输入框。验收：`scripts/verify-register-encryption.sh`（47 项，含 Go 与 Java 两个真 worker 进程各走一次完整握手）
- **控制台脚本任务录入**：处理器类名选成官方脚本处理器后，表单直接给出**脚本来源**入口——「脚本库文件名 + 命令行参数」或「内联脚本正文」，两种写法都序列化进「任务参数」这一个字段（脚本处理器的取参口径就是它），不新增存储；内联模式在界面上就写清需要执行节点开 `GOJOB_AGENT_ALLOW_INLINE_SCRIPTS`
- **批量导出**：`GET /job/export` 现在接受 `job_id`（单条，返回对象）/ `job_ids`（多条，返回数组）/ `app_id`（整项目，返回数组）；控制台任务列表加多选列与「批量导出」，可按选中或本项目全部生成一个任务包并下载
- **批量导入**：导入对话框可一次选多个 `.json` 文件，逐个解析后合并成待导清单（仍可手工编辑），逐条返回成败与原因；入口名称改为「导 入」，说明里点明支持兼容协议导出的任务包
- **国内镜像版 Dockerfile（`Dockerfile.cn`）**：与 `Dockerfile` 只差两处来源——基础镜像换成华为云 SWR 的搬运副本（`…/docker.io/library/golang:1.27.1-alpine`、`…/alpine:3.24.0`），构建期模块代理走 `goproxy.cn`；两个基础镜像都开成 `--build-arg`，同步进度对不上时换个 tag 就行。target、构建参数、产物布局、`USER`/`EXPOSE`/`ENTRYPOINT` 与原来那份逐行一致，**原 `Dockerfile` 保留为默认**。compose 用 `GOJOB_DOCKERFILE=Dockerfile.cn docker compose up -d --build` 切。刻意**没有**关 `GOSUMDB`：校验库由 goproxy.cn 代理，速度够了，不必拿依赖完整性换速度
- **容量实机验收**：`cmd/gojob-loadgen`（一台机器起 N 个真 worker 节点，或把 200 个任务压在 1 个节点上：`EXEC=MAP SPLIT=N CONC=M WORK=毫秒`）+ `scripts/verify-capacity.sh`（连打多轮，逐轮打派发延迟分位数、空闲与压完两态的服务端开销、注册退避次数；**故意不设数字门限**，阈值由实测反过来定）。两组口径：**200 节点各一份** ⇒ p50 59–69ms、p99 101–138ms、一轮 200 份全部开跑 114–162ms，只挂机时 Server 占 3.7% CPU / 14MB 堆 / 417 协程 / 200 连接；**1 个节点同时跑 200 个任务**（`MaxConcurrency=200`）⇒ p50 20–23ms、200 份全部开跑 62–134ms，每个任务真跑 50ms 时整轮 74–88ms（串行需 10s）。数字与瓶颈口径见 README「容量实机数字」

### 变更

- 面向用户的文案与内部命名不再以外部产品为主语：兼容层文件改名（`server/api/openapi_compat.go`）、任务包类型改名（`JobDTO`，Go 与 TS 同名），代码注释里的对照统一改称「兼容协议」。协议字段、JSON 键名、DDL、endpoint 路径与兼容行为一字未改
- 控制台与首页文案的主标题只留产品名 `gojob`，定位改为副标题「轻量级分布式任务调度平台」（`dashboard/src/layouts/MainLayout.vue` 顶部品牌区两行式）；README 副标题补上实测容量口径

### 修复（近期实机揪出，随本版本一起发布）

- 停止指令未 ack 期间，`fetchTask` 长轮询退化成忙等（实测单个 30s 任务被重复下发 53 次停止指令，每轮两条数据库查询）；现在只为 2s 内新下达的指令提前结束长轮询，SDK 侧对重复下发幂等
- 显式配置的 `max_retry_times=0` / `instance_retry_num=0` / `instance_num_limit=0` 会被数据库列默认值静默顶成 3（gorm 的 `Create` 跳过"带 `default` 标签且值为零"的列）；现在 0 真的能落库，修改接口也能把值改回 0
- 实例详情接口的 `order by created_time` 会报 `no such column: last_modified_time`（排序列名没映射到真实列）
- 多副本同时首启会撞管理员播种的唯一索引导致一个进程直接退出，改为 `ON CONFLICT DO NOTHING`
- `POST /job/save` 把显式传入的 0 当作"没传"；任务日志通道在无 sender 时被调用会 nil panic
- **长轮询丢唤醒**：`Fetch` 原先先 `claim()` 再取唤醒 channel，两步之间到达的那次唤醒会被丢掉（旧 channel 已被 close 并换掉），只能等 5s 兜底复查——表现为个别轮次里一整批任务统一迟到约 1 秒。改为先取 channel 再认领（close 过的 channel 永远可读，不漏也不多等）
- **注册风暴不再永久吃掉节点**：SDK 启动期注册按"连不上"与"服务端明确拒绝"分治——前者阶梯退避重试（200ms/1s/2s/5s + 抖动，2 分钟止损），后者仍然立即失败。整池节点同时冷启动时注册（唯一要过 bcrypt 的请求）会打满服务端 accept 队列，实测 200 节点每轮有 0~24 个连接被 reset，旧行为是这些节点直接退出且永不回来。Go / Java 两侧同语义
- **心跳齐射**：SDK 心跳间隔加 ±25% 相位抖动（Go / Java 两侧）。心跳是 worker 侧唯一高频写库的请求，整池同相位时每 5s 一起写，实测单条 `worker_nodes` upsert 被排到 237~338ms（服务端 `SLOW SQL`），与之共用单写者的任务认领被顶到 1 秒开外
- **满载时白等 1 秒**：SDK 拉取循环在"并发额度已满"时固定睡 1s 再检查 ⇒ 一台默认 16 额度的节点消化 200 条任务要白等 12 次秒级睡眠（实测整轮 11s）。改成任务释放额度即唤醒拉取（1s 降为兜底），同一场景 94–160ms；Go / Java 两侧同改。`verify-capacity.sh` 随之扩成能压"一个 worker 同时跑 N 个"（`EXEC=MAP SPLIT=N CONC=M WORK=毫秒`）

### 安全（口径与已知边界，不粉饰）

- **worker 的停止接单改由管理面驱动，demo/agent 不再监听任何端口**：此前 `cmd/gojob-demo` / `cmd/gojob-agent` 各开一个 `POST /drain` `/resume`（默认 `:7799`/`:7800`，所有网卡），而且**不带任何凭证**——同网段任何人不需要项目口令、不需要账号，一个空 POST 就能让这台节点停止收单，多节点挨个点就是把整池打停。这个端点本来就不该存在，所以删掉而不是绑回环。现在的形状：
  - 业务进程自己决定下线 → SDK 方法 `worker.StopReceivingNewTasks()` / `ResumeReceivingNewTasks()`（一直都有，进程内调用），或 Ctrl-C 走优雅下线（在跑的跑完再退）；
  - 运维从管理面停某一台节点 → 控制台「项目 → 节点 → 停止接单」，接口 `POST /openapi/worker/acceptTasks`，走控制台会话票 + 项目授权 + 操作审计。实现就是 `worker_nodes.accept_tasks` 这一列改成 0，生效靠选节点时过滤（`registry.Workers` 只返回 `accept_tasks=1` 的节点），**没有新增协议字段、没有改动 SDK、Java SDK 也不动**：那台 worker 照常长轮询，只是认领 SQL 永远筛不到属于它的任务，于是空转到超时；在跑的任务不受影响，继续执行并回报。停止/恢复都落库，所以重启副本、请求打到别的副本都一样有效
- 数据库里不存项目口令的明文等价物：`app_infos.password` 存 bcrypt，首启自动把存量明文升级；兼容协议的 `authApp` 只收明文口令，**只发 md5 摘要的官方 SDK 调用方会被明确拒绝**（这是与兼容协议的声明式差异）
- 数据面（心跳/拉单/回报/日志/派生子任务）只带短期 token，不带项目口令；只有 `POST /worker/register` 需要口令
- 回报与派生子任务都校验"只有正在执行该任务的节点能改判/派生"，冒用他人身份记日志并 401
- 边界：① 口令与 token 都走 HTTP 明文 ⇒ 必须置于内网或前置 TLS；② 能注册（= 知道项目口令）就能收单，`gojob-agent` 因此等同于"拿到口令的人能在该主机执行命令" ⇒ 只部署到隔离网段；③ token 不落库、票被盗在有效期内可冒用该节点身份；④ `GOJOB_CLUSTER_SECRET` 是共享长期凭据，拿到即可自签 `SUPER_ADMIN`，须与数据库口令同等对待

### 已知限制

- 依赖单个关系型数据库，无独立元数据存储；`sqlite` 只适合单副本（卷不能跨容器共享，且是单写者）
- 协作式取消的固有边界：处理器完全不检查中断/`ctx` 时停不掉它，只能等它跑完（状态仍如实记 `STOPPED`）
- 没有 Prometheus/Actuator 导出；节点监控只保留最近约 10 分钟曲线，历史留存与告警未做
- `life_cycle` 目前只能经 OpenAPI 设置，建单表单里还没有这一项

[unreleased]: https://github.com/libra1010/gojob/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/libra1010/gojob/releases/tag/v0.2.0

### 尚未实现（当前版本会明确拒绝或含糊可用）

- 工作流（Workflow / DAG 编排）—— 任务间的前后依赖目前由轻量的**依赖触发**表达（上游实例成功即拉起下游，单上游）
- `SHARDING` 执行类型 —— 建单时直接拒绝并提示改用 `MAP`（分片语义由运行期派生子任务承担）
- JAR 动态容器（兼容协议的 `EXTERNAL` 处理器类型）—— `processor_type=EXTERNAL` 会被 worker 明确拒绝，不会伪装成功
- `PYTHON` 处理器 —— 导入时逐条给出原因并拒绝
- 告警与通知（邮件/钉钉/Webhook）—— 未做，失败只体现在实例状态与任务日志里
- `instanceTimeLimit`（实例超时强杀）—— 未做，因此**没有超时兜底**：卡死的处理器只能人工停止实例
- 建单表单里还缺 `life_cycle`、机器配置三项（min CPU/内存/磁盘）、报警配置、`logConfig`、`taskTrackerBehavior`；这些字段多数需要后端先加列并实现语义
