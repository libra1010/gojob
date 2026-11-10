package dispatch

import (
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/registry"
	"gojob/server/store"
)

// 回归：dispatch_time 是入队时间，不是"失联时长"。节点每 5s 还在心跳、只是任务跑得久，
// 巡检不能把它重新入队——否则在跑的长任务（ERP 批处理常见）每个维护周期（30s）都会被
// 再拉起一份并发执行，原那份的回报还会因为归属已变被静默丢弃。
func TestReassignKeepsTaskOfLiveWorker(t *testing.T) {
	st := newStore(t)
	reg := registry.New(st.DB, time.Minute)
	if err := reg.Alive(protocol.WorkerInfo{InstanceId: "worker-A", AppId: "app1", Address: "127.0.0.1:7799"}); err != nil {
		t.Fatal(err)
	}
	d := New(st, reg, nil)
	_, taskID := newTask(t, st, protocol.TaskRunning, "worker-A")
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskID).
		Update("dispatch_time", store.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	if err := d.ReassignLostTasks(5 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if task := getTask(t, st, taskID); task.Status != protocol.TaskRunning || task.WorkerId != "worker-A" {
		t.Fatalf("在跑任务被回收：status=%s worker=%q（节点还活着，重派就是并发双跑）", task.Status, task.WorkerId)
	}
}

// 对照：心跳确实停了才允许回收。
func TestReassignRecoversTaskOfDeadWorker(t *testing.T) {
	st := newStore(t)
	reg := registry.New(st.DB, time.Minute)
	if err := reg.Alive(protocol.WorkerInfo{InstanceId: "worker-A", AppId: "app1", Address: "127.0.0.1:7799"}); err != nil {
		t.Fatal(err)
	}
	d := New(st, reg, nil)
	_, taskID := newTask(t, st, protocol.TaskRunning, "worker-A")
	if err := st.DB.Model(&store.InstanceTask{}).Where("id = ?", taskID).
		Update("dispatch_time", store.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB.Model(&store.WorkerNode{}).
		Where("app_id = ? AND instance_id = ?", "app1", "worker-A").
		Update("last_heartbeat", store.Now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}

	if err := d.ReassignLostTasks(5 * time.Minute); err != nil {
		t.Fatal(err)
	}
	if task := getTask(t, st, taskID); task.Status != protocol.TaskWaiting || task.WorkerId != "" {
		t.Fatalf("失联节点的任务未被回收：status=%s worker=%q", task.Status, task.WorkerId)
	}
}
