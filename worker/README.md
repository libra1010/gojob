# gojob Go Worker SDK
> 本文的「兼容协议」= gojob 控制面刻意对齐的那套外部接口语义；对齐范围与逐条差异见根 README 的「兼容性说明」。


`worker` 包是 gojob 的 Go 执行层 SDK：业务进程 `import` 本包，注册 `Processor` 后运行，即成为调度中心的一个在线节点。走 gojob 自有 HTTP 长轮询数据面协议（`/worker/*`）。

`Run` 启动时先用 `AppId + Password` 调 `/worker/register` 换一张 worker token，之后心跳/拉任务/回报/传日志都只带这张 token；被服务端回 `401`（服务重启、票过期、项目改了口令）时自动重新注册并重放当次请求，业务代码无需关心凭证轮换。

启动期注册的失败处理分两类，因为它们的正确反应相反：

- **连不上**（reset / refused / 超时）→ 阶梯退避重试（200ms → 1s → 2s → 5s，带抖动，累计 2 分钟后止损报错）。注册是整链路唯一要过 bcrypt 校验项目口令的请求，整池节点同时冷启动会把服务端的 accept 队列打满：实测 200 节点同机、`somaxconn=128` 时，每轮稳定有 0~24 个节点在注册上吃到 connection reset。旧行为是这些节点直接退出——**永久不上线**，而其余节点跑得好好的，是最难发现的一类故障。（生产上顺便把内核 accept 队列抬高：Linux `net.core.somaxconn`、LB 的 backlog。）
- **服务端明确拒绝**（口令不对、服务端强制加密注册、服务端没配 key）→ 立即失败，不重试。重试不会让配置变对，只会把配置故障伪装成网络故障拖住启动。

心跳间隔带 **±25% 相位抖动**（`HeartbeatSeconds` ≥ 2s 时生效）。这不是装饰：心跳是 worker 侧唯一高频写库的请求，整池节点同时启动后相位一致，每 5s 一起写，sqlite 单写者下实测把单条心跳 upsert 顶到 237~338ms，同一条写通道上的任务认领被排到 1 秒开外。打散相位后，200 节点一轮广播的尾部延迟从"整轮 ~1s"回到 120~150ms（见根目录 `README.md`「容量实机数字」）。Java SDK 同语义。

## 快速上手

```go
package main

import (
    "context"
    "fmt"
    "os"
    "os/signal"
    "syscall"
    "time"

    "gojob/worker"
)

type helloProcessor struct{}

func (helloProcessor) Process(ctx *worker.TaskContext) error {
    ctx.Infof("hello 开始执行，静态参数=%s 运行时参数=%s", ctx.JobParams, ctx.InstanceParams)
    time.Sleep(200 * time.Millisecond)
    ctx.Infof("hello 执行完成")
    return nil // 返回 error 视为失败并触发重试
}

func main() {
    worker.Register("hello", helloProcessor{})

    w := worker.New(worker.Config{
        ServerAddress:  "http://127.0.0.1:7800",
        AppId:          "demo_app",
        Password:       "demo123",
        MaxConcurrency: 8, // 默认 16
    })

    ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    if err := w.Run(ctx); err != nil { // 阻塞直到 ctx 取消
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

Server 端 Job 的 `processor` 字段填注册名（此处 `hello`）。也可直接用便捷函数：`worker.Run(ctx, cfg)`。

## 类型与选项

### `Config`

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `ServerAddress` | — | Server 基址，如 `http://host:7800`。单副本/前面已有 LB 时用 |
| `ServerAddresses` | 空 | 多副本部署填**全部**副本地址（`["http://h1:7800","http://h2:7800"]`）。SDK 自己寻址与容错，不再必须挂 LB/VIP；与 `ServerAddress` 可同时给，会合并去重，列表首个即启动时的首选地址 |
| `AppId` / `Password` | — | 所属项目与项目口令；口令只在注册（含 401 后重注册）时使用，配了 `AesKey` 时连注册都不再明文上线 |
| `Address` | `hostname-pid` | 本节点对外标识 |
| `MaxConcurrency` | 16 | 同时执行任务上限。**"一个 worker 能不能同时跑 200 个任务"就是这一项**：拉取时 SDK 把 `max_task_num` 报成当时剩余额度，服务端对该数不设上限，所以一次长轮询就能带回 200 条任务并当场起 200 条 goroutine。额度满时拉取循环等"有额度了"的信号（1s 只作兜底），不会白睡一秒。CPU 密集型任务把额度开到核数以上只是把排队挪进本进程 |
| `HeartbeatSeconds` | 5 | 心跳间隔 |
| `MaxWaitSeconds` | 30 | `fetchTask` 长轮询阻塞时长 |
| `Tag` | 空 | 机器标签，供任务的分发策略 `SPECIFY`（`tagIn:` / `tagEquals:`）匹配 |
| `AesKey` | 空 | 注册口令加密的 key（64 位 hex，AES-256），须与服务端 `GOJOB_WORKER_AES_KEYS` 中的某一把一致。配了它，`Password` 不再明文上线：注册先领一张一次性 challenge，再把口令封进 AES-256-GCM 密文。**配了就一定加密**——服务端没启用时注册直接失败并指出是它没配，不会静默退回明文。见根 README「注册口令加密」 |

