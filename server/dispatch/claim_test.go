package dispatch

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
	"gorm.io/gorm"
)

func seedTasks(t testing.TB, st *store.Store, n int) []uint64 {
	t.Helper()
	now := store.Now()
	ids := make([]uint64, 0, n)
	for i := 0; i < n; i++ {
		task := store.InstanceTask{JobId: 1, InstanceId: 1, AppId: "app1",
			Processor: "p.hello", Status: protocol.TaskWaiting, Attempts: 1, MaxAttempts: 1,
			DispatchTime: now, CreateTime: now, ModifyTime: now}
		if err := st.DB.Create(&task).Error; err != nil {
			t.Fatalf("seed task: %v", err)
		}
		ids = append(ids, task.ID)
	}
	return ids
}

// 两个 worker 的候选 SELECT 可能读到同一批 WAITING 行（读已提交下没有行锁）。
// 这里直接把"其中一行已被别人抢走"的状态造出来，验证只有真正改到行的那方拿到任务。
func TestTakeRowsSkipsRowsClaimedByOthers(t *testing.T) {
	st := newStore(t)
	ids := seedTasks(t, st, 3)

	// worker-A 先抢走中间那行
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", ids[1]).
		Updates(map[string]any{"status": protocol.TaskRunning, "worker_id": "worker-A"}).Error; err != nil {
		t.Fatal(err)
	}

	var won []uint64
	_, err := st.Tx(func(tx *gorm.DB) (struct{}, error) {
		var e error
		won, e = takeRows(tx, ids, "worker-B", store.Now())
		return struct{}{}, e
	})
	if err != nil {
		t.Fatalf("takeRows: %v", err)
	}
	if len(won) != 2 || won[0] != ids[0] || won[1] != ids[2] {
		t.Fatalf("won = %v, want [%d %d]（已被抢走的行不能再次下发）", won, ids[0], ids[2])
	}

	var stolen store.InstanceTask
	st.DB.First(&stolen, ids[1])
	if stolen.WorkerId != "worker-A" {
		t.Fatalf("worker_id = %s, want worker-A（抢输的一方不得改写归属）", stolen.WorkerId)
	}
}

// 并发认领不得出现重复下发：每个任务最终只交给一个 worker。
func TestConcurrentClaimsNeverDeliverTwice(t *testing.T) {
	st := newStore(t)
	reg := registry.New(st.DB, time.Minute)
	d := New(st, reg, nil)
	const total = 240
	ids := seedTasks(t, st, total)
	_ = ids

	var workers []string
	for i := 0; i < 8; i++ {
		id := "worker-" + string(rune('A'+i))
		workers = append(workers, id)
		_ = reg.Alive(protocol.WorkerInfo{InstanceId: id, AppId: "app1", Address: "127.0.0.1:7799"})
	}

	var mu sync.Mutex
	delivered := map[uint64]int{}
	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			for {
				req := &protocol.FetchTaskReq{InstanceId: worker, AppId: "app1", MaxTaskNum: 16}
				tasks, err := d.claim(req, req.MaxTaskNum)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if len(tasks) == 0 {
					return
				}
				mu.Lock()
				for _, task := range tasks {
					delivered[task.TaskId]++
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(delivered) != total {
		t.Fatalf("delivered %d 个不同任务, want %d", len(delivered), total)
	}
	for id, n := range delivered {
		if n != 1 {
			t.Fatalf("task %d 被下发 %d 次, want 1（同一任务不得双跑）", id, n)
		}
	}
	var running int64
	st.DB.Model(&store.InstanceTask{}).Where("status = ?", protocol.TaskRunning).Count(&running)
	if running != total {
		t.Fatalf("RUNNING 行数 = %d, want %d", running, total)
	}
}

// 拆掉 target_worker_id 的 OR 之后语义必须不变：指名给某机的任务优先由该机取走，
// 别的机器只能拿自由任务；剩余名额由自由任务补齐。
func TestClaimPrefersTasksTargetedAtThisWorker(t *testing.T) {
	st := newStore(t)
	reg := registry.New(st.DB, time.Minute)
	d := New(st, reg, nil)
	for _, w := range []string{"worker-A", "worker-B"} {
		_ = reg.Alive(protocol.WorkerInfo{InstanceId: w, AppId: "app1", Address: "127.0.0.1:7799"})
	}
	now := store.Now()
	add := func(target string, offset time.Duration) uint64 {
		task := store.InstanceTask{JobId: 1, InstanceId: 1, AppId: "app1", Processor: "p.hello",
			Status: protocol.TaskWaiting, Attempts: 1, MaxAttempts: 1, TargetWorkerId: target,
			DispatchTime: now.Add(offset), CreateTime: now, ModifyTime: now}
		if err := st.DB.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	targeted := add("worker-A", -time.Second) // 指名给 A
	free := add("", -time.Second)             // 任意机器可领

	req := func(w string, n int) *protocol.FetchTaskReq {
		return &protocol.FetchTaskReq{InstanceId: w, AppId: "app1", MaxTaskNum: n}
	}
	got, err := d.claim(req("worker-B", 1), 1)
	if err != nil || len(got) != 1 || got[0].TaskId != free {
		t.Fatalf("worker-B 应只拿到自由任务 %d，实得 %+v err=%v", free, ids(got), err)
	}
	got, err = d.claim(req("worker-A", 2), 2)
	if err != nil || len(got) != 1 || got[0].TaskId != targeted {
		t.Fatalf("worker-A 应拿到指名任务 %d（自由任务已被 B 领走），实得 %+v err=%v", targeted, ids(got), err)
	}
}

func ids(tasks []protocol.TaskDTO) []uint64 {
	out := make([]uint64, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.TaskId)
	}
	return out
}

// BenchmarkClaim 量服务端认领吞吐：w 个 worker 并发各拉 16 条，直到队列清空。
// go test ./server/dispatch -run x -bench BenchmarkClaim -benchtime 3x
func BenchmarkClaim(b *testing.B) {
	const perFetch = 16
	for _, w := range []int{4, 16} {
		b.Run(fmt.Sprintf("workers=%d", w), func(b *testing.B) {
			for n := 0; n < b.N; n++ {
				st := newStore(b)
				d := New(st, registry.New(st.DB, time.Minute), nil)
				total := w * perFetch * 20 // 每轮每 worker 约 20 次拉取
				seedTasks(b, st, total)
				for i := 0; i < w; i++ {
					d.reg.Alive(protocol.WorkerInfo{
						InstanceId: fmt.Sprintf("worker-%d", i), AppId: "app1", Address: "127.0.0.1:7799"})
				}
				start := time.Now()
				var wg sync.WaitGroup
				for i := 0; i < w; i++ {
					wg.Add(1)
					go func(worker string) {
						defer wg.Done()
						for {
							tasks, err := d.claim(&protocol.FetchTaskReq{
								InstanceId: worker, AppId: "app1", MaxTaskNum: perFetch}, perFetch)
							if err != nil || len(tasks) == 0 {
								return
							}
						}
					}(fmt.Sprintf("worker-%d", i))
				}
				wg.Wait()
				el := time.Since(start)
				b.ReportMetric(float64(total)/el.Seconds(), "tasks/s")
			}
		})
	}
}
