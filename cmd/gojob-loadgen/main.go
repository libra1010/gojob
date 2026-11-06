// gojob-loadgen：容量与调度延迟的实机发生器（验证工具，不是业务示例）。
//
// 它在一台机器上用 N 个 goroutine 起 N 个**真 worker 节点**（各自注册、各自换票、
// 各自长轮询、各自回报），所以"200 个节点在线"这件事是压出来的，不是换算出来的。
// 走的完全是 SDK 的真实代码路径，只有处理器函数什么都不做。
//
// 度量两个量：
//   - 派发延迟：从"打完触发请求"到"任务在某节点上开跑"（含入队、唤醒、认领、HTTP 往返）；
//   - 收尾跨度：从触发到"全部任务都跑完并回报完"。
//
// 典型用法（项目与任务由 scripts/verify-capacity.sh 准备好）：
//
//	GOJOB_SESSION_TOKEN=xxx go run ./cmd/gojob-loadgen \
//	    -server http://127.0.0.1:7800 -app cap_app -pw cap123 \
//	    -nodes 200 -job 1 -rounds 5 -hold 20s
//
// -job 指向一个 BROADCAST 任务时，"一轮"就是"每个在线节点一份"；
// -rounds 是连打几轮（第一轮是冷启动，统计里会单独看）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/worker"
)

// 计量的承重处：**只认本轮实例的任务**。
// 早先的版本按"重置计数器 + 数满 N 个"来分轮，结果上一轮的尾巴（以及并发在途的那一次
// 长轮询）会落进下一轮，量出来的分位数比真实值差一个数量级还不矛盾地自相洽
// （出现过 p50=2.1s 而"全部跑完=54ms"）。一轮广播 = 一个新实例，实例号就是天然的轮次标签。
var (
	mu        sync.Mutex
	latency   []time.Duration // 触发 → 任务开跑（仅本轮实例）
	done      int             // 本轮实例已跑完的任务数
	triggerAt time.Time
	// floorInst = 本轮触发**之前**该任务已有的最大实例号；InstanceID 比它大的才算本轮。
	// hasFloor=false 表示该任务此前一次实例都没有（首轮），此时本轮就是唯一一轮。
	floorInst uint64
	hasFloor  bool
)

// noopProcessor 什么都不做：进函数记一次"开跑"，出函数记一次"跑完"。
//
// -split>0 时它兼任 MAP 任务的根：根任务一次派生 split 个子任务就返回，**根那一跳不计入
// 延迟**（它不是"业务任务开跑"，它是发牌的那次）。这样"一个 worker 同时跑 200 个任务"
// 这件事就有了可测形态：一个实例一棵树，200 行子任务全落在同一台节点的空闲额度上。
type noopProcessor struct {
	work  time.Duration
	split int
}

func (p noopProcessor) Process(ctx *worker.TaskContext) error {
	if p.split > 0 && ctx.IsRootTask() {
		items := make([]any, p.split)
		for i := range items {
			items[i] = map[string]any{"shard": i}
		}
		if _, err := ctx.Map(items, "loadgen-sub"); err != nil {
			die("派生 %d 个子任务失败: %v", p.split, err)
		}
		return nil
	}
	mu.Lock()
	mine := !hasFloor || ctx.InstanceID > floorInst
	if mine {
		latency = append(latency, time.Since(triggerAt))
	}
	mu.Unlock()
	if p.work > 0 {
		time.Sleep(p.work)
	}
	mu.Lock()
	if mine {
		done++
	}
	mu.Unlock()
	return nil
}

