package cleanup

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gojob/internal/protocol"
	"gojob/server/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// seedJob 建一个任务，返回其 ID。lifeCycle=0 表示"没配"。
func seedJob(t *testing.T, st *store.Store, appID string, lifeCycle int) uint64 {
	t.Helper()
	now := store.Now()
	job := store.JobInfo{AppId: appID, JobName: fmt.Sprintf("j-%s-%d", appID, now.UnixNano()),
		Processor: "p", TimeExpressionType: "API", LifeCycle: lifeCycle,
		Enable: true, CreateTime: now, ModifyTime: now}
	if err := st.DB.Create(&job).Error; err != nil {
		t.Fatalf("create job: %v", err)
	}
	return job.ID
}

// seedInstance 建一个实例 + 一份子任务 + 两条日志，created 决定它的归属"哪一天"。
func seedInstance(t *testing.T, st *store.Store, jobID uint64, appID, status string, created time.Time) uint64 {
	t.Helper()
	inst := store.JobInstance{JobId: jobID, AppId: appID, Status: status,
		TotalTaskNum: 1, CreateTime: created, ModifyTime: created}
	if err := st.DB.Create(&inst).Error; err != nil {
		t.Fatalf("create instance: %v", err)
	}
	task := store.InstanceTask{JobId: jobID, InstanceId: inst.ID, AppId: appID,
		Status: status, Attempts: 1, MaxAttempts: 1, CreateTime: created, ModifyTime: created}
	if err := st.DB.Create(&task).Error; err != nil {
		t.Fatalf("create task: %v", err)
	}
	for seq := uint64(1); seq <= 2; seq++ {
		logRow := store.InstanceTaskLog{TaskId: task.ID, InstanceId: inst.ID, Seq: seq,
			Content: "line", CreateTime: created}
		if err := st.DB.Create(&logRow).Error; err != nil {
			t.Fatalf("create log: %v", err)
		}
	}
	return inst.ID
}