心跳随带 `cpu_processors`（`GOMAXPROCS`，容器内已按 CPU limit 收敛）、`memory_max`（cgroup v2/v1 限额，回落 `/proc/meminfo`）、`cpu_load`（`/proc/loadavg` 1 分钟均值）。Server 的健康优先策略用这三项算兼容协议公式 `(上限−已用)×2 + (核数−负载)`；macOS/Windows 本地开发取不到时上报 0，策略自动退化为按已用内存排序，不影响轮询/随机/指定机器。

### 多副本寻址与 owner 跟随

Server 是多副本时，把副本地址都填进 `ServerAddresses`，**不需要再挂 LB/VIP**。SDK 的选路规则（`applyHint`）：

1. 启动时按列表顺序取第一个地址；任何一次"这台没答上话"（连接失败、超时、5xx）就换下一个已知地址重放，业务错误（HTTP 200 + `code!=0`）不换副本——那是服务端的明确答复。
2. 每跳心跳的响应里带 `cluster_hint`：`servers`（在线副本地址，本地列表会自动并进去，所以新起的副本无需重启 worker）与 `owner`（本 app 调度归属副本的可达地址）。
3. `owner` 只是**首选地址**，不是唯一通路。要满足两个条件才迁移：目标已在已知集合里、且**连续 2 跳**都指向它。单跳不动作，避免接管窗口里被两个副本各说各话甩来甩去。迁移只影响下一次拉取周期；为不让 worker 挂在僵尸副本上等满一个轮询周期，地址变更时会掐断在途的那次长轮询。
4. 迁过去连不上 → 自动退回列表内的其它副本继续收单回报，任务不丢。**这就是与兼容协议的关键差别**：兼容协议的 `current_server` 是唯一通路（找不到 owner 连续 3 次会本地杀死秒级任务），我们的 `owner` 坏了也只是退化成"多一次跨副本信号"。

因此最坏时延是可算的：副本崩溃（RST）→ 连接立即可辨，加 2 跳确认 ≈ 10s；副本被冻结（SIGSTOP / 主机挂起，TCP 还连着但永不返回）→ 靠心跳 15s 超时辨认，加确认与在途轮询让位 ≈ 20~30s。调小 `HeartbeatSeconds`/`MaxWaitSeconds` 可缩短，代价是更多 HTTP 开销。

Java SDK（`sdk-java/`）的行为与本节逐条对齐：`OpenConfig.setServerAddresses(...)` 或 `serverAddress` 用逗号分隔多个副本地址，同样是"连续 2 跳确认才迁移 + 连不上自动退回 + 地址变更时放弃在途长轮询"，实机验收见 `scripts/verify-java-routing.sh`。

