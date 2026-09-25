package dispatch

import (
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
)

func reportFrom(t *testing.T, d *Dispatcher, worker string, taskID uint64, success bool) {
	t.Helper()
	status := protocol.TaskSucceed
	if !success {
		status = protocol.TaskFailed
	}
	if err := d.Report(t.Context(), &protocol.ReportTaskReq{
		InstanceId: worker, AppId: "app1", TaskId: taskID, Success: success, Status: status,
	}); err != nil {
		t.Fatalf("report: %v", err)
	}
}

// 同项目内别的 worker 不得越权改判别人正在跑的任务（它们持有同一份 app 密码）。
func TestReportFromNonOwnerIsIgnored(t *testing.T) {
	st := newStore(t)
	d := New(st, registry.New(st.DB, time.Minute), nil)
	var events []FinishEvent
	d.OnInstanceFinished(func(ev FinishEvent) { events = append(events, ev) })

	instID, taskID := newTask(t, st, protocol.TaskRunning, "worker-A")

	reportFrom(t, d, "worker-B", taskID, true)

	task := getTask(t, st, taskID)
	if task.Status != protocol.TaskRunning {
		t.Fatalf("task status = %s, want 仍 RUNNING（非归属 worker 的回报不得生效）", task.Status)
	}
	if task.WorkerId != "worker-A" {
		t.Fatalf("worker_id = %s, want worker-A", task.WorkerId)
	}
	inst := getInst(t, st, instID)
	if inst.RunningTimes != 1 || inst.Status != protocol.InstanceWaiting {
		t.Fatalf("实例被越权回报改判：running_times=%d status=%s", inst.RunningTimes, inst.Status)
	}
	if len(events) != 0 {
		t.Fatalf("不应有终态回调：%+v", events)
	}

	// 归属方自己回报仍然正常生效
	reportFrom(t, d, "worker-A", taskID, true)
	if task := getTask(t, st, taskID); task.Status != protocol.TaskSucceed {
		t.Fatalf("owner report 后 task status = %s, want SUCCEED", task.Status)
	}
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1 条", events)
	}
}

// 失联回收把任务重派给别的机器后，原 worker 复活补报不得污染新执行。
func TestLateReportAfterReassignIsIgnored(t *testing.T) {
	st := newStore(t)
	reg := registry.New(st.DB, time.Minute)
	for _, w := range []string{"worker-A", "worker-B"} {
		_ = reg.Alive(protocol.WorkerInfo{InstanceId: w, AppId: "app1", Address: "127.0.0.1:7799"})
	}
	d := New(st, reg, nil)
	instID, taskID := newTask(t, st, protocol.TaskRunning, "worker-A")
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskID).
		Update("dispatch_time", store.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	if err := d.ReassignLostTasks(10 * time.Minute); err != nil {
		t.Fatalf("reassign: %v", err)
	}
	tasks, err := d.claim(&protocol.FetchTaskReq{InstanceId: "worker-B", AppId: "app1", MaxTaskNum: 5}, 5)
	if err != nil || len(tasks) != 1 || tasks[0].TaskId != taskID {
		t.Fatalf("worker-B 应重新领到任务 %d，实得 %+v err=%v", taskID, tasks, err)
	}

	reportFrom(t, d, "worker-A", taskID, false) // 老 worker 复活后补报失败
	if task := getTask(t, st, taskID); task.Status != protocol.TaskRunning {
		t.Fatalf("task status = %s, want RUNNING（B 正在跑，不能被 A 的迟到回报判死）", task.Status)
	}
	if inst := getInst(t, st, instID); inst.FailedTaskNum != 0 {
		t.Fatalf("failed_task_num = %d, want 0", inst.FailedTaskNum)
	}

	reportFrom(t, d, "worker-B", taskID, true)
	if task := getTask(t, st, taskID); task.Status != protocol.TaskSucceed {
		t.Fatalf("task status = %s, want SUCCEED", task.Status)
	}
	if inst := getInst(t, st, instID); inst.Status != protocol.InstanceSucceed {
		t.Fatalf("instance status = %s, want SUCCEED", inst.Status)
	}
}
