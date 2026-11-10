package dispatch

import (
	"path/filepath"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
)

func newStore(t testing.TB) *store.Store {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 认领层按 job_infos.enable 硬过滤：默认造一个启用中的任务（ID=1），
	// 各用例的实例/任务都以 JobId=1 关联；验暂停语义的用例把它改成 0。
	job := store.JobInfo{ID: 1, AppId: "app1", JobName: "test-job", Enable: true,
		CreateTime: store.Now(), ModifyTime: store.Now()}
	if err := st.DB.Create(&job).Error; err != nil {
		t.Fatalf("seed job: %v", err)
	}
	return st
}

// newTask 造一个实例 + 单个任务（状态由调用方指定），返回实例 ID 与任务 ID。
func newTask(t *testing.T, st *store.Store, status string, workerId string) (uint64, uint64) {
	t.Helper()
	now := store.Now()
	inst := store.JobInstance{JobId: 1, AppId: "app1", Status: protocol.InstanceWaiting,
		RunningTimes: 1, TotalTaskNum: 1, CreateTime: now, ModifyTime: now}
	if err := st.DB.Create(&inst).Error; err != nil {
		t.Fatal(err)
	}
	task := store.InstanceTask{JobId: 1, InstanceId: inst.ID, AppId: "app1", WorkerId: workerId,
		Status: status, Attempts: 1, MaxAttempts: 3, DispatchTime: now, CreateTime: now, ModifyTime: now}
	if err := st.DB.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return inst.ID, task.ID
}

func mustStop(t *testing.T, d *Dispatcher, instID uint64) {
	t.Helper()
	if err := d.StopInstance(instID); err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
}

// getTask 必须每次用全新结构体读：gorm 扫描 NULL 列时不会覆盖复用结构体里的旧指针，
// 用同一个变量二次 First 会读到已清空字段的陈旧值。
func getTask(t *testing.T, st *store.Store, taskID uint64) store.InstanceTask {
	t.Helper()
	var task store.InstanceTask
	if err := st.DB.First(&task, taskID).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	return task
}

func getInst(t *testing.T, st *store.Store, instID uint64) store.JobInstance {
	t.Helper()
	var inst store.JobInstance
	if err := st.DB.First(&inst, instID).Error; err != nil {
		t.Fatalf("load instance: %v", err)
	}
	return inst
}

func TestStopWaitingTaskTerminatesImmediately(t *testing.T) {
	st := newStore(t)
	d := New(st, registry.New(st.DB, time.Minute), nil)
	instID, taskID := newTask(t, st, protocol.TaskWaiting, "")

	mustStop(t, d, instID)

	if task := getTask(t, st, taskID); task.Status != protocol.TaskStopped {
		t.Fatalf("task status = %s, want STOPPED", task.Status)
	}
	inst := getInst(t, st, instID)
	if inst.Status != protocol.InstanceStopped {
		t.Fatalf("instance status = %s, want STOPPED", inst.Status)
	}
	if inst.RunningTimes != 0 {
		t.Fatalf("running_times = %d, want 0（未派发任务被停也要扣计数，否则实例永不收敛）", inst.RunningTimes)
	}
}

func TestStopRunningTaskRecordsDurableIntent(t *testing.T) {
	st := newStore(t)
	d := New(st, registry.New(st.DB, time.Minute), nil)
	instID, taskID := newTask(t, st, protocol.TaskRunning, "worker-A")

	mustStop(t, d, instID)

	// 指令只落在行上、不带内存态：server 重启后仍能随长轮询/心跳下发
	if got := d.PendingCancels("worker-A"); len(got) != 1 || got[0] != taskID {
		t.Fatalf("PendingCancels = %v, want [%d]", got, taskID)
	}
	if task := getTask(t, st, taskID); task.StopRequestTime == nil {
		t.Fatal("RUNNING 任务应落 stop_request_time 等 worker 协作退出")
	}
	if inst := getInst(t, st, instID); inst.Status != protocol.InstanceStopping {
		t.Fatalf("instance status = %s, want STOPPING", inst.Status)
	}
	// 幂等：重复点停止不应报错，也不应多扣计数
	mustStop(t, d, instID)
	if inst := getInst(t, st, instID); inst.RunningTimes != 1 {
		t.Fatalf("running_times = %d, want 1", inst.RunningTimes)
	}
	if other := d.PendingCancels("worker-B"); len(other) != 0 {
		t.Fatalf("PendingCancels(worker-B) = %v, want 空（cancel 只发给持该任务的机器）", other)
	}
}

