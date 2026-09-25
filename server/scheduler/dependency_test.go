package scheduler

import (
	"path/filepath"
	"testing"

	"gojob/internal/protocol"
	"gojob/server/store"
)

type depFixture struct {
	st    *store.Store
	sched *Scheduler
	picks []uint64 // 每次预选调用对应的 job，验证下游也走分发策略
}

func newDepFixture(t *testing.T) *depFixture {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "dep.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	f := &depFixture{st: st}
	f.sched = New(st, nil, func() {}, func(job *store.JobInfo) string {
		f.picks = append(f.picks, job.ID)
		return ""
	}, nil)
	return f
}

func (f *depFixture) addJob(t *testing.T, name string, upstream uint64, enable bool) *store.JobInfo {
	t.Helper()
	now := store.Now()
	// 真实配置里"依赖触发"是一种调度方式（时间类型=DEPENDENCY）；API 型仅用于测"不自动触发"
	teType := "DEPENDENCY"
	if upstream == 0 {
		teType = "API"
	}
	job := store.JobInfo{AppId: "app1", JobName: name, Processor: "p." + name,
		TimeExpressionType: teType, MaxRetryTimes: 0, InstanceNumLimit: 0,
		UpstreamJobId: upstream, Enable: enable, LogLevel: "INFO",
		NextTriggerTime: now, CreateTime: now, ModifyTime: now}
	if err := f.st.DB.Create(&job).Error; err != nil {
		t.Fatalf("create job %s: %v", name, err)
	}
	// gorm 对带 default 标签的零值字段会跳过写入（enable=false 被 DB 默认 true 覆盖），必须补一次 UPDATE
	if err := f.st.DB.Model(&store.JobInfo{}).Where("id = ?", job.ID).
		Update("enable", enable).Error; err != nil {
		t.Fatalf("set enable: %v", err)
	}
	job.Enable = enable
	return &job
}

func (f *depFixture) instances(t *testing.T, jobId uint64) []store.JobInstance {
	t.Helper()
	var out []store.JobInstance
	if err := f.st.DB.Where("job_id = ?", jobId).Order("id asc").Find(&out).Error; err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *depFixture) tasks(t *testing.T, jobId uint64) []store.InstanceTask {
	t.Helper()
	var out []store.InstanceTask
	if err := f.st.DB.Where("job_id = ?", jobId).Order("id asc").Find(&out).Error; err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDependencyTriggersDownstreamOncePerUpstreamInstance(t *testing.T) {
	f := newDepFixture(t)
	up := f.addJob(t, "up", 0, true)
	down := f.addJob(t, "down", up.ID, true)

	f.sched.TriggerDependency(up.ID, 100, protocol.InstanceSucceed)

	insts := f.instances(t, down.ID)
	if len(insts) != 1 {
		t.Fatalf("downstream instances = %d, want 1", len(insts))
	}
	inst := insts[0]
	if inst.Status != protocol.InstanceWaiting {
		t.Fatalf("instance status = %s, want WAITING", inst.Status)
	}
	if inst.Source != protocol.SourceDependency {
		t.Fatalf("instance source = %s, want %s", inst.Source, protocol.SourceDependency)
	}
	if inst.UpstreamInstanceId == nil || *inst.UpstreamInstanceId != 100 {
		t.Fatalf("upstream_instance_id = %v, want 100", inst.UpstreamInstanceId)
	}
	tasks := f.tasks(t, down.ID)
	if len(tasks) != 1 || tasks[0].Status != protocol.TaskWaiting {
		t.Fatalf("downstream tasks = %+v, want 1 条 WAITING", tasks)
	}
	if len(f.picks) != 1 || f.picks[0] != down.ID {
		t.Fatalf("pick calls = %v, want 下游任务走分发策略预选", f.picks)
	}

	// 同一个上游实例重放（回报重试/重启重放）不得再拉起第二个下游实例
	f.sched.TriggerDependency(up.ID, 100, protocol.InstanceSucceed)
	if got := len(f.instances(t, down.ID)); got != 1 {
		t.Fatalf("downstream instances after replay = %d, want 1（幂等）", got)
	}
	// 上游另一个实例成功 → 再触发一次
	f.sched.TriggerDependency(up.ID, 101, protocol.InstanceSucceed)
	if got := len(f.instances(t, down.ID)); got != 2 {
		t.Fatalf("downstream instances = %d, want 2", got)
	}
}

func TestDependencyOnlyTriggersOnSuccess(t *testing.T) {
	f := newDepFixture(t)
	up := f.addJob(t, "up", 0, true)
	down := f.addJob(t, "down", up.ID, true)

	for _, status := range []string{protocol.InstanceFailed, protocol.InstanceStopped} {
		f.sched.TriggerDependency(up.ID, 200, status)
	}
	if got := len(f.instances(t, down.ID)); got != 0 {
		t.Fatalf("downstream instances = %d, want 0（失败/被停不传导下游）", got)
	}
}

func TestDependencySkipsDisabledDownstreamAndUnrelatedJobs(t *testing.T) {
	f := newDepFixture(t)
	up := f.addJob(t, "up", 0, true)
	off := f.addJob(t, "off", up.ID, false)
	other := f.addJob(t, "other", 0, true)

	f.sched.TriggerDependency(up.ID, 300, protocol.InstanceSucceed)

	for _, j := range []*store.JobInfo{off, other} {
		if got := len(f.instances(t, j.ID)); got != 0 {
			t.Fatalf("job %d instances = %d, want 0", j.ID, got)
		}
	}
}

// 下游被依赖链拉起时不应推进自身 next_trigger_time（依赖触发不参与 cron 节奏）。
func TestDependencyDoesNotAdvanceSchedule(t *testing.T) {
	f := newDepFixture(t)
	up := f.addJob(t, "up", 0, true)
	down := f.addJob(t, "down", up.ID, true)
	before := down.NextTriggerTime

	f.sched.TriggerDependency(up.ID, 400, protocol.InstanceSucceed)

	var after store.JobInfo
	if err := f.st.DB.First(&after, down.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !after.NextTriggerTime.Equal(before) {
		t.Fatalf("next_trigger_time = %s, want 不变 (%s)", after.NextTriggerTime, before)
	}
	if after.Enable != true {
		t.Fatal("下游任务不应被依赖触发改变启用状态")
	}
}

// 下游同时是定时任务时，依赖触发只是额外插一次实例，不影响其自身调度。
func TestDependencyRespectsInstanceNumLimit(t *testing.T) {
	f := newDepFixture(t)
	now := store.Now()
	up := f.addJob(t, "up", 0, true)
	down := f.addJob(t, "down", up.ID, true)
	if err := f.st.DB.Model(&store.JobInfo{}).Where("id = ?", down.ID).
		Update("instance_num_limit", 1).Error; err != nil {
		t.Fatal(err)
	}
	// 先占一个运行中实例
	running := store.JobInstance{JobId: down.ID, AppId: "app1", JobName: "down",
		Status: protocol.InstanceRunning, RunningTimes: 1, TotalTaskNum: 1,
		CreateTime: now, ModifyTime: now}
	if err := f.st.DB.Create(&running).Error; err != nil {
		t.Fatal(err)
	}

	f.sched.TriggerDependency(up.ID, 500, protocol.InstanceSucceed)

	insts := f.instances(t, down.ID)
	if len(insts) != 1 || insts[0].ID != running.ID {
		t.Fatalf("instances = %+v, want 仅原有实例 %d（超实例上限应跳过本次依赖触发）", insts, running.ID)
	}
}
