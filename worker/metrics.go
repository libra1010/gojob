package worker

import (
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"gojob/internal/proc"
	"gojob/internal/protocol"
)

// fillRuntime 把本进程运行时指标填进心跳包，供控制台的节点监控页展示。
// 堆内存走已有的 heapInuseMB/memory_max 字段，这里只补它们表达不了的项；
// 取不到的项（macOS 上没有 /proc）保持 0，页面显示"—"，绝不猜数。
func (w *Worker) fillRuntime(info *protocol.WorkerInfo) {
	var gc debug.GCStats
	debug.ReadGCStats(&gc)

	if now, ok := proc.CPUTime(); ok {
		if !w.cpuAt.IsZero() {
			if dt := time.Since(w.cpuAt); dt > 0 {
				info.CpuPercent = round1(float64(now-w.cpuBase) / float64(dt) * 100)
			}
		}
		w.cpuBase, w.cpuAt = now, time.Now()
	}
	info.GcCount = gc.NumGC
	info.GcPauseMs = round1(gc.PauseTotal.Seconds() * 1000)
	info.RssMb = proc.RSSMb()
	info.Threads = runtime.NumGoroutine()
	info.OpenFDs = proc.OpenFDs()
	info.RunningTasks = len(w.sem) // 信号量已占用的额度 = 正在执行的任务数
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// memoryTotalMB 本进程可用内存上限（MB），0=未知。
// 优先读容器限额，与 PowerJob 用 JVM 上限打分的口径对齐：容器里跑的 worker 应以 cgroup 限额为准。
func memoryTotalMB() float64 {
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil { // cgroup v2
		if s := strings.TrimSpace(string(b)); s != "" && s != "max" {
			if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
				return v / 1024 / 1024
			}
		}
	} else if b, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil { // cgroup v1
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil && v > 0 && v < 1<<62 {
			return v / 1024 / 1024
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil { // 物理机/VM
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[0] == "MemTotal:" {
				if kb, err := strconv.ParseFloat(f[1], 64); err == nil {
					return kb / 1024 // kB → MB
				}
			}
		}
	}
	return 0 // darwin/windows 本地开发取不到：健康分退化为按已用内存排序
}

// loadAverage1m 1 分钟平均负载，单位=繁忙核数，与 PowerJob SystemMetrics.cpuLoad 同量纲
// （其打分公式为 cpuProcessors - cpuLoad）。取不到返回 0（视为空闲）。
func loadAverage1m() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return v
}

// usableCores 可用核数。Go 1.25 起 GOMAXPROCS 会按 cgroup CPU 限额收敛，
// 因此用它比 NumCPU 更贴近该进程真实可用算力。
func usableCores() int {
	if n := runtime.GOMAXPROCS(0); n > 0 {
		return n
	}
	return runtime.NumCPU()
}