### `Processor`

```go
type Processor interface {
    Process(ctx *TaskContext) error
}
```

`Register(name, p)` 在包级注册；`name` 对应 Job 的 `processor` 字段；重复注册 panic。处理器应为并发安全、无状态（进程内单例）。

### `TaskContext`

内嵌 `context.Context`，附带：`TaskID` `JobID` `InstanceID` `JobParams`（建单静态参数 JSON）、`InstanceParams`（单次触发运行时参数）、`Attempts`（第几次尝试，从 1）；多任务执行类型再补 `TaskKey`（层次 ID：根 `0`、第一片 `0.1`、二级 `0.1.2`、reduce `9999`）、`TaskName`（`map` 时起的名字）、`ExecuteType`、`SubTaskContent`（本子任务负载原文）。

日志 API：`Debugf / Infof / Warnf / Errorf`（SLF4J 风格 `{}`… 实际为 Go `%s/%v` 格式化），按任务 `log_level` 阈值过滤后批量上报 Server，落「实例 → 日志」页。

## 多任务执行类型（BROADCAST / MAP / MAP_REDUCE）

`BROADCAST` **不需要写任何代码**：Server 在触发那一刻按在线节点快照每台派生一份，处理器只管执行，同一份逻辑在每台机器上各跑一次。节点中途失联时那一份判失败、**不会**换机重跑（换机等于让同一份工作在一个从没执行过它的节点上再执行一遍）。

`MAP` / `MAP_REDUCE` 用 `TaskContext` 上的这几个方法，形状与兼容协议的 `MapProcessor` 一致：

```go
type mapSplit struct{}

func (mapSplit) Process(ctx *worker.TaskContext) error {
    if ctx.IsRootTask() {                      // 根任务：算出分片，派生出去
        keys, err := ctx.Map([]any{
            map[string]string{"word": "alpha"},
            map[string]string{"word": "beta"},
        }, "UPPER")
        ctx.SetResult(fmt.Sprintf("已派生 %d 片：%s", len(keys), strings.Join(keys, " ")))
        return err
    }
    var p struct{ Word string `json:"word"` }  // 子任务：干真正的活
    if err := ctx.SubTask(&p); err != nil {
        return err
    }
    ctx.SetResult(strings.ToUpper(p.Word))
    return nil
}
```

| API | 说明 |
| --- | --- |
| `ctx.Map(taskList []any, taskName string) ([]string, error)` | 派生子任务，返回 Server 分配的层次 ID。每个元素单独 JSON 编码，因此负载必须**可跨进程解码**（子任务落在哪台机器由集群决定，不能引用本进程的对象）。子任务里再调 `Map` 就是多级 Map。单次上限 **5000** 个，一个实例上限 **50000** 行 |
| `ctx.SubTask(v any) error` | 把本行负载解码进 `v`；根任务没有负载，返回错误而不是 panic |
| `ctx.SetResult(s string)` | 留下结果文本：非 MAP_REDUCE 型它就是实例结果，MAP_REDUCE 型它会出现在 reduce 收到的 `results` 里；`Process` 返回 error 时被错误原因覆盖。超过 **8096** 字节截断（兼容协议 `maxResultLength` 同值） |
| `ctx.IsRootTask()` / `ctx.IsLastTask()` | 判根任务 / reduce 任务。业务方一般只用到前者 |
| `ctx.SubTaskResults() ([]TaskResult, error)` | 自查全部兄弟子任务的结论（不含根与 reduce 自身），内部分页取满 |
| `worker.Reducer` 接口 | `Reduce(ctx *TaskContext, results []TaskResult) error`。**MAP_REDUCE 的处理器必须额外实现它**，否则那条任务明确失败并点名原因，不会静默跳过 |

`Reduce` 的返回值就是实例的最终结论：`MAP` 是"任一子任务失败即实例失败"，`MAP_REDUCE` 允许你在 reduce 里兜住部分失败（`results` 里失败的那几行 `Success=false`、`Result` 是错误原因），这是两种类型唯一的语义差别。

