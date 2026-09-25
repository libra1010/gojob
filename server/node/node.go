// Package node 采集"本 server 进程"的运行指标（CPU / 内存 / GC / HTTP 连接），
// 供控制台「系统管理 → 节点监控」页展示。多副本部署时每个进程各自上报，
// 经 server_nodes 表聚合成一张跨节点视图。
package node

import (
	"context"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"gojob/internal/proc"
)

// HistoryLen 环形缓冲保留的采样点数；配合 ReportInterval=5s = 最近 10 分钟曲线。
const HistoryLen = 120

// Sample 一次采样快照。
type Sample struct {
	Time         int64   `json:"time"` // Unix 毫秒
	CPUPercent   float64 `json:"cpu_percent"`
	RssMb        float64 `json:"rss_mb"` // 0 = 本平台取不到
	HeapInuseMb  float64 `json:"heap_inuse_mb"`
	Goroutines   int     `json:"goroutines"`
	GcCount      uint32  `json:"gc_count"`
	GcPauseMs    float64 `json:"gc_pause_ms"` // 进程启动以来的累计 GC 暂停
	ConnsActive  int     `json:"conns_active"`
	ConnsIdle    int     `json:"conns_idle"`
	PollInflight int     `json:"poll_inflight"` // /worker/fetchTask 长轮询在途
	ReqInflight  int     `json:"req_inflight"`
	ReqPerSec    float64 `json:"req_per_sec"`
}

// Collector 累积式采集器。计数器走 atomic（中间件与 ConnState 在 http 各自的 goroutine 里调用），
// 连接状态表和环形缓冲用一把锁（每 5s 才读一次，竞争可忽略）。
type Collector struct {
	reqTotal atomic.Uint64
	reqIn    atomic.Int64
	pollIn   atomic.Int64

	mu       sync.Mutex
	states   map[net.Conn]bool // true=active，false=idle
	hist     []Sample
	hpos     int
	hfull    bool
	prevWall time.Time
	prevCPU  time.Duration
	prevReqs uint64
}

func New() *Collector {
	c := &Collector{states: make(map[net.Conn]bool), hist: make([]Sample, 0, HistoryLen)}
	c.prevWall = time.Now()
	c.prevCPU, _ = proc.CPUTime() // 基线：首帧差值从这里起算
	return c
}

// ConnState 挂到 http.Server.ConnState。
// 长轮询会让连接长期停在 active，所以活跃连接数要结合 poll_inflight 才看得清"挂了多少 worker"。
func (c *Collector) ConnState(nc net.Conn, state http.ConnState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch state {
	case http.StateActive:
		c.states[nc] = true
	case http.StateIdle:
		c.states[nc] = false
	default: // StateClosed / StateHijacked：连接不再属于本 server
		delete(c.states, nc)
	}
}

func (c *Collector) EnterRequest() { c.reqTotal.Add(1); c.reqIn.Add(1) }
func (c *Collector) LeaveRequest() { c.reqIn.Add(-1) }
func (c *Collector) EnterPoll()    { c.pollIn.Add(1) }
func (c *Collector) LeavePoll()    { c.pollIn.Add(-1) }

// Sample 采一次快照并写入环形缓冲。
func (c *Collector) Sample() Sample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	var gc debug.GCStats
	debug.ReadGCStats(&gc) // Go 1.27 已删 runtime.GCStats/ReadGCStats，只有 debug 这份

	cpuNow, cpuOK := proc.CPUTime()
	wall := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	s := Sample{
		Time:         wall.UnixMilli(),
		RssMb:        proc.RSSMb(),
		HeapInuseMb:  float64(ms.HeapInuse) / 1024 / 1024,
		Goroutines:   runtime.NumGoroutine(),
		GcCount:      uint32(gc.NumGC),
		GcPauseMs:    round1(gc.PauseTotal.Seconds() * 1000),
		PollInflight: int(c.pollIn.Load()),
		ReqInflight:  int(c.reqIn.Load()),
	}
	for _, active := range c.states {
		if active {
			s.ConnsActive++
		} else {
			s.ConnsIdle++
		}
	}
	// CPU% 与 QPS 都是"窗口内差值"：runtime 只给累计量，速率得自己存基线。
	if cpuOK {
		if dt := wall.Sub(c.prevWall); dt > 0 {
			s.CPUPercent = round1(float64(cpuNow-c.prevCPU) / float64(dt) * 100)
			s.ReqPerSec = round1(float64(c.reqTotal.Load()-c.prevReqs) / dt.Seconds())
		}
		c.prevCPU, c.prevWall, c.prevReqs = cpuNow, wall, c.reqTotal.Load()
	}

	if len(c.hist) < HistoryLen {
		c.hist = append(c.hist, s)
	} else {
		c.hist[c.hpos] = s
		c.hfull = true
	}
	c.hpos = (c.hpos + 1) % HistoryLen
	return s
}

// History 返回按时间升序的采样序列（前端画曲线用）。
func (c *Collector) History() []Sample {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hfull {
		out := make([]Sample, len(c.hist))
		copy(out, c.hist)
		return out
	}
	out := make([]Sample, 0, HistoryLen)
	out = append(out, c.hist[c.hpos:]...)
	out = append(out, c.hist[:c.hpos]...)
	return out
}

// Run 每 interval 采一次样并把快照交给 publish；ctx 取消即退出。
// 首次采样在启动后一个 interval 才发生，所以进程刚起来时曲线是空的，属正常。
func (c *Collector) Run(ctx context.Context, publish func(Sample)) {
	t := time.NewTicker(ReportInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s := c.Sample()
			if publish != nil {
				publish(s)
			}
		}
	}
}
