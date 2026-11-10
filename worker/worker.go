// Package worker 是 gojob 的 Go 执行层 SDK：业务方 import 本包，注册 Processor 后运行。
//
//	w := worker.New(worker.Config{...})
//	go w.Run(ctx)
//	w.StopReceivingNewTasks() // 优雅下线：不再拉新任务，运行中任务继续执行
package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	rand "math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/internal/registercrypto"
)

// sdkVersion 上报给服务端的本 SDK 版本（心跳与注册共用）。
// defaultTokenTTL 服务端没回 ttl_seconds 时的兜底（老版本服务端）。
const defaultTokenTTL = 10 * time.Minute

const sdkVersion = "gojob-go/0.2"

// TaskContext 传给处理器的执行上下文（对标兼容协议的 JobContext 子集）。
type TaskContext struct {
	context.Context
	TaskID         uint64
	JobID          uint64
	InstanceID     uint64
	JobParams      string // 创建 Job 时配置的静态参数（JSON 字符串）
	InstanceParams string // 单次触发传入的运行时参数（兼容协议的 instanceParams 语义）
	Attempts       int    // 第几次尝试，从 1 开始

	// 多任务形态（BROADCAST/MAP/MAP_REDUCE）才非默认值：TaskKey 是层次 ID（"0"、"0.3"、
	// reduce 是 "9999"），TaskName 是 map() 的第二参，ExecuteType 决定能不能再派生子任务。
	// 对应兼容协议 TaskContext 的 getTaskId()/getTaskName() 与 isRootTask() 的判据。
	TaskKey     string
	TaskName    string
	ExecuteType string

	logger *taskLogger // Infof/Errorf 等日志 API 的实现
	worker *Worker     // Map/取子任务结果要走数据面，得拿到客户端
	// subTaskContent 本任务的子任务负载（根任务为空）。不直接暴露原文：
	// 给用户一个 SubTask(&v) 的解码入口，比让 ta 自己记着"这是 JSON"要难用错。
	subTaskContent string
	result         string // SetResult 写入，作为实例/子任务结果上报
	mapBatch       int    // 本任务第几次调 map，与 server 侧的幂等键同源
}

// Processor 业务处理器接口（对标 BasicProcessor）。
//
// 返回值只有 error：成功时如需给实例/reduce 留一份结果，调 ctx.SetResult。
// 这是刻意与兼容协议的 ProcessResult 分道扬镳的一处——Go 里 error 已经承载了"失败+原因"，
// 再包一层结构体只会让 90% 的处理器写成 return ProcessResult.success(null)。
type Processor interface {
	Process(ctx *TaskContext) error
}

var (
	procMu sync.Mutex
	procs  = map[string]Processor{}
)

// Register 注册处理器；name 与 Job 配置中的 processor 字段对应。重复注册 panic。
func Register(name string, p Processor) {
	procMu.Lock()
	defer procMu.Unlock()
	if _, dup := procs[name]; dup {
		panic("worker: duplicate processor " + name)
	}
	procs[name] = p
}

func lookupProcessor(name string) (Processor, bool) {
	procMu.Lock()
	defer procMu.Unlock()
	p, ok := procs[name]
	return p, ok
}

type Config struct {
	ServerAddress    string   // http://host:7800（单地址写法，等价于 ServerAddresses 只有一项）
	ServerAddresses  []string // 集群各副本地址；worker 自己容错，不再必须挂 LB/VIP
	AppId            string
	Password         string
	Address          string // 本节点对外标识，默认 hostname-pid
	MaxConcurrency   int    // 同时执行的任务上限，默认 16
	HeartbeatSeconds int    // 默认 5
	MaxWaitSeconds   int    // 长轮询阻塞时长，默认 30
	Tag              string // 机器标签，供 SPECIFY 分发策略 tagIn/tagEquals 匹配
	// AesKey 注册口令加密的 key（64 位 hex，AES-256），须与服务端 GOJOB_WORKER_AES_KEYS
	// 中的某一把一致。配了它，项目口令就不再出现在链路上（注册走一次性 challenge +
	// AES-256-GCM，见 worker/registercipher.go）；留空则口令明文送上来。
	// 配了 key 而服务端未启用加密 ⇒ 注册失败并指出是哪一侧没配，不会静默退回明文。
	AesKey string
}