三点与兼容协议不同，值得知道：① 子任务负载固定 **JSON**，不是可插拔的 JDK 序列化（worker 池是 Go/Java 混布的）；② `taskName` 撞了调度器保留名（`OMS_ROOT_TASK`/`OMS_LAST_TASK`/`OMS_BROADCAST_TASK`）**直接报错**，不像兼容协议那样悄悄改名——改名会让"我明明 map 了 3 片"和库里的名字不一致，更难查；③ reduce 不钉回根任务那台机器，任何在线节点都能认领（结果在库里，不在谁的本地表上）。

## 停止接收新任务（优雅下线）

```go
w.StopReceivingNewTasks()   // 停止拉取新任务，运行中的继续跑完
// ... 观察 w.IsReceiving() / 等任务排空 ...
w.ResumeReceivingNewTasks() // 需要时可恢复收单
```

`ctx` 取消时 `Run` 会：停止收单 → 等在跑任务执行并回报完毕（drain）→ 最后 flush 日志通道，保证下线不丢日志与结果。

**这两个方法是给业务进程自己调的，worker 不监听任何端口。** 以前 demo/agent 上各开过一个 `POST /drain` 让运维从网络侧翻这个开关，那是错的形状：一个不带凭证就能停节点的口子，等于把整池 worker 的可用性暴露在同一网段上（挨个点就能把集群打停）。现在从管理面停某台节点的收单，走控制台「项目 → 节点 → 停止接单」（`POST /openapi/worker/acceptTasks`，需要控制台会话票 + 该项目写权限 + 落操作审计），实现只是把 `worker_nodes.accept_tasks` 改成 0——**worker 侧不需要任何改动，也不感知这个开关**：它照常心跳和长轮询，只是选节点时被过滤掉，认领不到属于它的任务。

## 被服务端停止（单任务取消）

控制台/OpenAPI 停止实例时，Server 把 `cancel_task_ids` 夹带在 `fetchTask` 与 `heartbeat` 的响应里；SDK 为每个任务维护 `context.WithCancel`，收到指令即取消该任务的 ctx，并回报终态 `STOPPED`（不计失败、不重试）。

**取消是协作式的**——处理器必须响应 ctx，否则会跑到自然结束：

```go
func (p *Processor) Process(ctx *worker.TaskContext) error {
    select {
    case <-ctx.Done():        // 被停止：返回 ctx.Err() 即可
        return ctx.Err()
    case <-time.After(10 * time.Second):
        return nil
    }
}
```

`worker.TaskContext` 直接内嵌 `context.Context`，`ctx.Done()/Err/Value` 均可用；把 ctx 继续往下传给 DB/RPC 调用即可获得全链路取消。

**停止指令如果赶在处理器拿到任务之前到达，处理器一次都不会被调用**（回报 `STOPPED`，结果列 `cancelled before start`，与 Java SDK 逐字一致）。所以"我的处理器完全不检查 ctx"这种写法，在开跑前被停下的那部分任务是安全的；已经开跑的那部分才会跑到自然结束。

**跑到自然结束的那部分，记的仍然是 `STOPPED`，不是 `SUCCEED`**：回报状态看的是 worker 侧"这个 taskId 收到过停止指令"的标记，不看处理器的返回值（实机验证：一个吞掉中断、跑满全程并返回成功的任务，实例与任务都是 `STOPPED`，结果列留着它自己那句"跑满了 30 秒"）。也就是说停止的**记账**总是准的，不确定的只是**副作用**——处理器该退出还得自己退出。

**指令送达要多久**：有空闲并发额度时长轮询在飞，`StopInstance` 提交后会立刻唤醒并夹带下发（实测下一个检查点就退，≈秒级）；额度被占满时 SDK 根本不发 `fetchTask`，只能靠 5s 一跳的心跳捎带（实测从下达停止到任务收尾 4.92s）。所以"点了停止但还在跑"先别怀疑没生效，按上面两条看：要么等一个检查点，要么等一跳心跳。重复下发是常态（未 ack 前每次响应都带全量清单），SDK 只认第一次，不会重复取消、不会刷日志。