func count(t *testing.T, st *store.Store, model any) int64 {
	t.Helper()
	var n int64
	if err := st.DB.Model(model).Count(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func sweep(t *testing.T, r *Runner) int {
	t.Helper()
	n, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return n
}

func daysAgo(n int) time.Time { return store.Now().AddDate(0, 0, -n) }

// TestSweepHonoursPerJobLifeCycle 核心语义：只删"终态 + 超过本任务保留期"的实例，
// 并且连子任务与任务日志一起删掉（否则最大的两张表会永远留着）。
func TestSweepHonoursPerJobLifeCycle(t *testing.T) {
	st := newStore(t)
	job := seedJob(t, st, "app1", 7)
	old := seedInstance(t, st, job, "app1", protocol.InstanceSucceed, daysAgo(30))
	recent := seedInstance(t, st, job, "app1", protocol.InstanceSucceed, daysAgo(2))
	running := seedInstance(t, st, job, "app1", protocol.InstanceRunning, daysAgo(30))

	if n := sweep(t, New(st, nil, 0)); n != 1 {
		t.Fatalf("删除实例数 = %d, want 1", n)
	}
	for _, tc := range []struct {
		name string
		id   uint64
		want bool
	}{{"超期终态", old, false}, {"未超期", recent, true}, {"运行中", running, true}} {
		var got int64
		if err := st.DB.Model(&store.JobInstance{}).Where("id = ?", tc.id).Count(&got).Error; err != nil {
			t.Fatal(err)
		}
		if (got > 0) != tc.want {
			t.Errorf("%s: 实例存在=%v, want %v", tc.name, got > 0, tc.want)
		}
	}
	// 级联：只剩 1 个实例 ⇒ 1 份子任务、2 条日志
	if got := count(t, st, &store.InstanceTask{}); got != 2 {
		t.Errorf("子任务残留 %d 行, want 2", got)
	}
	if got := count(t, st, &store.InstanceTaskLog{}); got != 4 {
		t.Errorf("任务日志残留 %d 行, want 4", got)
	}
}

// TestNothingConfiguredDeletesNothing 两道闸都没开（没配 life_cycle 也没配全局保留天数）时
// 必须一条都不删——存量部署滚上来不能看见"历史突然没了"。
func TestNothingConfiguredDeletesNothing(t *testing.T) {
	st := newStore(t)
	job := seedJob(t, st, "app1", 0)
	seedInstance(t, st, job, "app1", protocol.InstanceFailed, daysAgo(900))

	if n := sweep(t, New(st, nil, 0)); n != 0 {
		t.Fatalf("未配置保留期却删了 %d 条", n)
	}
	if got := count(t, st, &store.JobInstance{}); got != 1 {
		t.Fatalf("实例数 = %d, want 1", got)
	}
}

// TestGlobalRetentionCoversJobsWithoutLifeCycle GOJOB_RETENTION_DAYS 的作用就是让"一个任务
// 都没配 life_cycle"的存量库也能自动收敛。
func TestGlobalRetentionCoversJobsWithoutLifeCycle(t *testing.T) {
	st := newStore(t)
	job := seedJob(t, st, "app1", 0)
	seedInstance(t, st, job, "app1", protocol.InstanceStopped, daysAgo(31))
	seedInstance(t, st, job, "app1", protocol.InstanceSucceed, daysAgo(29))

	if n := sweep(t, New(st, nil, 30)); n != 1 {
		t.Fatalf("删除实例数 = %d, want 1", n)
	}
}

// TestOwnsFilterSkipsOtherReplicas 多副本下别的副本持有的项目不许动。
func TestOwnsFilterSkipsOtherReplicas(t *testing.T) {
	st := newStore(t)
	mine := seedJob(t, st, "app-mine", 7)
	theirs := seedJob(t, st, "app-theirs", 7)
	seedInstance(t, st, mine, "app-mine", protocol.InstanceSucceed, daysAgo(30))
	seedInstance(t, st, theirs, "app-theirs", protocol.InstanceSucceed, daysAgo(30))

	owns := func(appID string) bool { return appID == "app-mine" }
	if n := sweep(t, New(st, owns, 0)); n != 1 {
		t.Fatalf("删除实例数 = %d, want 1", n)
	}
	if got := count(t, st, &store.JobInstance{}); got != 1 {
		t.Fatalf("剩余实例 = %d, want 1（别的项目不许动）", got)
	}
}

// TestOrphanInstancesReclaimed 任务被删掉之后，实例上再没有 life_cycle 可依；
// 只按全局保留天数回收，且必须配了才动。
func TestOrphanInstancesReclaimed(t *testing.T) {
	st := newStore(t)
	job := seedJob(t, st, "app1", 7)
	seedInstance(t, st, job, "app1", protocol.InstanceSucceed, daysAgo(40))
	if err := st.DB.Delete(&store.JobInfo{}, job).Error; err != nil {
		t.Fatal(err)
	}

	if n := sweep(t, New(st, nil, 0)); n != 0 {
		t.Fatalf("没配全局保留天数时不该动孤儿实例，实删 %d", n)
	}
	if n := sweep(t, New(st, nil, 30)); n != 1 {
		t.Fatalf("孤儿实例未被回收，删除数 = %d, want 1", n)
	}
}

// TestBatchesAcrossRounds 单轮跨批：造 45 条超期实例（>BatchSize），一批 20 条要连着删三批，
// 同时验证幂等——再扫一遍没有东西可删。
func TestBatchesAcrossRounds(t *testing.T) {
	st := newStore(t)
	job := seedJob(t, st, "app1", 1)
	for i := range 45 {
		seedInstance(t, st, job, "app1", protocol.InstanceSucceed, daysAgo(10+i))
	}
	if n := sweep(t, New(st, nil, 0)); n != 45 {
		t.Fatalf("删除实例数 = %d, want 45", n)
	}
	if got := count(t, st, &store.JobInstance{}); got != 0 {
		t.Fatalf("实例残留 %d 行", got)
	}
	if got := count(t, st, &store.InstanceTaskLog{}); got != 0 {
		t.Fatalf("日志残留 %d 行", got)
	}
	if n := sweep(t, New(st, nil, 0)); n != 0 {
		t.Fatalf("二次扫描不幂等，又删了 %d 条", n)
	}
}

// TestPerJobLifeCycleWinsOverGlobal 任务上显式配的天数优先于全局值：配了 365 天的任务
// 不能被一个 30 天的全局默认提前删掉。
func TestPerJobLifeCycleWinsOverGlobal(t *testing.T) {
	st := newStore(t)
	keep := seedJob(t, st, "app1", 365)
	seedInstance(t, st, keep, "app1", protocol.InstanceSucceed, daysAgo(200))

	if n := sweep(t, New(st, nil, 30)); n != 0 {
		t.Fatalf("长保留任务被全局值删掉了，删除数 = %d", n)
	}
}