func (c *Config) applyDefaults() {
	if c.MaxConcurrency <= 0 {
		c.MaxConcurrency = 16
	}
	if c.HeartbeatSeconds <= 0 {
		c.HeartbeatSeconds = 5
	}
	if c.MaxWaitSeconds <= 0 {
		c.MaxWaitSeconds = 30
	}
	if c.Address == "" {
		host, _ := os.Hostname()
		c.Address = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
}

// ownerConfirmBeats 连续看到同一个 owner 多少跳才迁移长轮询。
// 取 2 的理由见 applyHint 规则 4：宁可慢一跳，也不要被一个还没刷到租约的副本甩走。
const ownerConfirmBeats = 2

// beatTimeout 单次心跳上限。服务端阻塞上限远小于它，所以它只用来兜住"对端不收口"这一种情况。
const beatTimeout = 15 * time.Second

// fetchGrace 长轮询在服务端最大阻塞时间之外允许的网络余量；服务端是 wait+5s 收口，给 15s 防抖。
const fetchGrace = 15 * time.Second

// normalizeServers 把单地址与地址列表合成一个去重、去尾斜杠的有序列表（首次启动的首选顺序即此顺序）。
func normalizeServers(single string, list []string) []string {
	out := make([]string, 0, len(list)+1)
	seen := make(map[string]bool, len(list)+1)
	for _, raw := range append([]string{single}, list...) {
		s := strings.TrimRight(strings.TrimSpace(raw), "/")
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// Worker 一个执行层节点实例。
type Worker struct {
	cfg          Config
	servers      []string // 配置的副本地址（applyHint 之后 known 集合会比它大）
	instanceID   string
	memMaxMB     float64       // 本机/容器内存上限，0=未知
	cpuCores     int           // 可用核数
	sem          chan struct{} // 并发额度
	slotFree     chan struct{} // 缓冲 1：任一任务释放额度时 poke 一下，见 pollLoop
	receiving    atomic.Bool   // false=停止接收新任务（优雅下线中）
	client       *client
	logs         *logSender
	running      sync.WaitGroup
	tasks        sync.Map // taskID -> context.CancelFunc，服务端停止指令用
	stoppedTasks sync.Map // taskID -> true，标记该任务是"被停止"而非"失败"

	// 数据面凭证：启动时用项目口令注册换取，之后只带 token 不带口令。
	// 心跳会不断滑动续期，只有服务重启或本地静默过 TTL 才会失效，届时 401 自动重注册。
	tokMu       sync.Mutex
	token       string
	tokenExpire time.Time // 服务端给的有效期到点时刻，SDK 主动换票用
	tokenTTL    time.Duration
	regMu       sync.Mutex // 防多个 goroutine 同时 401 引发重注册风暴

	// 集群路由提示的状态：owner 要连续确认若干跳才切，见 applyHint。
	hintMu    sync.Mutex
	hintOwner string // 上一跳心跳看到的、且尚未迁移过去的 owner 地址
	hintSeen  int    // 该 owner 已连续出现的跳数

	// 正在途中的那一次长轮询。地址一变更就要掐断它（见 abortPoll），
	// 否则僵尸副本（SIGSTOP、网络黑洞）会把拉取循环挂满 wait+grace 才发现换了台机器。
	pollMu     sync.Mutex
	pollCancel context.CancelFunc

	// 进程 CPU 占用要算差值，这里存上次心跳的基线（只有心跳 goroutine 读写）
	cpuBase time.Duration
	cpuAt   time.Time
}

func New(cfg Config) *Worker {
	cfg.applyDefaults()
	servers := normalizeServers(cfg.ServerAddress, cfg.ServerAddresses)
	w := &Worker{
		cfg:        cfg,
		servers:    servers,
		instanceID: fmt.Sprintf("%s-%d", cfg.Address, time.Now().UnixNano()),
		memMaxMB:   memoryTotalMB(),
		cpuCores:   usableCores(),
		sem:        make(chan struct{}, cfg.MaxConcurrency),
		slotFree:   make(chan struct{}, 1),
		client:     newClient(servers),
	}
	w.logs = newLogSender(w)
	// 首选地址因为"那台没答上话"而变更时，立刻掐断挂在旧地址上的长轮询。
	w.client.onRotate = w.abortPoll
	return w
}

// setPollCancel 登记/注销当前在途的那次拉取，供 abortPoll 打断用。
func (w *Worker) setPollCancel(cancel context.CancelFunc) {
	w.pollMu.Lock()
	w.pollCancel = cancel
	w.pollMu.Unlock()
}

// abortPoll 放弃挂在旧地址上的长轮询。没有可掐的（不在拉取中）就是空操作，
// 所以从心跳协程调用是安全的；被掐断的那批任务由 server 侧 ReassignLostTasks 回收重派。
func (w *Worker) abortPoll() {
	w.pollMu.Lock()
	c := w.pollCancel
	w.pollCancel = nil
	w.pollMu.Unlock()
	if c != nil {
		log.Printf("gojob worker: server address changed, abandoning the in-flight long poll")
		c()
	}
}

// StopReceivingNewTasks 停止拉取新任务；已在执行的任务不受影响继续跑完。
// 再次调用 ResumeReceivingNewTasks 可恢复收单。
func (w *Worker) StopReceivingNewTasks() { w.receiving.Store(false) }

// ResumeReceivingNewTasks 恢复接收新任务。
func (w *Worker) ResumeReceivingNewTasks() { w.receiving.Store(true) }

// IsReceiving 当前是否仍在接收新任务。
func (w *Worker) IsReceiving() bool { return w.receiving.Load() }

// Run 启动心跳与拉取循环，阻塞至 ctx 取消，并等待运行中任务全部执行完。
func (w *Worker) Run(ctx context.Context) error {
	w.receiving.Store(true)
	log.Printf("gojob worker starting: app=%s addr=%s servers=%v", w.cfg.AppId, w.cfg.Address, w.servers)
	// 口令只在注册这一刻出场；换到 token 后所有数据面请求都不再带口令
	if err := w.registerAtStartup(ctx); err != nil {
		return fmt.Errorf("worker register failed: %w", err)
	}
	w.beat(ctx) // 先注册再开拉取，避免首次 fetch 撞上"无存活节点"
	go w.heartbeatLoop(ctx)
	go w.logs.run(ctx)
	w.pollLoop(ctx) // 阻塞直到 ctx 取消
	w.running.Wait()
	// 退出前把残留日志送出去（ctx 已取消，用独立超时上下文重试）
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	w.logs.flush(fctx)
	cancel()
	log.Printf("gojob worker drained: app=%s addr=%s", w.cfg.AppId, w.cfg.Address)
	return nil
}

// registerGiveUp 启动期注册对"连不上"的止损时间：服务端还没起来、网络抖一下都不该让
// 节点永久缺席，但一直连不上的环境也不该无限等下去——到点把最后一次的错误抛回调用方。
const registerGiveUp = 2 * time.Minute

// registerAtStartup 是 Run() 的第一步：注册失败时按"服务端答没答"分类处理。
//
//   - 服务端明确拒绝（口令不对、要求加密注册、没配 key）：重试不会让它变对，立即失败。
//   - 连不上（reset / refused / 超时）：退避重试。
//
// 为什么必须有第二条：注册是整条链路上唯一要过 bcrypt 的请求（校验项目口令），
// 一次几十毫秒 CPU。整池 worker 同时冷启动时，服务端的 accept 队列会被打满——
// 实测 200 节点同机启动，本机 kern.ipc.somaxconn=128，每轮稳定有 6~23 个节点
// 在 /worker/register 上吃到 connection reset。SDK 若就此退出，这些节点永久不上线，
// 而现场看起来"调度没问题"（其余节点跑得好好的），是最难发现的那一类故障。
// 退避带抖动：整池节点是同时启动的，不错峰就会在每个阶梯上再撞一次队列。
func (w *Worker) registerAtStartup(ctx context.Context) error {
	ladder := []time.Duration{200 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second}
	deadline := time.Now().Add(registerGiveUp)
	for attempt := 0; ; attempt++ {
		err := w.register(ctx)
		if err == nil {
			if attempt > 0 {
				log.Printf("gojob worker: 注册在第 %d 次尝试后成功", attempt+1)
			}
			return nil
		}
		var se *serverError
		if errors.As(err, &se) {
			return err // 服务端答了，只是答"不行"：不重试
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("注册持续连不上（重试 %d 次，超过 %v 止损）: %w", attempt+1, registerGiveUp, err)
		}
		wait := ladder[min(attempt, len(ladder)-1)]
		wait += rand.N(wait / 2)
		if wait > left {
			wait = left
		}
		log.Printf("gojob worker: 注册连不上(%v)，%v 后重试", err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// ---------- 数据面凭证 ----------

func (w *Worker) currentToken() string {
	w.tokMu.Lock()
	defer w.tokMu.Unlock()
	return w.token
}

// register 用配置里的项目口令换一张新 token。配了 AesKey 时口令不再明文上线，
// 改走"一次性 challenge + AES-256-GCM"握手（见 registercipher.go）。
func (w *Worker) register(ctx context.Context) error {
	req := &protocol.RegisterReq{
		InstanceId: w.instanceID, AppId: w.cfg.AppId,
		Address: w.cfg.Address, Version: sdkVersion, SdkType: "GO",
	}
	if w.cfg.AesKey == "" {
		req.AuthScheme, req.Password = registercrypto.SchemePlain, w.cfg.Password
	} else if err := w.sealRegister(ctx, req); err != nil {
		return err
	}
	resp, err := post[protocol.RegisterResp](w.client, ctx, "/worker/register", req)
	if err != nil {
		var se *serverError
		if errors.As(err, &se) && se.Code == protocol.CodeChallengeRequired {
			// 服务端只接受加密注册，而本节点发的是明文——原因在配置，不在口令。
			return fmt.Errorf("%w（服务端已强制加密注册，请给 worker.Config.AesKey 配一把与服务端一致的 key）", err)
		}
		return err
	}
	ttl := time.Duration(resp.Data.TtlSeconds) * time.Second
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	w.tokMu.Lock()
	w.token, w.tokenTTL = resp.Data.Token, ttl
	w.tokenExpire = time.Now().Add(ttl)
	w.tokMu.Unlock()
	log.Printf("worker token issued for instance %s (ttl %ds, encrypted=%t)",
		w.instanceID, resp.Data.TtlSeconds, req.AuthScheme == registercrypto.SchemeAesGcm)
	return nil
}

// refresh 重新注册；used 是刚才被拒的那张 token。若已有别的 goroutine 换过票，
// 直接复用新票，避免一次 401 引发 N 次注册。
func (w *Worker) refresh(ctx context.Context, used string) error {
	w.regMu.Lock()
	defer w.regMu.Unlock()
	if cur := w.currentToken(); cur != "" && cur != used {
		return nil
	}
	return w.register(ctx)
}

// renewIfNeeded 主动换票。服务端令牌是无状态签名的、到点即失效（没有可续的会话记录），
// 所以"不掉线"只能由持有方在有效期将尽时重新注册来保证——放在心跳里最省事，
// 也避免节点在 10 分钟边界上集体吃一次 401 再重注册。
// 阈值给 1/4 剩余：留出至少一次重试的余量，注册失败下次心跳还会再来。
func (w *Worker) renewIfNeeded(ctx context.Context) {
	w.tokMu.Lock()
	exp, ttl := w.tokenExpire, w.tokenTTL
	w.tokMu.Unlock()
	if exp.IsZero() || time.Until(exp) > ttl/4 {
		return
	}
	w.regMu.Lock()
	defer w.regMu.Unlock()
	log.Printf("worker token nearing expiry (剩余 %v)，主动重注册", time.Until(exp).Round(time.Second))
	if err := w.register(ctx); err != nil {
		log.Printf("worker proactive re-register: %v", err) // 不致命：真过期了还有 401 自愈那条路
	}
}

// call 带 token 发一次数据面请求；服务端 401（重启/过期）时就地重注册并重放一次。
func call[T any](w *Worker, ctx context.Context, path string, body protocol.WorkerTokenBearer) (*protocol.Resp[T], error) {
	tk := w.currentToken()
	body.SetWorkerToken(tk)
	resp, err := post[T](w.client, ctx, path, body)
	if !isAuthRejected(err) {
		return resp, err
	}
	log.Printf("%s: worker token rejected (%v), re-registering", path, err)
	if rerr := w.refresh(ctx, tk); rerr != nil {
		return nil, errors.Join(err, rerr)
	}
	body.SetWorkerToken(w.currentToken())
	return post[T](w.client, ctx, path, body)
}

// Run 便捷入口：New + Run。
func Run(ctx context.Context, cfg Config) error { return New(cfg).Run(ctx) }

// heartbeatLoop 心跳循环，间隔带 ±25% 相位抖动。
//
// 抖动不是装饰：心跳是 worker 侧唯一高频写库的请求（每节点每跳 upsert 一行 worker_nodes）。
// 一整池节点同时冷启动（compose up / k8s 滚动发布）后，它们会永远同相位，每 5 秒
// **齐射**一次。sqlite 只有一个写者，实测 200 节点时单条心跳 upsert 被排到 237~338ms
// （服务端日志直接打成 SLOW SQL），而任务认领同样是写事务，排在它后面就被顶到 1 秒开外
// ——广播的 p50 从 60ms 变成整轮 1s。把相位打散，齐射就摊成匀速。
func (w *Worker) heartbeatLoop(ctx context.Context) {
	interval := time.Duration(w.cfg.HeartbeatSeconds) * time.Second
	for {
		wait := interval
		if interval >= 2*time.Second { // 间隔短到这个量级就别抖（rand.N(0) 会 panic）
			wait += rand.N(interval/2) - interval/4
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		w.beat(ctx)
	}
}

func (w *Worker) beat(ctx context.Context) {
	req := &protocol.HeartbeatReq{
		WorkerInfo: protocol.WorkerInfo{
			InstanceId: w.instanceID, AppId: w.cfg.AppId, Address: w.cfg.Address,
			Version: sdkVersion, SdkType: "GO", MemoryUsed: heapInuseMB(),
			MemoryMax: w.memMaxMB, CpuProcessors: w.cpuCores,
			CpuLoad: loadAverage1m(), Tag: w.cfg.Tag,
		},
	}
	w.renewIfNeeded(ctx)
	w.fillRuntime(&req.WorkerInfo)
	// 心跳必须自带短超时：进程被 SIGSTOP 冻结（长 GC、虚机挂起）时 TCP 连接不会断，
	// 请求只是不返回，靠 hc 的 150s 兜底会让 worker 挂在那里——而集群路由提示正是从心跳
	// 响应下来的，挂住就等于新 owner 永远追不上。15s 足够慢查询容忍，又远短于一次租约交接。
	bctx, cancel := context.WithTimeout(ctx, beatTimeout)
	defer cancel()
	resp, err := call[protocol.HeartbeatResp](w, bctx, "/worker/heartbeat", req)
	if err != nil {
		log.Printf("heartbeat: %v", err)
		return
	}
	w.cancelTasks(resp.Data.CancelTaskIds)
	w.applyHint(resp.Data.Cluster)
}

// applyHint 处理服务端下发的集群路由提示，回答"worker 如何选择并确认 owner"：
//
//  1. servers 一律并入已知地址集合——这是 worker 感知"集群后来又起了一个副本"的唯一来源；
//  2. owner 为空 = 没有可用提示（单机部署、租约正在交接、或对方没配对外可达地址），保持现状；
//  3. owner 就是当前地址 = 已确认，清零计数（这一步同时是"确认"动作：我们只信这个副本自己
//     也认为 owner 是谁，而不是只看一次别人的转述）；
//  4. owner 与当前地址不同：必须它已在已知集合内、且连续 ownerConfirmBeats 跳都指向它，
//     才把首选地址迁过去。单跳不动作，是为了不让一台还没刷到新租约的副本把 worker 甩来甩去；
//  5. 迁过去之后若连不上，send 的故障转移会自动退回其它已知地址——owner 提示不承重，
//     所以这里不需要任何"回滚 owner"的状态。
//
// 迁移只影响下一个拉取周期：正阻塞在长轮询上的那次请求仍打到旧地址，任务照常送达。
func (w *Worker) applyHint(h protocol.ClusterHint) bool {
	if len(h.Servers) > 0 {
		if added := w.client.note(h.Servers); added > 0 {
			log.Printf("gojob worker: learned %d more server(s) from cluster hint: %v", added, h.Servers)
		}
	}
	w.hintMu.Lock()
	defer w.hintMu.Unlock()
	if h.Owner == "" || h.Owner == w.client.current() {
		// 没有提示，或本来就在跟 owner 说话：清零计数，什么都不做
		w.hintOwner, w.hintSeen = "", 0
		return false
	}
	if h.Owner != w.hintOwner {
		w.hintOwner, w.hintSeen = h.Owner, 1 // 第一跳看到新 owner：只记下，不动作
		return false
	}
	if w.hintSeen+1 < ownerConfirmBeats {
		w.hintSeen++
		log.Printf("gojob worker: cluster hint owner=%s (%d/%d 确认中，暂不迁移)", h.Owner, w.hintSeen, ownerConfirmBeats)
		return false
	}
	if !w.client.jumpTo(h.Owner) {
		log.Printf("gojob worker: cluster hint owner=%s 还不在已知副本集合里，本次忽略（下一跳再确认）", h.Owner)
		w.hintOwner, w.hintSeen = "", 0
		return false
	}
	log.Printf("gojob worker: owner %s 已连续 %d 跳确认，长轮询迁过去", h.Owner, ownerConfirmBeats)
	w.hintOwner, w.hintSeen = "", 0
	w.abortPoll() // 挂在旧 owner 上的那次拉取立刻让位，不等它自然到点
	return true
}

func (w *Worker) pollLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if !w.IsReceiving() { // 优雅下线：只等 ctx，不再拉任务
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		if len(w.sem) == cap(w.sem) { // 无空余并发额度
			// 睡到"有额度了"为止，1s 只是兜底。原先是死睡 1 秒：一台 16 额度的节点
			// 排 200 个 50ms 的任务，就要在批次之间白等 12 次 × 1s，实测整轮 11s；
			// 改成事件驱动后同一场景 0.7s 量级（见 README「容量实机数字」）。
			t := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-w.slotFree:
				t.Stop()
			case <-t.C:
			}
			continue
		}
		req := &protocol.FetchTaskReq{
			InstanceId: w.instanceID, AppId: w.cfg.AppId,
			Address: w.cfg.Address, MaxTaskNum: cap(w.sem) - len(w.sem),
			WaitSeconds: w.cfg.MaxWaitSeconds,
		}
		// 长轮询的超时 = 服务端最长阻塞时间 + 余量。没有它，被冻结的副本会把整个拉取循环
		// 挂住 150s（hc 的兜底），期间既拿不到任务也收不到集群提示。
		// 超时后被放弃的那一批任务已在 server 侧记为本节点持有：本节点失联时由
		// ReassignLostTasks 回收重派；本节点一直在线、只是这一次响应没送达的任务不会
		// 自动重派，会停在 RUNNING 等人工停止——服务端分不清"响应丢了"和"跑得久"，
		// 宁可卡住也不并发双跑。
		fctx, cancel := context.WithTimeout(ctx,
			time.Duration(req.WaitSeconds)*time.Second+fetchGrace)
		w.setPollCancel(cancel)
		resp, err := call[protocol.FetchTaskResp](w, fctx, "/worker/fetchTask", req)
		w.setPollCancel(nil)
		cancel()
		if err != nil {
			log.Printf("fetchTask: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if len(resp.Data.CancelTaskIds) > 0 {
			w.cancelTasks(resp.Data.CancelTaskIds)
		}
		for _, task := range resp.Data.Tasks {
			w.execute(ctx, task)
		}
	}
}

func (w *Worker) execute(ctx context.Context, task protocol.TaskDTO) {
	w.sem <- struct{}{}
	w.running.Add(1)
	go func() {
		defer func() {
			<-w.sem
			// 额度一释放就喊一声：pollLoop 那台"没额度就不发 fetch"的等待靠它醒来，
			// 不用固定再睡 1 秒（缓冲 1 + 非阻塞发：多次释放合并成一次唤醒即可）
			select {
			case w.slotFree <- struct{}{}:
			default:
			}
			w.running.Done()
		}()
		// 每个任务一条可取消 ctx：服务端的停止指令经 cancelTasks 打到这里
		tctx, cancel := context.WithCancel(ctx)
		w.tasks.Store(task.TaskId, cancel)
		defer func() {
			cancel()
			w.tasks.Delete(task.TaskId)
			w.stoppedTasks.Delete(task.TaskId)
		}()
		logger := newTaskLogger(w.logs, task)
		var result string
		var err error
		if tctx.Err() != nil {
			// 停止指令在开跑前就到了：处理器一次都不执行。Java SDK 的
			// "cancelled before start" 一直有这道短路，Go 侧缺的话，一个完全不检查
			// ctx 的处理器会在"已经被判停止"的任务上真把业务副作用做一遍。
			result = "cancelled before start"
			err = errors.New(result)
		} else {
			result, err = w.runProcessor(tctx, task, logger)
		}
		stoppedByServer := w.stopRequested(task.TaskId)
		if err != nil {
			if stoppedByServer {
				logger.output("WARN", "task stopped by server request: %v", err)
			} else {
				logger.output("ERROR", "task failed: %v (attempt %d/%d)", err, task.Attempts, task.Attempts+task.RetryTimes)
			}
		}
		// 先保证本任务日志送达，再上报结果——控制台看到终态时日志一定已可查
		fctx, cancel2 := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		logger.flush(fctx)
		cancel2()
		report := protocol.ReportTaskReq{
			InstanceId: w.instanceID, AppId: w.cfg.AppId,
			TaskId: task.TaskId, Success: err == nil && !stoppedByServer,
		}
		switch {
		case stoppedByServer:
			report.Status = protocol.TaskStopped
			// 处理器自己留了话就以它为准（"cancelled before start" 这类），
			// 与 Java SDK 回报 result=ProcessResult.msg 的口径一致；没话才给通用文案。
			report.Result = result
			if report.Result == "" {
				report.Result = "stopped by server request"
			}
		case err != nil:
			// 失败原因优先：处理器既 SetResult 又返回 error 时，留在结果里的是原因
			report.Result = err.Error()
			log.Printf("task %d failed: %v", task.TaskId, err)
		default:
			// 没调 SetResult 就是空串：兼容协议的 ProcessResult.msg 同样允许为空
			report.Result = result
		}
		rctx, cancel3 := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel3()
		if e := w.reportWithRetry(rctx, report); e != nil {
			log.Printf("report task %d: %v", task.TaskId, e)
		}
	}()
}

// stopRequested 该任务是否被服务端下达过停止指令（决定回报 STOPPED 还是 FAILED）。
func (w *Worker) stopRequested(taskID uint64) bool {
	_, ok := w.stoppedTasks.Load(taskID)
	return ok
}

// cancelTasks 执行服务端夹带下来的停止指令。取消是协作式的：
// 处理器必须检查 ctx.Done()，否则会一直跑到自然结束（那时回报仍记为 STOPPED）。
func (w *Worker) cancelTasks(ids []uint64) {
	for _, id := range ids {
		v, ok := w.tasks.Load(id)
		if !ok { // 不在本机执行（已完成或从未领取）→ 忽略，server 侧靠 ack 清理
			continue
		}
		if _, done := w.stoppedTasks.Load(id); done {
			continue // 重复下发：本轮已经通知过，别再刷一次日志（标记随本次执行结束清掉）
		}
		w.stoppedTasks.Store(id, true)
		v.(context.CancelFunc)()
		log.Printf("task %d: stop requested by server, cancelling", id)
	}
}

// reportWithRetry 结果回报退避重试（sqlite BUSY/瞬时网络错误不应该丢任务终态）。
func (w *Worker) reportWithRetry(ctx context.Context, report protocol.ReportTaskReq) error {
	var err error
	for i, wait := range []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if i > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(wait):
			}
		}
		if _, err = call[struct{}](w, ctx, "/worker/report", &report); err == nil {
			return nil
		}
	}
	return err
}

// runProcessor 执行一个任务，返回"要上报的结果文本"与错误。
//
// reduce 任务（兼容协议的 OMS_LAST_TASK）走单独一条分支：先取满全部子任务结果，
// 再调处理器的 Reduce。处理器没实现 Reducer 就明确报错——静默跳过等于实例凭空变绿。
func (w *Worker) runProcessor(ctx context.Context, task protocol.TaskDTO, logger *taskLogger) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("processor panic: %v", r)
		}
	}()
	// EXTERNAL 在兼容协议里意味着"去 JAR 动态容器里按 容器ID#类名 加载"，gojob 没有对应实现。
	// 早一步说清楚，比报"没注册"更好排查（兼容协议的那句 "fetch Processor failed, please
	// check your processorType and processorInfo config"）。
	if task.ProcessorType == protocol.ProcessorExternal {
		return "", fmt.Errorf("processorType=EXTERNAL is not supported by gojob worker (the compat protocol loads it from a JAR container as containerID#className); implement/register it on the worker and use BUILT_IN")
	}
	p, ok := lookupProcessor(task.Processor)
	if !ok {
		return "", fmt.Errorf("processor %q not registered on this worker", task.Processor)
	}
	tctx := &TaskContext{
		Context: ctx, TaskID: task.TaskId, JobID: task.JobId,
		InstanceID: task.InstanceId, JobParams: task.JobParams,
		InstanceParams: task.FixedParams, Attempts: task.Attempts,
		TaskKey: task.TaskKey, TaskName: task.TaskName, ExecuteType: task.ExecuteType,
		worker: w, subTaskContent: task.SubTaskContent,
		logger: logger,
	}
	if task.TaskName == protocol.LastTaskName {
		r, ok := p.(Reducer)
		if !ok {
			return "", fmt.Errorf("processor %q 未实现 worker.Reducer，但任务执行类型是 MAP_REDUCE", task.Processor)
		}
		results, e := tctx.SubTaskResults()
		if e != nil {
			return "", fmt.Errorf("取子任务结果失败: %w", e)
		}
		if e := r.Reduce(tctx, results); e != nil {
			return "", e
		}
		return tctx.result, nil
	}
	if e := p.Process(tctx); e != nil {
		return "", e
	}
	return tctx.result, nil
}

