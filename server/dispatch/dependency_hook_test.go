package dispatch

import (
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
)

// addRunningTask 往既有实例上再挂一条 RUNNING 任务。
func addRunningTask(t *testing.T, st *store.Store, instId uint64) uint64 {
	t.Helper()
	now := store.Now()
	task := store.InstanceTask{JobId: 1, InstanceId: instId, AppId: "app1", WorkerId: "worker-A",
		Status: protocol.TaskRunning, Attempts: 1, MaxAttempts: 1,
		DispatchTime: now, CreateTime: now, ModifyTime: now}
	if err := st.DB.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return task.ID
}

func reportOK(t *testing.T, d *Dispatcher, taskID uint64, success bool) {
	t.Helper()
	status := protocol.TaskSucceed
	if !success {
		status = protocol.TaskFailed
	}
	if err := d.Report(t.Context(), &protocol.ReportTaskReq{
		InstanceId: "worker-A", AppId: "app1", TaskId: taskID, Success: success, Status: status,
	}); err != nil {
		t.Fatalf("report %d: %v", taskID, err)
	}
}

// 依赖触发只认"实例刚进终态"这一次回调：中间任务完成不能回调，重复回报也不能二次回调。
func TestInstanceFinishHookFiresOnceWithTerminalStatus(t *testing.T) {
	st := newStore(t)
	d := New(st, registry.New(st.DB, time.Minute), nil)
	var events []FinishEvent
	d.OnInstanceFinished(func(ev FinishEvent) { events = append(events, ev) })

	instId, first := newTask(t, st, protocol.TaskRunning, "worker-A")
	second := addRunningTask(t, st, instId)
	st.DB.Model(&store.JobInstance{}).Where("id = ?", instId).
		Update("running_times", 2)

	reportOK(t, d, first, true)
	if len(events) != 0 {
		t.Fatalf("events = %+v, want 空（还有任务未结束，实例未到终态）", events)
	}

	reportOK(t, d, second, true)
	if len(events) != 1 {
		t.Fatalf("events = %+v, want 1 条", events)
	}
	ev := events[0]
	if ev.Status != protocol.InstanceSucceed || ev.InstanceId != instId {
		t.Fatalf("event = %+v, want {InstanceId:%d Status:SUCCEED}", ev, instId)
	}

	// worker 结果回报有重试，重复回报不得再回调（否则下游会被重复触发）
	reportOK(t, d, second, true)
	if len(events) != 1 {
		t.Fatalf("events = %+v after duplicate report, want 仍为 1 条", events)
	}
}

func TestInstanceFinishHookStatusReflectsOutcome(t *testing.T) {
	cases := []struct {
		name   string
		report bool // 回报成功与否
		want   string
	}{
		{"failed", false, protocol.InstanceFailed},
		{"succeed", true, protocol.InstanceSucceed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t)
			d := New(st, registry.New(st.DB, time.Minute), nil)
			var got string
			d.OnInstanceFinished(func(ev FinishEvent) { got = ev.Status })

			instId, taskId := newTask(t, st, protocol.TaskRunning, "worker-A")
			if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskId).
				Update("max_attempts", 1).Error; err != nil { // 不再重试，本次失败即终态
				t.Fatal(err)
			}
			reportOK(t, d, taskId, tc.report)
			if got != tc.want {
				t.Fatalf("hook status = %q, want %q", got, tc.want)
			}
			if inst := getInst(t, st, instId); inst.Status != tc.want {
				t.Fatalf("instance status = %s, want %s", inst.Status, tc.want)
			}
		})
	}
}

// 被停止的实例回调 STOPPED，依赖触发据此不传导。
func TestInstanceFinishHookStoppedNotSucceed(t *testing.T) {
	st := newStore(t)
	d := New(st, registry.New(st.DB, time.Minute), nil)
	var got string
	d.OnInstanceFinished(func(ev FinishEvent) { got = ev.Status })

	instId, taskId := newTask(t, st, protocol.TaskRunning, "worker-A")
	mustStop(t, d, instId)
	reportOK(t, d, taskId, false) // 处理器被取消，SDK 按失败回报

	if got != protocol.InstanceStopped {
		t.Fatalf("hook status = %q, want %q", got, protocol.InstanceStopped)
	}
}
