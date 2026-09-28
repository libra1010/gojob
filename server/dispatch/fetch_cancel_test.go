package dispatch

import (
	"context"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
)

// TestFetchWakesOnlyForFreshCancels 守住"cancel 未 ack 期间长轮询退化成忙等"这个回归。
//
// 停止指令要等任务回报才从 DB 清掉，所以"有没有 pending cancel"在指令下达后的整段执行期
// 里恒为真；把它当早退条件，worker 就会以"一次 HTTP 往返 + 两条查询"的频率空转（实机测过：
// 一个 30 秒任务被重复下发 53 次停止指令），挤住 sqlite 单写者还会把别的写入拖慢。
// 修法是只为"刚下达"的指令提前结束长轮询，但清单照旧随响应带回——指令不会因为不早退而丢。
func TestFetchWakesOnlyForFreshCancels(t *testing.T) {
	st := newStore(t)
	reg := registry.New(st.DB, time.Minute)
	if err := reg.Alive(protocol.WorkerInfo{AppId: "app1", InstanceId: "w1", Address: "127.0.0.1:7788"}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	d := New(st, reg, nil)
	instID, taskID := newTask(t, st, protocol.TaskRunning, "w1")
	mustStop(t, d, instID) // RUNNING 任务：只落 stop_request_time，等 worker 协作退出

	fetch := func(wait time.Duration) (time.Duration, []uint64) {
		t.Helper()
		req := &protocol.FetchTaskReq{
			AppId: "app1", InstanceId: "w1", Address: "127.0.0.1:7788",
			MaxTaskNum: 1, WaitSeconds: int(wait.Seconds()),
		}
		start := time.Now()
		tasks, cancels, err := d.Fetch(context.Background(), req)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if len(tasks) > 0 {
			t.Fatalf("不该有任务可领，实得 %d 条", len(tasks))
		}
		return time.Since(start), cancels
	}
	wantCancel := func(cancels []uint64) {
		t.Helper()
		if len(cancels) != 1 || cancels[0] != taskID {
			t.Fatalf("cancel 清单 = %v, want [%d]", cancels, taskID)
		}
	}

	// 刚下达：立刻醒，不必等到超时。
	el, cancels := fetch(5 * time.Second)
	if el > 2*time.Second {
		t.Errorf("新到的停止指令没能唤醒长轮询：耗时 %v", el)
	}
	wantCancel(cancels)

	// 把时间戳"变旧"，模拟指令已通知过、任务还在跑（未 ack）的整段时间。
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskID).
		Update("stop_request_time", store.Now().Add(-10*time.Second)).Error; err != nil {
		t.Fatalf("age the request: %v", err)
	}
	el, cancels = fetch(2 * time.Second)
	wantCancel(cancels) // 不提前返回 ≠ 不下发
	if el < 1500*time.Millisecond {
		t.Errorf("陈旧指令仍在让长轮询早退：耗时 %v，会退化成忙等", el)
	}
}

// TestFetchWithNoCancelsBlocksToTimeout 一条停止指令都没有时，行为与修复前完全一致：
// 安静地阻塞到超时，别因为新加的判定改动空转路径。
func TestFetchWithNoCancelsBlocksToTimeout(t *testing.T) {
	st := newStore(t)
	reg := registry.New(st.DB, time.Minute)
	if err := reg.Alive(protocol.WorkerInfo{AppId: "app1", InstanceId: "w1", Address: "127.0.0.1:7788"}); err != nil {
		t.Fatalf("register worker: %v", err)
	}
	d := New(st, reg, nil)
	newTask(t, st, protocol.TaskRunning, "w1") // 在跑，但没人请求停止

	var n int64
	if err := st.DB.Model(&store.InstanceTask{}).Where("stop_request_time IS NOT NULL").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("前置条件：不该有停止指令，实有 %d 条", n)
	}
	req := &protocol.FetchTaskReq{AppId: "app1", InstanceId: "w1", MaxTaskNum: 1, WaitSeconds: 1}
	start := time.Now()
	_, cancels, err := d.Fetch(context.Background(), req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(cancels) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("无指令时应安静地阻塞到超时，耗时 %v，cancels=%v", time.Since(start), cancels)
	}
}