func heapInuseMB() float64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapInuse) / (1 << 20)
}

// ---------- HTTP 客户端（encoding/json/v2 + 泛型） ----------

// client 是"已知副本地址集合 + 当前首选地址"。地址有三个来源：本地配置的列表、
// 心跳下发的 cluster_hint.servers、以及 owner 提示。任何一次"这个副本没答上话"都会立刻
// 换下一个已知地址重放，所以多副本部署不再强制要求 LB/VIP：
// worker 手里只要还剩一个活着的副本，就照常收单、回报、上报日志。
type client struct {
	mu    sync.Mutex
	cur   string
	known []string // 含 cur；rotateFrom 按此顺序循环
	hc    *http.Client
	// onRotate 在首选地址因为"这台没答上话"而变更后被调用（不带 client 锁）。
	// worker 用它掐断挂在旧地址上的那一次长轮询——否则僵尸副本要等满 wait+grace 才摆脱。
	onRotate func()
}

func newClient(addrs []string) *client {
	c := &client{hc: &http.Client{Timeout: 150 * time.Second}}
	for _, a := range addrs {
		c.note([]string{a})
	}
	if len(c.known) > 0 {
		c.cur = c.known[0]
	}
	return c
}

// current 当前首选地址（心跳与长轮询都发往它）。
func (c *client) current() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// note 把地址并进已知集合（去重、去尾斜杠），返回新增个数。
func (c *client) note(addrs []string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	added := 0
	for _, raw := range addrs {
		a := strings.TrimRight(strings.TrimSpace(raw), "/")
		if a == "" {
			continue
		}
		if slices.Contains(c.known, a) {
			continue
		}
		c.known = append(c.known, a)
		added++
	}
	return added
}