业务代码要配合的只有两类地方：

| 你在干什么 | 要做什么 |
| --- | --- |
| `time.Sleep(d)` | 换 `select { case <-ctx.Done(): return ctx.Err(); case <-time.After(d): }` |
| HTTP / DB / gRPC | `http.NewRequestWithContext(ctx, …)`、`db.QueryContext`、gRPC 直接传 ctx |
| 批处理/分页长循环 | 每轮开头 `if err := ctx.Err(); err != nil { return err }` |
| 自己起 goroutine | 用 `errgroup.WithContext(ctx)` 或从 ctx 派生，别用 `context.Background()` |
| 起子进程 | `exec.CommandContext(ctx, …)`（`gojob-agent` 还额外用 `Setpgid`+`kill(-pgid)` 连脚本的子孙进程一起收） |

回报的判定不看返回值看意图：SDK 拿任务 ID 对着服务端的停止意图记账，所以**你只要尽快返回**，返回 `nil`、返回 `ctx.Err()`、返回别的错误都会记成 `STOPPED`（不算失败、不触发重试）；只有处理器自己 `SetResult` 留了话时，结果列才用你那句话。

## 日志可靠性

与 Server、Java SDK 一致：每任务单调 `seq` + Server `UNIQUE(task_id,seq)` 去重、确认后才出队、失败指数退避无限重试、先 flush 日志再回报结果、有界缓冲 50000。至少一次投递 + 幂等去重 ⇒ 有效不丢不重。

结果回报（`/worker/report`）同样至少一次：Server 端幂等，且**只接受认领该任务那台 worker** 的回报——`instance_id` 与任务归属不符时按成功应答但直接忽略，既防同项目内别的 worker 冒名改判，也让失联 worker 复活后的迟到回报不会污染已被重派的任务。因此 SDK 侧无需感知：`instance_id` 进程启动即固定，认领与回报走同一个值。

## 运行时指标上报（节点监控）

每次心跳（`worker/metrics.go` 的 `fillRuntime`）顺带带上本进程指标，控制台「系统管理 → 节点监控」据此画 worker 行：

| 字段 | 取法 | 取不到时 |
| --- | --- | --- |
| `cpu_percent` | `getrusage(RUSAGE_SELF)` 的 user+sys 时间差 / 墙钟时间，100 = 吃满一个核 | 非 unix 平台不出该值 |
| `gc_count` / `gc_pause_ms` | `runtime/debug.ReadGCStats`（Go 1.27 已删除 `runtime.GCStats`），暂停为启动以来累计毫秒 | — |
| `rss_mb` / `open_fds` | Linux `/proc/self/statm`、`/proc/self/fd` | macOS/Windows 报 0，页面显示"—" |
| `threads` | `runtime.NumGoroutine()`（Go 语义即并发执行单元，与 JVM 线程数同列展示） | — |
| `running_tasks` | `len(sem)`，即已占用的并发额度 | — |

口径统一为"**取不到就报 0，绝不猜数**"，因此对 SDK 使用者而言：本地 macOS 开发看到"—"是正常现象，部署到 Linux 容器即自动出数。堆内存沿用既有的 `memory_used`（heap inuse）/`memory_max`（cgroup 限额或 `/proc/meminfo`）两列，不在这里重复。

## 示例进程

`cmd/gojob-demo` 演示 `hello / slow / failOnce` 三种处理器与收单/下线，见 `../README.md` 快速开始。
`cmd/gojob-demo/map_processors.go` 里另有 `broadcastEcho`（广播：结果里带本机标识）、`mapSplit`（最简单的 Map）、`mapFailOne`（让指定那一片故意失败，用来验"任一失败即实例失败"）、`deepMap`（两级 Map）、`mrSum`（MapReduce 求和，reduce 里容忍部分失败）、`sleepMap`（慢子任务，给停止链路用）、`retryMap`（根任务首次尝试故意失败，给重派作废链路用）七个演示处理器，它们同时也是 `scripts/verify-map-broadcast.sh` 的断言靶子。