func TestReportAfterStopRequestIsNotFailure(t *testing.T) {
	st := newStore(t)
	d := New(st, registry.New(st.DB, time.Minute), nil)
	instID, taskID := newTask(t, st, protocol.TaskRunning, "worker-A")
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskID).
		Update("stop_request_time", store.Now()).Error; err != nil {
		t.Fatal(err)
	}

	// 处理器被取消后返回 context.Canceled，SDK 报 FAILED：不能计入失败、不能重排队
	err := d.Report(t.Context(), &protocol.ReportTaskReq{
		InstanceId: "worker-A", AppId: "app1", TaskId: taskID, Success: false, Result: "context canceled",
	})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	task := getTask(t, st, taskID)
	if task.Status != protocol.TaskStopped {
		t.Fatalf("task status = %s, want STOPPED", task.Status)
	}
	if task.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1（被停任务不重排队）", task.Attempts)
	}
	if task.StopRequestTime != nil {
		t.Fatalf("stop_request_time = %v, want NULL（ack 后清空）", *task.StopRequestTime)
	}
	if left := d.PendingCancels("worker-A"); len(left) != 0 {
		t.Fatalf("PendingCancels = %v, want 空（不得重复下发已确认的 cancel）", left)
	}
	inst := getInst(t, st, instID)
	if inst.Status != protocol.InstanceStopped {
		t.Fatalf("instance status = %s, want STOPPED", inst.Status)
	}
	if inst.FailedTaskNum != 0 {
		t.Fatalf("failed_task_num = %d, want 0（停止不算失败）", inst.FailedTaskNum)
	}
}

// 用户按下停止 → server 立刻宕机重启 → worker 也失联：
// 巡检必须把停止就地收尾成 STOPPED，而不是把任务重新排队再跑一遍。
func TestStopSurvivesServerRestartWithLostWorker(t *testing.T) {
	st := newStore(t)
	d := New(st, registry.New(st.DB, time.Minute), nil)
	instID, taskID := newTask(t, st, protocol.TaskRunning, "worker-A")
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskID).
		Update("dispatch_time", store.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	mustStop(t, d, instID)

	// 进程重启后 DB 里的意图仍在，失联巡检据此收尾
	if err := d.ReassignLostTasks(10 * time.Minute); err != nil {
		t.Fatalf("ReassignLostTasks: %v", err)
	}
	task := getTask(t, st, taskID)
	if task.Status != protocol.TaskStopped {
		t.Fatalf("task status = %s, want STOPPED（失联不得复活重跑）", task.Status)
	}
	if inst := getInst(t, st, instID); inst.Status != protocol.InstanceStopped {
		t.Fatalf("instance status = %s, want STOPPED", inst.Status)
	}

	// 二次巡检不得重复扣计数、不得把实例改判成 SUCCEED
	if err := d.ReassignLostTasks(10 * time.Minute); err != nil {
		t.Fatalf("ReassignLostTasks again: %v", err)
	}
	inst := getInst(t, st, instID)
	if inst.RunningTimes != 0 {
		t.Fatalf("running_times = %d, want 0", inst.RunningTimes)
	}
	if inst.Status != protocol.InstanceStopped {
		t.Fatalf("instance status = %s, want STOPPED（终态不可被改写）", inst.Status)
	}
}

func TestStopFinishedInstanceRejected(t *testing.T) {
	st := newStore(t)
	d := New(st, registry.New(st.DB, time.Minute), nil)
	instID, taskID := newTask(t, st, protocol.TaskSucceed, "worker-A")
	if err := st.DB.Model(&store.JobInstance{}).Where("id = ?", instID).
		Updates(map[string]any{"status": protocol.InstanceSucceed, "running_times": 0}).Error; err != nil {
		t.Fatal(err)
	}

	if err := d.StopInstance(instID); err == nil {
		t.Fatal("已结束实例应拒绝停止")
	}
	if task := getTask(t, st, taskID); task.Status != protocol.TaskSucceed {
		t.Fatalf("task status = %s，want SUCCEED（拒绝停止不得改写终态）", task.Status)
	}
}