// rotateFrom 把首选地址换成"失败那台的下一台"（循环）并返回它；只有一个地址时原地返回。
//
// 必须按 base 而不是按 cur 来挪：心跳与长轮询共用这一个首选地址，如果谁失败就从 cur 往后挪，
// 两个协程交错时会在 A→B→A 之间来回弹（实测过：心跳把地址挪到 B，紧接着一笔挂在 A 上
// 超时的旧拉取又把 B 挪回 A）。失败者不在集合里（早被淘汰）则不动 cur。
func (c *client) rotateFrom(base string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.known) > 1 {
		if i := slices.Index(c.known, base); i >= 0 {
			c.cur = c.known[(i+1)%len(c.known)]
		} else if c.cur == base { // 不在集合里还又是当前地址：挑一个别的顶上
			c.cur = c.known[0]
		}
	}
	return c.cur
}

// jumpTo 直接把首选地址设为 a（owner 提示用）；a 不在集合里则视为无效跳转。
func (c *client) jumpTo(a string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.known, a) {
		return false
	}
	c.cur = a
	return true
}

// send 发一次请求。只在"这个副本没答上话"时换副本重放，最多把已知集合走一遍。
func (c *client) send(ctx context.Context, path string, bs []byte) ([]byte, error) {
	c.mu.Lock()
	attempts := len(c.known)
	c.mu.Unlock()
	if attempts == 0 {
		return nil, errors.New("worker: no server address configured")
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		base := c.current()
		body, err := c.raw(ctx, base, path, bs)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !replicaUnreachable(err) {
			return nil, err
		}
		next := c.rotateFrom(base)
		if c.onRotate != nil {
			c.onRotate() // 挂在旧地址上的那一次长轮询没有意义了，掐断它
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// 整轮预算已经花光（心跳 15s、长轮询 wait+15s 都是"这一轮"的额度）：
			// 带着一个到点的 ctx 去撞下一台只会立刻失败，还白留一条误导日志。
			// 这里只把首选地址换掉，让调用方的重试循环用新预算去打下一台。
			log.Printf("gojob worker: server %s 本轮超时，下一轮改由 %s 服务", base, next)
			return nil, err
		}
		log.Printf("gojob worker: server %s unreachable (%v), fail over to %s", base, err, next)
	}
	return nil, lastErr
}