## 零代码执行节点 `gojob-agent`

`cmd/gojob-agent` 是一个**不写一行代码**就能执行脚本任务的可执行程序：它内嵌本 SDK，把 `internal/agent` 里的官方脚本处理器注册好，参数全部从 `GOJOB_*` 环境变量读。业务方要的只是「在控制台建个任务，处理器信息填脚本名，脚本放在服务器上」，而不是每个脚本都编一个 Go 进程。

```bash
GOJOB_WORKER_SERVER=http://127.0.0.1:7800 GOJOB_WORKER_APP=demo_app \
  GOJOB_WORKER_PASSWORD=demo123 GOJOB_AGENT_SCRIPT_DIR=/srv/gojob/scripts \
  ./gojob-agent
```

| 环境变量 | flag | 默认 | 说明 |
| --- | --- | --- | --- |
| `GOJOB_WORKER_SERVER` | `-server` | `http://127.0.0.1:7800` | Server 地址，填**任意一个副本**即可；其余副本由心跳响应里的 `cluster_hint` 自动学习，地址挂了 worker 侧自行容错 |
| `GOJOB_WORKER_APP` | `-app` | `demo_app` | 项目 ID |
| `GOJOB_WORKER_PASSWORD` | `-password` | — | 项目口令，**只用环境变量传**，命令行参数会被同机用户看到 |
| `GOJOB_WORKER_AES_KEY` | — | 空 | 注册口令加密的 key（64 位 hex）。配了它口令不再明文上线；agent 能跑命令，这一项比其他 worker 更值得配。故意**不提供 flag 形态**：key 与口令同为凭据，不该出现在 `ps` 里 |
| `GOJOB_WORKER_TAG` | `-tag` | `executor` | 节点 tag，配合分发策略 `SPECIFY` 的 `tagEquals:executor` 把脚本任务锁到这类节点 |
| `GOJOB_AGENT_SCRIPT_DIR` | `-script-dir` | — | 脚本库目录：预置脚本放这里，任务参数写裸文件名 |
| `GOJOB_AGENT_ALLOW_INLINE_SCRIPTS` | `-allow-inline-script` | `false` | 打开后才允许执行任务参数里直接带来的脚本文本/下载链接 |
| `GOJOB_AGENT_MAX_CONCURRENCY` | `-concurrency` | `8` | 同时执行的脚本数 |
| `GOJOB_AGENT_WORKSPACE` | `-script-workspace` | `{home}/gojob/agent` | 内联脚本的落盘工作目录 |
| `GOJOB_AGENT_SCRIPT_TIMEOUT` | `-script-timeout` | `0`（不限） | 单脚本超时，超时杀**整个进程组** |

两者都没配（既无脚本库也没开内联）时进程**直接拒绝启动**——一个什么都执行不了的 agent 留着只会让人误以为任务在跑。

任务里怎么写：`处理器类型` 选 `内建（BUILT_IN）`，`处理器信息` 填 `shell`（短名）或 `tech.powerjob.official.processors.impl.script.ShellProcessor`（兼容协议官方类全名，两个名字注册的是同一个处理器，所以同一份任务配置在 gojob 与兼容协议两边通用），`任务参数` 就是脚本内容或脚本库里的文件名——控制台建单抽屉识别出这个处理器后会直接给出**脚本来源**入口（脚本库文件名 + 命令行参数 / 内联脚本正文），它写的也是 `任务参数` 这一个字段，不另存。一次执行的流程：

