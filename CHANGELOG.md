# 更新记录

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

尚未实现、且当前版本会明确拒绝或含糊可用的能力：

- 工作流（Workflow / DAG 编排）—— 任务间的前后依赖目前由轻量的**依赖触发**表达（上游实例成功即拉起下游，单上游）
- `SHARDING` 执行类型 —— 建单时直接拒绝并提示改用 `MAP`（分片语义由运行期派生子任务承担）
- JAR 动态容器（PowerJob 的 `EXTERNAL` 处理器类型）—— `processor_type=EXTERNAL` 会被 worker 明确拒绝，不会伪装成功
- `PYTHON` 处理器 —— 导入时逐条给出原因并拒绝
- 告警与通知（邮件/钉钉/Webhook）—— 未做，失败只体现在实例状态与任务日志里
- `instanceTimeLimit`（实例超时强杀）—— 未做，因此**没有超时兜底**：卡死的处理器只能人工停止实例
- 建单表单里还缺 `life_cycle`、机器配置三项（min CPU/内存/磁盘）、报警配置、`logConfig`、`taskTrackerBehavior`；这些字段多数需要后端先加列并实现语义

## [0.2.0] - 2026-09-25

首个公开版本。调度内核、控制台、双语言 Worker SDK、零代码脚本节点与部署全套已可用，实机断言 182 项。

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
- **PowerJob 兼容层**：OpenAPI 会话接口、控制台交互与任务导入导出格式对齐，可直接吃 PowerJob 导出的任务包（`SHELL` 任务被改写成内建脚本处理器并真的能跑；`PYTHON`/`EXTERNAL` 逐条带原因拒绝）
- **数据保留**：`GOJOB_RETENTION_DAYS` / 任务 `life_cycle` 双闸门，按批回收超期的终态实例及其子任务与日志
- **运维**：数据库连接池显式参数化（此前是 `database/sql` 的"无限连接"默认）

### 安全（口径与已知边界，不粉饰）

- 数据库里不存项目口令的明文等价物：`app_infos.password` 存 bcrypt，首启自动把存量明文升级；PowerJob 的 `authApp` 只收明文口令，**只发 md5 摘要的官方 SDK 调用方会被明确拒绝**（这是与 PowerJob 的声明式差异）
- 数据面（心跳/拉单/回报/日志/派生子任务）只带短期 token，不带项目口令；只有 `POST /worker/register` 需要口令
- 回报与派生子任务都校验"只有正在执行该任务的节点能改判/派生"，冒用他人身份记日志并 401
- 边界：① 口令与 token 都走 HTTP 明文 ⇒ 必须置于内网或前置 TLS；② 能注册（= 知道项目口令）就能收单，`gojob-agent` 因此等同于"拿到口令的人能在该主机执行命令" ⇒ 只部署到隔离网段；③ token 不落库、票被盗在有效期内可冒用该节点身份；④ `GOJOB_CLUSTER_SECRET` 是共享长期凭据，拿到即可自签 `SUPER_ADMIN`，须与数据库口令同等对待

### 修复（近期实机揪出，随本版本一起发布）

- 停止指令未 ack 期间，`fetchTask` 长轮询退化成忙等（实测单个 30s 任务被重复下发 53 次停止指令，每轮两条数据库查询）；现在只为 2s 内新下达的指令提前结束长轮询，SDK 侧对重复下发幂等
- 显式配置的 `max_retry_times=0` / `instance_retry_num=0` / `instance_num_limit=0` 会被数据库列默认值静默顶成 3（gorm 的 `Create` 跳过"带 `default` 标签且值为零"的列）；现在 0 真的能落库，修改接口也能把值改回 0
- 实例详情接口的 `order by created_time` 会报 `no such column: last_modified_time`（排序列名没映射到真实列）
- 多副本同时首启会撞管理员播种的唯一索引导致一个进程直接退出，改为 `ON CONFLICT DO NOTHING`
- `POST /job/save` 把显式传入的 0 当作"没传"；任务日志通道在无 sender 时被调用会 nil panic

### 已知限制

- 依赖单个关系型数据库，无独立元数据存储；`sqlite` 只适合单副本（卷不能跨容器共享，且是单写者）
- 协作式取消的固有边界：处理器完全不检查中断/`ctx` 时停不掉它，只能等它跑完（状态仍如实记 `STOPPED`）
- 没有 Prometheus/Actuator 导出；节点监控只保留最近约 10 分钟曲线，历史留存与告警未做
- `life_cycle` 目前只能经 OpenAPI 设置，建单表单里还没有这一项

[unreleased]: https://github.com/libra1010/gojob/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/libra1010/gojob/releases/tag/v0.2.0