func (c *client) raw(ctx context.Context, base, path string, bs []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(bs))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{Status: resp.StatusCode, Body: string(body)}
	}
	return body, nil
}

// replicaUnreachable 是否属于"换一台副本可能就好了"：连接失败、对端不收口（超时）、5xx。
// 进程被 SIGSTOP 冻结的副本是这一类里最阴的一种——TCP 连着、请求发出、永远不返回，
// 只有超时能认出它，所以超时必须算"换一台"，否则 worker 会一直挂在僵尸副本上。
// 4xx 是本副本对这次请求的明确答复（换副本结果一样）；主动取消是 worker 自己在退出，也不重放。
func replicaUnreachable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		return se.Status >= 500
	}
	return true
}

// httpStatusError 非 200 响应；5xx 与 4xx 的区别见 replicaUnreachable。
type httpStatusError struct {
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("server %d: %s", e.Status, e.Body)
}

// post 发送请求并解析 Resp[T]；T 为 data 字段类型。
func post[T any](c *client, ctx context.Context, path string, body any) (*protocol.Resp[T], error) {
	bs, err := v2.Marshal(body)
	if err != nil {
		return nil, err
	}
	respBody, err := c.send(ctx, path, bs)
	if err != nil {
		return nil, err
	}
	var r protocol.Resp[T]
	if err := v2.Unmarshal(respBody, &r); err != nil {
		return nil, err
	}
	if r.Code != protocol.CodeOK {
		return nil, &serverError{Code: r.Code, Message: r.Message}
	}
	return &r, nil
}

// serverError 服务端信封 code != 0 的业务错误，SDK 按 code 区分"可重注册恢复"与真失败。
type serverError struct {
	Code    int
	Message string
}

func (e *serverError) Error() string { return fmt.Sprintf("server error(%d): %s", e.Code, e.Message) }

// isAuthRejected token 被拒（未注册/已过期/服务端重启），换一张新 token 即可恢复。
func isAuthRejected(err error) bool {
	var se *serverError
	return errors.As(err, &se) && se.Code == protocol.CodeAuthErr
}
