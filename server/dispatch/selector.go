// 分发策略（PowerJob TaskTrackerSelector 的 Go 移植）。
// 在任务入队时预选执行机器，PowerJob 在推送前做同样的选择，pull 模型只是把投递改为取件。
package dispatch

import (
	"log"
	"math/rand/v2"
	"sort"
	"strings"

	"gojob/internal/protocol"
	"gojob/server/store"
)

// 候选机器 = 在线节点 ∩ designated_workers（老式指定），再由策略从中选一台。
func (d *Dispatcher) candidates(job *store.JobInfo) []protocol.WorkerInfo {
	online := d.reg.Workers(job.AppId)
	want := strings.TrimSpace(job.DesignatedWorkers)
	if want == "" {
		return online
	}
	set := map[string]bool{}
	for _, a := range strings.Split(want, ",") {
		if a = strings.TrimSpace(a); a != "" {
			set[a] = true
		}
	}
	out := make([]protocol.WorkerInfo, 0, len(online))
	for _, w := range online {
		if set[w.Address] {
			out = append(out, w)
		}
	}
	return out
}

// PickTarget 按 job 的分发策略预选执行节点，返回 worker instance_id；
// 无可用节点或策略无法判定时返回 ""，表示退回"任意在线节点可认领"。
func (d *Dispatcher) PickTarget(job *store.JobInfo) string {
	ws := d.candidates(job)
	if len(ws) == 0 {
		return ""
	}
	if len(ws) == 1 {
		return ws[0].InstanceId
	}
	switch job.DispatchStrategy {
	case protocol.DispatchRandom:
		return ws[rand.IntN(len(ws))].InstanceId
	case protocol.DispatchSpecify:
		var matched []protocol.WorkerInfo
		for _, w := range ws {
			if matchSpecify(w, job.DispatchStrategyConfig) {
				matched = append(matched, w)
			}
		}
		if len(matched) == 0 {
			// 同 PowerJob SpecifyTaskTrackerSelector：条件落空降级随机并告警
			log.Printf("job %d: dispatch_strategy_config %q matched no worker, fallback to random", job.ID, job.DispatchStrategyConfig)
			return ws[rand.IntN(len(ws))].InstanceId
		}
		return matched[rand.IntN(len(matched))].InstanceId
	case protocol.DispatchRoundRobin:
		return ws[d.cursor(job.ID, uint64(len(ws)))].InstanceId
	default: // HEALTH_FIRST 及未识别值，与 PowerJob DispatchStrategy.of(null)=HEALTH_FIRST 一致
		sort.SliceStable(ws, func(i, j int) bool {
			si, sj := healthScore(ws[i]), healthScore(ws[j])
			if si != sj {
				return si > sj
			}
			return ws[i].Address < ws[j].Address
		})
		return ws[0].InstanceId
	}
}

// BroadcastTargets 广播落点：该项目下每个在线节点一份，返回 worker instance_id 列表。
//
// 不套分发策略——"每台一份"本身就是分发结果，再按健康分选一次只会漏掉节点。
// 只有 designated_workers 仍然生效（那是"这个任务只允许在这几台上跑"的硬约束，
// 与 PowerJob 的 designatedWorkers 同语义），地址按 instance_id 稳定排序，
// 让同一批在线节点在两个副本上算出的 task_key 序号一致，排查时不用猜。
func (d *Dispatcher) BroadcastTargets(job *store.JobInfo) []string {
	ws := d.candidates(job)
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.InstanceId)
	}
	sort.Strings(out)
	return out
}

// healthScore 移植 PowerJob SystemMetrics.calculateScore：内存权重 2，CPU 余量权重 1。
// worker 未上报内存上限时退化为"已用内存越少越优"，保持同向可排序。
func healthScore(w protocol.WorkerInfo) float64 {
	mem := w.MemoryMax - w.MemoryUsed
	if w.MemoryMax <= 0 {
		mem = -w.MemoryUsed
	}
	cpu := float64(w.CpuProcessors) - w.CpuLoad
	if cpu > float64(w.CpuProcessors) { // 取不到 CPU 负载（负值）时的兜底，同 PowerJob
		cpu = 1
	}
	return mem*2 + cpu
}

// matchSpecify 移植 PowerJob SpecifyUtils.match：tagEquals: 全等、tagIn: 子串、默认逗号分隔 IP·tag 全等。
func matchSpecify(w protocol.WorkerInfo, specifyInfo string) bool {
	specifyInfo = strings.TrimSpace(specifyInfo)
	if specifyInfo == "" {
		return false
	}
	if tag, ok := strings.CutPrefix(specifyInfo, "tagIn:"); ok {
		return strings.Contains(w.Tag, tag)
	}
	if tag, ok := strings.CutPrefix(specifyInfo, "tagEquals:"); ok {
		return w.Tag == tag
	}
	for _, s := range strings.Split(specifyInfo, ",") {
		s = strings.TrimSpace(s)
		if s != "" && (s == w.Tag || s == w.Address) {
			return true
		}
	}
	return false
}
