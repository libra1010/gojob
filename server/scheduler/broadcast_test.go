package scheduler

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/store"
)

func newBroadcastFixture(t *testing.T, targets []string) (*store.Store, *Scheduler, *[]uint64) {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "bcast.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	var picks []uint64
	s := New(st, nil, func() {}, func(job *store.JobInfo) string {
		picks = append(picks, job.ID)
		return "should-not-be-used" // 广播不该走分发策略预选
	}, func(*store.JobInfo) []string { return slices(targets) })
	return st, s, &picks
}

// slices 复制一份，避免被测代码改到调用方的切片（也顺手确认 fanout 返回值被当只读用）。
func slices(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func (f *bcastJob) create(t *testing.T, st *store.Store) *store.JobInfo {
	t.Helper()
	now := store.Now()
	job := store.JobInfo{AppId: "app1", JobName: f.name, Processor: "p." + f.name,
		TimeExpressionType: "API", ExecuteType: f.execType, Enable: true, LogLevel: "INFO",
		MaxRetryTimes: 1, InstanceNumLimit: 0,
		NextTriggerTime: now, CreateTime: now, ModifyTime: now}
	if err := st.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return &job
}

type bcastJob struct {
	name     string
	execType string
}

// 广播 = 根任务不派给任何人 + 每台在线节点一份指名子任务。
func TestFireBroadcastFansOutOneTaskPerWorker(t *testing.T) {
	st, s, _ := newBroadcastFixture(t, []string{"w1", "w2", "w3"})
	job := (&bcastJob{"bcast", protocol.ExecuteBroadcast}).create(t, st)

	skipped, instID, err := s.fire(job, job.NextTriggerTime, "API", "", 0)
	if err != nil || skipped {
		t.Fatalf("fire: skipped=%v err=%v", skipped, err)
	}
	var rows []store.InstanceTask
	if err := st.DB.Where("instance_id = ?", instID).Order("id asc").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("任务行数 = %d, want 4（1 根 + 3 份广播）", len(rows))
	}
	root := rows[0]
	if root.TaskName != protocol.RootTaskName || root.Status != protocol.TaskSucceed {
		t.Fatalf("广播的根任务应建出来即终态：%+v", root)
	}
	if !strings.Contains(root.Result, "3") {
		t.Fatalf("根任务结果应说明广播了几台：%q", root.Result)
	}
	for i, want := range []string{"w1", "w2", "w3"} {
		kid := rows[i+1]
		if kid.TaskName != protocol.BroadcastTaskName || kid.TargetWorkerId != want {
			t.Fatalf("第 %d 份广播 target=%q name=%q, want 指名给 %s", i+1, kid.TargetWorkerId, kid.TaskName, want)
		}
		if kid.Status != protocol.TaskWaiting || kid.ParentTaskId != root.ID {
			t.Fatalf("第 %d 份广播 status=%s parent=%d", i+1, kid.Status, kid.ParentTaskId)
		}
	}
	var inst store.JobInstance
	if err := st.DB.First(&inst, instID).Error; err != nil {
		t.Fatal(err)
	}
	if inst.TotalTaskNum != 4 || inst.RunningTimes != 3 {
		t.Fatalf("实例计数 total=%d running=%d, want 4/3", inst.TotalTaskNum, inst.RunningTimes)
	}
}

// 没有在线节点时广播必须当场收尾：留下一个永不结束的实例比失败更难排查。
func TestFireBroadcastWithoutWorkersSettlesImmediately(t *testing.T) {
	st, s, _ := newBroadcastFixture(t, nil)
	job := (&bcastJob{"bcast0", protocol.ExecuteBroadcast}).create(t, st)

	_, instID, err := s.fire(job, job.NextTriggerTime, "API", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var inst store.JobInstance
	if err := st.DB.First(&inst, instID).Error; err != nil {
		t.Fatal(err)
	}
	if inst.Status != protocol.InstanceSucceed || inst.RunningTimes != 0 {
		t.Fatalf("无节点广播后实例 = %s/running %d, want SUCCEED/0（否则它永远挂着）",
			inst.Status, inst.RunningTimes)
	}
	if !strings.Contains(inst.Result, "没有在线节点") {
		t.Fatalf("实例结果应说明什么都没执行：%q", inst.Result)
	}
}

// 非广播类型不受 fanout 影响：仍然是一实例一任务、走分发策略预选。
func TestFireStandaloneIgnoresFanout(t *testing.T) {
	st, s, picks := newBroadcastFixture(t, []string{"w1", "w2"})
	job := (&bcastJob{"plain", protocol.ExecuteStandalone}).create(t, st)

	_, instID, err := s.fire(job, job.NextTriggerTime, "API", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var rows []store.InstanceTask
	if err := st.DB.Where("instance_id = ?", instID).Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("STANDALONE 产生了 %d 行任务, want 1", len(rows))
	}
	if rows[0].TargetWorkerId != "should-not-be-used" {
		t.Fatalf("STANDALONE 没走分发策略预选：target=%q", rows[0].TargetWorkerId)
	}
	if rows[0].TaskKey != protocol.RootTaskKey || rows[0].Status != protocol.TaskWaiting {
		t.Fatalf("根任务字段不完整：%+v", rows[0])
	}
	if len(*picks) != 1 {
		t.Fatalf("pick 被调用 %d 次, want 1", len(*picks))
	}
}

// MAP 型建单时只有一行根任务，子任务是运行期 map 出来的（建单不该预派生）。
func TestFireMapCreatesOnlyRootTask(t *testing.T) {
	st, s, _ := newBroadcastFixture(t, []string{"w1"})
	job := (&bcastJob{"mr", protocol.ExecuteMap}).create(t, st)

	_, instID, err := s.fire(job, job.NextTriggerTime, "API", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := st.DB.Model(&store.InstanceTask{}).
		Where("instance_id = ?", instID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("MAP 建单产生 %d 行任务, want 1", n)
	}
	var inst store.JobInstance
	if err := st.DB.First(&inst, instID).Error; err != nil {
		t.Fatal(err)
	}
	if inst.ExecuteType != protocol.ExecuteMap {
		t.Fatalf("实例没记下执行类型：%+v", inst)
	}
	// 建单时间隔 0 也必须可认领：dispatch_time 落在未来会让任务饿到下一轮
	if time.Since(rowsDispatchTime(t, st, instID)) > time.Second {
		t.Fatal("dispatch_time 不在可认领窗口内")
	}
}

func rowsDispatchTime(t *testing.T, st *store.Store, instID uint64) time.Time {
	t.Helper()
	var task store.InstanceTask
	if err := st.DB.Where("instance_id = ?", instID).First(&task).Error; err != nil {
		t.Fatal(err)
	}
	return task.DispatchTime
}