// die 打印致命错误并退出。**不能**用 log.Fatal：本工具为压测静音了标准 log
// （否则 200 个节点的 SDK 日志会盖掉结果），log.Fatal 会连错误一起吞掉，
// 只留一个 exit=1 —— 排查过一次"没有轮次输出"就是这个原因。
func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "loadgen: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	server := flag.String("server", "http://127.0.0.1:7800", "Server 地址，逗号分隔多副本")
	app := flag.String("app", "", "项目 ID")
	pw := flag.String("pw", "", "项目口令")
	job := flag.Uint64("job", 0, "要触发的任务 ID（BROADCAST 型：一份/节点）")
	nodes := flag.Int("nodes", 200, "起多少个 worker 节点")
	concurrency := flag.Int("concurrency", 1, "每节点并发额度")
	split := flag.Int("split", 0, ">0 时任务改成 MAP：根一次派生这么多子任务（测一台节点同时跑 N 个任务）")
	rounds := flag.Int("rounds", 1, "连打几轮触发")
	work := flag.Duration("work", 0, "每个任务模拟耗时（0=纯派发延迟）")
	hold := flag.Duration("hold", 30*time.Second, "每轮最多等多久")
	lead := flag.Duration("lead", 0, "首轮触发前静置多久（供外部采空闲开销）")
	sdklog := flag.String("sdklog", "", ">空时把 SDK 日志写到这里（查个别节点迟到原因）")
	idle := flag.Duration("idle", 0, ">0 时不触发，只挂机这么久（统计在线开销）")
	flag.Parse()

	if *app == "" || *pw == "" {
		log.Fatal("-app 与 -pw 必填")
	}
	servers := strings.Split(*server, ",")
	worker.Register("loadgen", noopProcessor{work: *work, split: *split})

	// SDK 的 log 会在 200 个节点下把结果淹掉，默认整体静音（错误仍走返回值）；
	// 但要查"某些节点为什么慢了 2 秒"就得看得见 SDK 的 fetchTask 错误，所以留一个落盘口子。
	if *sdklog != "" {
		f, err := os.Create(*sdklog)
		if err != nil {
			die("打开 -sdklog 失败: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	} else {
		log.SetOutput(io.Discard)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := time.Now()
	for i := range *nodes {
		w := worker.New(worker.Config{
			ServerAddresses: servers,
			AppId:           *app,
			Password:        *pw,
			Address:         fmt.Sprintf("10.244.%d.%d:7799", i/256, i%256),
			MaxConcurrency:  *concurrency,
			Tag:             fmt.Sprintf("gen-%d", i), // 每节点唯一，便于确认"确实各起了一个"
		})
		go func() {
			if err := w.Run(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "worker exited: %v\n", err)
			}
		}()
	}
	fmt.Printf("nodes=%d started_in=%v\n", *nodes, time.Since(started).Round(time.Millisecond))

	if *idle > 0 {
		time.Sleep(*idle)
		return
	}
	if *job == 0 {
		fmt.Println("没给 -job：只起节点不触发（在线开销见服务端节点监控）。-h 看用法。")
		return
	}
	waitOnline(*server, *app, *nodes, 90*time.Second)
	time.Sleep(500 * time.Millisecond) // 让最后上线的节点把首跳心跳落库
	if *lead > 0 {
		// 静置窗口：让外部脚本在"节点全都在线、但一次都没触发"的状态下采服务端开销，
		// 否则采到的是压测进行中的数字，空闲成本这条线就没法看了。
		time.Sleep(*lead)
	}

	// 一轮该收到多少条任务：MAP 是 split 个子任务（与节点数无关，测的就是"一台节点同时跑
	// N 个"），BROADCAST 是每台一份。
	tasksPerRound := *nodes
	if *split > 0 {
		tasksPerRound = *split
	}
	for r := range *rounds {
		if r > 0 {
			time.Sleep(2 * time.Second) // 轮次之间留白，别把上一轮的尾巴算进来
		}
		runRound(*server, *job, tasksPerRound, *hold, r+1)
	}
}

func runRound(base string, jobID uint64, nodes int, hold time.Duration, round int) {
	// 先记下"本轮之前的最大实例号"，再触发。顺序反了这一轮的任务会被全部判成上一轮的。
	// 该任务一次实例都没有（首轮）时没有基线可用，此时本轮即唯一一轮，全部计入。
	prev, err := latestInstance(base, jobID)
	if err != nil {
		die("取不到实例号基线，无法把任务归属到轮次: %v", err)
	}
	mu.Lock()
	latency, done = nil, 0
	floorInst, hasFloor = prev, prev != 0
	mu.Unlock()

	wait := trigger(base, jobID)

	deadline := time.Now().Add(hold)
	for {
		mu.Lock()
		n := done
		mu.Unlock()
		if n >= nodes {
			break
		}
		if time.Now().After(deadline) {
			fmt.Printf("round %d TIMEOUT: %d/%d 跑完\n", round, n, nodes)
			printStats(round, nodes, wait)
			os.Exit(1)
		}
		time.Sleep(10 * time.Millisecond)
	}
	printStats(round, nodes, wait)
}

func printStats(round, nodes int, httpWait time.Duration) {
	mu.Lock()
	ls := make([]time.Duration, len(latency))
	copy(ls, latency)
	finished := done
	mu.Unlock()
	sort.Slice(ls, func(i, j int) bool { return ls[i] < ls[j] })
	pct := func(p float64) time.Duration {
		if len(ls) == 0 {
			return 0
		}
		return ls[int(p*float64(len(ls)-1))]
	}
	fmt.Printf("round %d tasks=%d finished=%d sampled=%d trigger_http=%v | 派发延迟 p50=%v p90=%v p99=%v max=%v | 全部跑完=%v\n",
		round, nodes, finished, len(ls), httpWait.Round(time.Millisecond),
		pct(0.5).Round(time.Millisecond), pct(0.9).Round(time.Millisecond),
		pct(0.99).Round(time.Millisecond), lastOf(ls).Round(time.Millisecond),
		time.Since(triggerAt).Round(100*time.Microsecond))
}

func lastOf(s []time.Duration) time.Duration {
	if len(s) == 0 {
		return 0
	}
	return s[len(s)-1]
}

func waitOnline(base, app string, want int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	var n int
	for time.Now().Before(deadline) {
		n = onlineCount(base, app)
		if n >= want {
			fmt.Printf("online=%d/%d（等满才打，否则测的是启动等待而不是派发）\n", n, want)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	fmt.Printf("online=%d/%d 超时（继续，但结果要按实际在线数看）\n", n, want)
}

// onlineCount 查已在线的 worker 数。/openapi 全部要会话票（/worker/* 才用换到的 worker 票），
// 不带票会拿到 401，这里就会一直数出 0 —— 曾因此白等 90 秒。
func onlineCount(base, app string) int {
	req, err := http.NewRequest("GET", base+"/openapi/app/workers?app_id="+app, nil)
	if err != nil {
		return 0
	}
	req.Header.Set("X-Gojob-Token", os.Getenv("GOJOB_SESSION_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var r protocol.Resp[[]protocol.WorkerInfo]
	bs, err := io.ReadAll(resp.Body)
	if err != nil || v2.Unmarshal(bs, &r) != nil {
		return 0
	}
	return len(r.Data)
}

// trigger 用管理面触发一次运行（会话票从 GOJOB_SESSION_TOKEN 取），返回这次请求自身的往返耗时。
//
// 计时点打在**发出请求之前**：先前的版本打在响应回来之后，而任务完全可能在那之前就已经
// 被别的节点抢跑，于是读到的是上一轮的 triggerAt —— 量出 p50=2s（正好是轮次间隔）的假数，
// 首轮甚至溢出成负数。口径随之改为"从按下触发到任务开跑"，含管理面往返；
// 往返耗时单独打印（trigger_http），要净派发延迟就自己减掉。
func trigger(base string, jobID uint64) time.Duration {
	bs, _ := v2.Marshal(map[string]any{"job_id": jobID, "action": "trigger"})
	at := time.Now()
	mu.Lock()
	triggerAt = at
	mu.Unlock()
	req, err := http.NewRequest("POST", base+"/openapi/job/operate", strings.NewReader(string(bs)))
	if err != nil {
		die("%v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gojob-Token", os.Getenv("GOJOB_SESSION_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("trigger: %v", err)
	}
	defer resp.Body.Close()
	var out protocol.Resp[any]
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		die("trigger read: %v", err)
	}
	if err := v2.Unmarshal(raw, &out); err != nil {
		die("trigger decode: %v (%s)", err, raw)
	}
	if out.Code != 0 {
		die("trigger failed: code=%d msg=%s body=%s", out.Code, out.Message, raw)
	}
	return time.Since(at)
}

// latestInstance 取该任务最新一个实例的 id（instance/list 按 id 倒序返回）。
// 该任务还没有实例时返回 (0, nil)：这是首轮，不是错误。
func latestInstance(base string, jobID uint64) (uint64, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/openapi/instance/list?job_id=%d", base, jobID), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Gojob-Token", os.Getenv("GOJOB_SESSION_TOKEN"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var r protocol.Resp[[]instanceRow]
	if err := v2.Unmarshal(raw, &r); err != nil {
		return 0, err
	}
	if len(r.Data) == 0 {
		return 0, nil // 首轮：没有基线不等于失败
	}
	return r.Data[0].ID, nil
}

// instanceRow 只取需要的两列（不为压测工具引整个 store 包）。
type instanceRow struct {
	ID     uint64 `json:"id"`
	Status string `json:"status"`
}
