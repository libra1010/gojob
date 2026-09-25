package dispatch

import (
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
)

func worker(appID, addr string, cores int, cpuLoad, memUsed, memMax float64, tag string) protocol.WorkerInfo {
	return protocol.WorkerInfo{
		InstanceId: addr, AppId: appID, Address: addr,
		CpuProcessors: cores, CpuLoad: cpuLoad,
		MemoryUsed: memUsed, MemoryMax: memMax, Tag: tag,
	}
}

// newTestDispatcher 造一个只装了在线节点的调度器：分发策略在入队时选机器，
// 而在线节点表现在是库里的表，所以测试也得有库。
func newTestDispatcher(t *testing.T, workers ...protocol.WorkerInfo) *Dispatcher {
	t.Helper()
	st := newStore(t)
	reg := registry.New(st.DB, time.Second)
	for _, w := range workers {
		if err := reg.Alive(w); err != nil {
			t.Fatal(err)
		}
	}
	return New(st, reg, nil)
}

func job(strategy int, cfg string) *store.JobInfo {
	return &store.JobInfo{ID: 1, AppId: "app1", DispatchStrategy: strategy, DispatchStrategyConfig: cfg}
}

func TestHealthFirstPicksFreshestNode(t *testing.T) {
	// 同机器规格下，已用内存少 + CPU 负载低者优先（PowerJob: (max-used)*2 + (cores-load)）
	busy := worker("app1", "10.0.0.1:1", 8, 7.5, 7000, 8000, "")
	idle := worker("app1", "10.0.0.2:1", 8, 0.2, 1000, 8000, "")
	d := newTestDispatcher(t, busy, idle)
	if got := d.PickTarget(job(protocol.DispatchHealthFirst, "")); got != idle.InstanceId {
		t.Fatalf("health_first picked %q, want %q", got, idle.InstanceId)
	}
	// 内存项权重×2 会盖过 CPU 项，因此单独构造内存画像相同的两节点来验证 CPU 方向
	cpuBusy := worker("app1", "10.0.0.3:1", 8, 7.9, 2000, 8000, "")
	cpuFree := worker("app1", "10.0.0.4:1", 8, 0.3, 2000, 8000, "")
	d2 := newTestDispatcher(t, cpuBusy, cpuFree)
	if got := d2.PickTarget(job(protocol.DispatchHealthFirst, "")); got != cpuFree.InstanceId {
		t.Fatalf("cpu term: picked %q, want %q", got, cpuFree.InstanceId)
	}
}

func TestRoundRobinAlternatesStrictly(t *testing.T) {
	a := worker("app1", "10.0.0.1:1", 8, 1, 1000, 8000, "")
	b := worker("app1", "10.0.0.2:1", 8, 1, 1000, 8000, "")
	d := newTestDispatcher(t, a, b)
	j := job(protocol.DispatchRoundRobin, "")
	// 候选按 address 排序，游标依次推进：A B A B
	want := []string{a.InstanceId, b.InstanceId, a.InstanceId, b.InstanceId}
	for i, w := range want {
		if got := d.PickTarget(j); got != w {
			t.Fatalf("round %d: got %q want %q", i, got, w)
		}
	}
	// 另一 job 有独立游标，不受上面 4 次推进影响
	if got := d.PickTarget(job(protocol.DispatchRoundRobin, "")); got != a.InstanceId {
		t.Fatalf("cursor leaked across jobs: got %q", got)
	}
}

func TestSpecifySyntaxAndFallback(t *testing.T) {
	gray := worker("app1", "10.0.0.1:1", 8, 1, 100, 8000, "gray")
	prod := worker("app1", "10.0.0.2:1", 8, 1, 100, 8000, "prod")
	d := newTestDispatcher(t, gray, prod)

	if got := d.PickTarget(job(protocol.DispatchSpecify, "tagEquals:gray")); got != gray.InstanceId {
		t.Fatalf("tagEquals: got %q want %q", got, gray.InstanceId)
	}
	if got := d.PickTarget(job(protocol.DispatchSpecify, "tagIn:ray")); got != gray.InstanceId {
		t.Fatalf("tagIn: got %q want %q", got, gray.InstanceId)
	}
	if got := d.PickTarget(job(protocol.DispatchSpecify, "10.0.0.2:1")); got != prod.InstanceId {
		t.Fatalf("address match: got %q want %q", got, prod.InstanceId)
	}
	// 条件落空降级随机：结果必须仍是候选之一，而不是空串
	got := d.PickTarget(job(protocol.DispatchSpecify, "tagEquals:nonexistent"))
	if got != gray.InstanceId && got != prod.InstanceId {
		t.Fatalf("specify fallback returned %q", got)
	}
}

func TestDesignatedWorkersNarrowCandidates(t *testing.T) {
	a := worker("app1", "10.0.0.1:1", 8, 1, 100, 8000, "")
	b := worker("app1", "10.0.0.2:1", 8, 1, 100, 8000, "")
	c := worker("app1", "10.0.0.3:1", 8, 1, 100, 8000, "")
	d := newTestDispatcher(t, a, b, c)

	j := job(protocol.DispatchRoundRobin, "")
	j.DesignatedWorkers = "10.0.0.1:1, 10.0.0.3:1" // 含空格，应被 trim
	seen := map[string]int{}
	for range 6 {
		seen[d.PickTarget(j)]++
	}
	if seen[b.InstanceId] > 0 {
		t.Fatalf("non-designated worker %q received tasks", b.InstanceId)
	}
	if seen[a.InstanceId] != 3 || seen[c.InstanceId] != 3 {
		t.Fatalf("uneven round robin over designated set: %+v", seen)
	}
}

func TestPickTargetWithoutOnlineWorkerReturnsEmpty(t *testing.T) {
	d := newTestDispatcher(t)
	if got := d.PickTarget(job(protocol.DispatchHealthFirst, "")); got != "" {
		t.Fatalf("expected empty target with no alive worker, got %q", got)
	}
}

func TestHealthScoreMatchesPowerJobFormula(t *testing.T) {
	// (16000-2000)*2 + (16-4) = 28012
	w := worker("app1", "10.0.0.1:1", 16, 4, 2000, 16000, "")
	if got := healthScore(w); got != 28012 {
		t.Fatalf("healthScore = %v, want 28012", got)
	}
	// 上限未知（0）时内存项退化为 -已用×2，仍可排序
	unknown := worker("app1", "10.0.0.2:1", 8, 0, 3000, 0, "")
	if got := healthScore(unknown); got != -5992 { // -3000*2 + (8-0)
		t.Fatalf("healthScore with unknown max = %v, want -5992", got)
	}
}
