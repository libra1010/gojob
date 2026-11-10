package scheduler

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/store"
)

func newProbeFixture(t *testing.T, probe func(string) (bool, error)) (*store.Store, *Scheduler, *int) {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "probe.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	notified := 0
	return st, New(st, nil, func() { notified++ }, nil, nil, probe), &notified
}

func seedProbeJob(t *testing.T, st *store.Store, executeType string) *store.JobInfo {
	t.Helper()
	now := store.Now()
	job := store.JobInfo{AppId: "app1", JobName: "minute", Processor: "p.minute",
		TimeExpressionType: "CRON", TimeExpression: "0 0 * * *", ExecuteType: executeType,
		Enable: true, MaxRetryTimes: 0, InstanceNumLimit: 0, LogLevel: "INFO",
		NextTriggerTime: now, CreateTime: now, ModifyTime: now}
	if err := st.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return &job
}

// 没有在线节点：实例与根任务当场 FAILED，计划时间照常推进，且不唤醒长轮询。
func TestFireFailsFastWhenNoWorkerOnline(t *testing.T) {
	st, s, notified := newProbeFixture(t, func(string) (bool, error) { return false, nil })
	job := seedProbeJob(t, st, "")
	next := job.NextTriggerTime.Add(time.Minute)

	skipped, instID, err := s.fire(job, next, "CRON", "", 0)
	if err != nil || skipped || instID == 0 {
		t.Fatalf("fire = (%v, %d, %v), want 一条失败实例", skipped, instID, err)
	}
	var inst store.JobInstance
	if err := st.DB.First(&inst, instID).Error; err != nil {
		t.Fatal(err)
	}
	if inst.Status != protocol.InstanceFailed || !strings.Contains(inst.Result, "无可用 worker") {
		t.Fatalf("实例 = %s / %q, want FAILED + 无可用 worker", inst.Status, inst.Result)
	}
	if inst.RunningTimes != 0 || inst.TotalTaskNum != 1 {
		t.Fatalf("running/total = %d/%d, want 0/1", inst.RunningTimes, inst.TotalTaskNum)
	}
	var task store.InstanceTask
	if err := st.DB.Where("instance_id = ?", instID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != protocol.TaskFailed || !strings.Contains(task.Result, "无可用 worker") {
		t.Fatalf("根任务 = %s / %q, want FAILED + 无可用 worker", task.Status, task.Result)
	}
	var refreshed store.JobInfo
	if err := st.DB.First(&refreshed, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !refreshed.NextTriggerTime.Equal(next) {
		t.Fatalf("next_trigger_time = %s, want 照常推进到 %s", refreshed.NextTriggerTime, next)
	}
	if *notified != 0 {
		t.Fatalf("notify 被调用 %d 次，want 0（没有可领取任务）", *notified)
	}
}

func TestFireQueuesWhenWorkerOnline(t *testing.T) {
	st, s, notified := newProbeFixture(t, func(string) (bool, error) { return true, nil })
	job := seedProbeJob(t, st, "")

	_, instID, err := s.fire(job, job.NextTriggerTime, "API", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var inst store.JobInstance
	if err := st.DB.First(&inst, instID).Error; err != nil {
		t.Fatal(err)
	}
	if inst.Status != protocol.InstanceWaiting {
		t.Fatalf("实例状态 = %s, want WAITING", inst.Status)
	}
	var task store.InstanceTask
	if err := st.DB.Where("instance_id = ?", instID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != protocol.TaskWaiting {
		t.Fatalf("根任务 = %s, want WAITING", task.Status)
	}
	if *notified != 1 {
		t.Fatalf("notify = %d, want 1（有新任务入队）", *notified)
	}
}

// 在线探测查询失败时按"有节点"处理：宁可排队，也不能因为一次抖动把整批实例判死。
func TestFireQueuesWhenWorkerProbeFails(t *testing.T) {
	st, s, _ := newProbeFixture(t, func(string) (bool, error) { return false, errors.New("db down") })
	job := seedProbeJob(t, st, "")

	_, instID, err := s.fire(job, job.NextTriggerTime, "API", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var inst store.JobInstance
	if err := st.DB.First(&inst, instID).Error; err != nil {
		t.Fatal(err)
	}
	if inst.Status != protocol.InstanceWaiting {
		t.Fatalf("实例状态 = %s, want WAITING（探测失败不判死）", inst.Status)
	}
}

// 广播零节点保持既有语义：根任务就是终态，不产生待领取任务，不该被改判失败。
func TestFireBroadcastZeroNodesStillSucceeds(t *testing.T) {
	st, s, _ := newProbeFixture(t, func(string) (bool, error) { return false, nil })
	job := seedProbeJob(t, st, protocol.ExecuteBroadcast)

	_, instID, err := s.fire(job, job.NextTriggerTime, "API", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var inst store.JobInstance
	if err := st.DB.First(&inst, instID).Error; err != nil {
		t.Fatal(err)
	}
	if inst.Status != protocol.InstanceSucceed || !strings.Contains(inst.Result, "没有在线节点") {
		t.Fatalf("实例 = %s / %q, want SUCCEED + 没有在线节点", inst.Status, inst.Result)
	}
}
