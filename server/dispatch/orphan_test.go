package dispatch

import (
	"strings"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
)

// agedWaitingTask 造一个"实例 WAITING + 单任务 WAITING"的行，并把 dispatch_time 推到过去。
func agedWaitingTask(t *testing.T, st *store.Store, age time.Duration) (uint64, uint64) {
	t.Helper()
	instID, taskID := newTask(t, st, protocol.TaskWaiting, "")
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskID).
		Update("dispatch_time", store.Now().Add(-age)).Error; err != nil {
		t.Fatal(err)
	}
	return instID, taskID
}

func newOrphanDispatcher(t *testing.T, st *store.Store, workers ...string) *Dispatcher {
	t.Helper()
	reg := registry.New(st.DB, time.Minute)
	for _, w := range workers {
		if err := reg.Alive(protocol.WorkerInfo{InstanceId: w, AppId: "app1", Address: w + ":7799"}); err != nil {
			t.Fatalf("register %s: %v", w, err)
		}
	}
	return New(st, reg, nil)
}

// 排队超过宽限且项目无在线节点：任务与实例一起收成 FAILED。
func TestFailOrphanedWaitingFailsWhenNoWorker(t *testing.T) {
	st := newStore(t)
	d := newOrphanDispatcher(t, st)
	instID, taskID := agedWaitingTask(t, st, 2*time.Minute)

	if err := d.FailOrphanedWaiting(time.Minute); err != nil {
		t.Fatal(err)
	}
	task := getTask(t, st, taskID)
	if task.Status != protocol.TaskFailed || !strings.Contains(task.Result, "无可用 worker") {
		t.Fatalf("任务 = %s / %q, want FAILED + 无可用 worker", task.Status, task.Result)
	}
	inst := getInst(t, st, instID)
	if inst.Status != protocol.InstanceFailed || inst.RunningTimes != 0 {
		t.Fatalf("实例 = %s running=%d, want FAILED/0", inst.Status, inst.RunningTimes)
	}
}

// 项目还有在线节点：老任务保持排队，交给 worker 按空闲额度认领。
func TestFailOrphanedWaitingLeavesQueueWithWorker(t *testing.T) {
	st := newStore(t)
	d := newOrphanDispatcher(t, st, "w1")
	instID, taskID := agedWaitingTask(t, st, 2*time.Minute)

	if err := d.FailOrphanedWaiting(time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := getTask(t, st, taskID); got.Status != protocol.TaskWaiting {
		t.Fatalf("任务 = %s, want WAITING（有节点时不能判死）", got.Status)
	}
	if got := getInst(t, st, instID); got.Status != protocol.InstanceWaiting {
		t.Fatalf("实例 = %s, want WAITING", got.Status)
	}
}

// 未到宽限的新任务不能动：它可能只是在正常排队等 worker 取件。
func TestFailOrphanedWaitingLeavesFreshTask(t *testing.T) {
	st := newStore(t)
	d := newOrphanDispatcher(t, st)
	_, taskID := agedWaitingTask(t, st, 0)

	if err := d.FailOrphanedWaiting(time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := getTask(t, st, taskID); got.Status != protocol.TaskWaiting {
		t.Fatalf("任务 = %s, want WAITING（未到宽限）", got.Status)
	}
}

// 停止意图已经落在排队任务上的（停止与失联回收的竞态遗留）：哪怕有节点也不再派发。
func TestFailOrphanedWaitingStopsRequestedTaskEvenWithWorker(t *testing.T) {
	st := newStore(t)
	d := newOrphanDispatcher(t, st, "w1")
	instID, taskID := agedWaitingTask(t, st, 2*time.Minute)
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskID).
		Update("stop_request_time", store.Now()).Error; err != nil {
		t.Fatal(err)
	}

	if err := d.FailOrphanedWaiting(time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := getTask(t, st, taskID); got.Status != protocol.TaskStopped {
		t.Fatalf("任务 = %s, want STOPPED", got.Status)
	}
	if got := getInst(t, st, instID); got.Status != protocol.InstanceStopped {
		t.Fatalf("实例 = %s, want STOPPED", got.Status)
	}
}

// 拉取守卫：已停止实例下的任务、以及已带停止意图的排队任务都不下发。
func TestClaimSkipsStoppedInstanceAndStopRequestedTasks(t *testing.T) {
	st := newStore(t)
	d := newOrphanDispatcher(t, st, "w1")
	now := store.Now()

	deadInst := seedWaitingInstance(t, st, "app1")
	if err := st.DB.Model(&store.JobInstance{}).Where("id = ?", deadInst.ID).
		Update("status", protocol.InstanceStopped).Error; err != nil {
		t.Fatal(err)
	}
	newTaskRow := func(instID uint64, stop *time.Time) uint64 {
		t.Helper()
		task := store.InstanceTask{JobId: 1, InstanceId: instID, AppId: "app1",
			Processor: "p.hello", Status: protocol.TaskWaiting, StopRequestTime: stop,
			Attempts: 1, MaxAttempts: 1, DispatchTime: now, CreateTime: now, ModifyTime: now}
		if err := st.DB.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
		return task.ID
	}
	newTaskRow(deadInst.ID, nil)

	liveInst := seedWaitingInstance(t, st, "app1")
	newTaskRow(liveInst.ID, &now)
	okTask := newTaskRow(liveInst.ID, nil)

	tasks, err := d.claim(&protocol.FetchTaskReq{InstanceId: "w1", AppId: "app1", MaxTaskNum: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].TaskId != okTask {
		t.Fatalf("认领 = %v, want 只下发正常任务 %d", ids(tasks), okTask)
	}
}

// 停止中的实例不再接收 map 派生：否则新任务会绕过 StopInstance 当时那次批量收尾。
func TestMapRejectsStoppingInstance(t *testing.T) {
	st := newStore(t)
	d := newTreeDispatcher(t, st, "worker-A")
	instID, rootID := newTree(t, st, protocol.ExecuteMap)
	if err := st.DB.Model(&store.JobInstance{}).Where("id = ?", instID).
		Update("status", protocol.InstanceStopping).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := mapFrom(t, d, "worker-A", rootID, 1, "SPLIT", `"a"`); err == nil {
		t.Fatal("停止中的实例不该再接收 map 子任务")
	}
	var n int64
	if err := st.DB.Model(&store.InstanceTask{}).Where("parent_task_id = ?", rootID).
		Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("派生了 %d 个子任务，want 0", n)
	}
}