1. 取参数：`instanceParams` 非空优先，否则 `jobParams`（与兼容协议一致）；
2. 判定来源：参数是脚本库里的**裸文件名** → 直接执行库里那份；否则视为内联脚本文本；
3. 内联文本再分一支：以 `http://` / `https://` / `ftp://` 开头 → 按兼容协议的 `DOWNLOAD_PROTOCOL` 语义下载（`ftp://` 未实现，明确报错）；否则就是脚本文本本身；
4. **闸**：第 2、3 步里凡是"内容来自任务参数"的分支，`GOJOB_AGENT_ALLOW_INLINE_SCRIPTS=false` 时直接失败，错误信息点名这个开关；脚本库分支不受影响；
5. 落盘：内联脚本写进 `{workspace}/official_script_processor/shell_{instanceId}.sh`，`chmod 755`，工作目录即脚本所在目录。与兼容协议的 `AbstractScriptProcessor` 有一处刻意的差异：它按 `instanceId` 命名且 `if (script.exists())` 就复用旧文件，同实例重试会执行**上一次**留下的脚本；gojob 每次覆写，因为这里没有任何机制保证内容没被别人改过；
6. 执行：`/bin/sh 脚本 参数…`（参数按空格切分，位置参数进脚本），独立进程组；stdout 逐行进任务日志 `INFO`、stderr 进 `WARN`，同时留在缓冲里；环境变量额外注入 `GOJOB_TASK_ID` / `GOJOB_JOB_ID` / `GOJOB_INSTANCE_ID`；退出码非 0 时结论形如 `脚本退出码 3，耗时 1.2s；[ERROR]: {stderr 尾部}；[INPUT]: {参数}`，成功返回空错误即 `SUCCEED`。Windows 上直接拒绝执行（`/bin/sh` 不存在，不做伪支持）。

### 安全边界（必须如实读这一段）

**拿到项目口令或集群密钥的人，等于可以在 agent 所在主机上执行命令。** gojob 的权限模型里项目口令就是数据面凭据，任何持者都能建任务并派给 agent；开了 `GOJOB_AGENT_ALLOW_INLINE_SCRIPTS` 之后，"任务参数"就成了任意脚本正文的传送带。这不是 gojob 引入的漏洞，兼容协议的脚本任务是同样的形状——但这里把它写死在文档里，别指望它被默认配置挡住。

因此：

- **默认只允许执行脚本库（`GOJOB_AGENT_SCRIPT_DIR`）里预置的脚本**，路径解析强制留在库目录内（`../` 出去会被拒），改脚本 = 改代码，走发布流程而不是走控制台；
- 内联脚本必须显式开关，且开启时进程启动就打一条醒目告警；
- agent **只应部署在隔离网段**：只出网到 Server，不接受来自办公网/公网的直接访问，主机上不放置任何它不该被读到的凭据（用最小权限账号跑，凭据只给这一个项目的口令，不要给数据库或云 AK）；
- 每次执行都落 worker 日志通道（脚本来源、参数、退出码、耗时、stdout/stderr），控制台实例日志页可查，`{workspace}/official_script_processor/` 下留有落盘脚本作为现场；
- `GOJOB_CLUSTER_SECRET` 与数据库口令同级对待：持者可为任意项目自签 `SUPER_ADMIN` 会话票，也就间接拥有派单权，不要写进镜像层、CI 变量或前端可见的配置。
- `GOJOB_WORKER_AES_KEY`（注册口令加密）与上一行同级：它防的是"链路上被抓走口令"，**不改变**"能注册就能收单"这条模型。开了它，拿到 key 的人依然能冒注任意 worker、拿到口令的人依然能让 agent 跑脚本。所以它属于"值得配、但别当成 agent 的护身符"那一类；agent 的真正防线仍然是隔离网段 + 脚本库白名单。

要跑更不可信的脚本，正确的做法不是继续加开关，而是把 agent 放在一次性隔离环境（容器/虚机）里——gojob 本身不做沙箱。Docker 一次性容器执行**未实现**（兼容协议的"容器"指的是 JAR 动态容器，与此无关）。
