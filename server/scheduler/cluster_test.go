// 多副本下的调度正确性：装载归属、同一触发只能被消费一次、僵尸主写不进库。
package scheduler

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gojob/server/cluster"
	"gojob/server/store"
)

type clusterFixture struct {
	st *store.Store
}

func newClusterFixture(t *testing.T) *clusterFixture {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "cluster.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	f := &clusterFixture{st: st}
	f.addApp(t, "app1")
	return f
}

// addApp 建项目：归属以 app 为粒度，所以"这个项目存不存在"直接决定租约能不能拿到。
func (f *clusterFixture) addApp(t *testing.T, appId string) {
	t.Helper()
	now := store.Now()
	if err := f.st.DB.Create(&store.AppInfo{AppId: appId, AppName: appId, Password: "x",
		CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
}

// addJob 建一个 CRON 型任务；表达式只是占位，测试里不靠它算下次时间。
func (f *clusterFixture) addJob(t *testing.T, appId, name string, next time.Time) *store.JobInfo {
	t.Helper()
	now := store.Now()
	job := store.JobInfo{AppId: appId, JobName: name, Processor: "p." + name,
		TimeExpressionType: "CRON", TimeExpression: "0 0 * * *", LogLevel: "INFO",
		MaxRetryTimes: 0, Enable: true, NextTriggerTime: next,
		CreateTime: now, ModifyTime: now}
	if err := f.st.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return &job
}

func (f *clusterFixture) sched(name string, leases *cluster.Leases, picks *[]uint64) *Scheduler {
	return New(f.st, leases, func() {}, func(job *store.JobInfo) string {
		if picks != nil {
			*picks = append(*picks, job.ID)
		}
		return ""
	}, nil)
}

func (f *clusterFixture) countInstances(t *testing.T, jobId uint64) int64 {
	t.Helper()
	var n int64
	if err := f.st.DB.Model(&store.JobInstance{}).Where("job_id = ?", jobId).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *clusterFixture) reload(t *testing.T, id uint64) *store.JobInfo {
	t.Helper()
	var job store.JobInfo
	if err := f.st.DB.First(&job, id).Error; err != nil {
		t.Fatal(err)
	}
	return &job
}

// 粗扫只装载本机持有租约的项目：别的副本的项目连时间轮都不上，谈不上重复触发。
func TestScanOnlyArmsOwnedApps(t *testing.T) {
	f := newClusterFixture(t)
	now := store.Now()
	j1 := f.addJob(t, "app1", "owned", now)
	j2 := f.addJob(t, "app2", "other", now)

	leasesA := cluster.NewLeases(f.st.DB, "server-a", cluster.LeaseTTL)
	leasesA.Sync() // app1 有行；app2 这个 app 还不存在，所以拿不到它
	sA := f.sched("a", leasesA, nil)
	sA.scan()
	if len(sA.armed) != 1 || !sA.armed[j1.ID].Equal(truncMs(j1.NextTriggerTime)) {
		t.Fatalf("server-a armed = %v, want 只有 job %d", sA.armed, j1.ID)
	}

	// app2 是"存在但不由本机调度"的项目：另一台副本没 Sync 过，就不该替它装载
	empty := cluster.NewLeases(f.st.DB, "server-b", cluster.LeaseTTL)
	sB := f.sched("b", empty, nil)
	sB.scan()
	if len(sB.armed) != 0 {
		t.Fatalf("server-b armed = %v, 没有归属时一台副本也不该装载任何任务", sB.armed)
	}
	if sB.leases.Owns("app1") {
		t.Fatal("server-b 不该以为 app1 是自己的")
	}
	if j2 == nil {
		t.Fatal("app2 的任务只是占位")
	}
}

// 失去归属要能整片撤轮，否则僵尸主仍会按内存里的轮子一路撞到 fencing。
func TestDropAppCancelsArmedJobs(t *testing.T) {
	f := newClusterFixture(t)
	now := store.Now()
	f.addApp(t, "app2")
	keep := f.addJob(t, "app2", "keep", now)
	drop := f.addJob(t, "app1", "drop", now)

	leases := cluster.NewLeases(f.st.DB, "server-a", cluster.LeaseTTL)
	lost := make(chan string, 1)
	leases.OnLost(func(appId string) { lost <- appId })
	leases.Sync() // 两个项目都归本机，才有"撤一个、留一个"可测
	s := f.sched("a", leases, nil)
	s.scan()
	if len(s.armed) != 2 {
		t.Fatalf("armed = %v, want 2 个任务", s.armed)
	}
	s.DropApp("app1")
	if _, ok := s.armed[drop.ID]; ok {
		t.Fatal("app1 的任务应被撤下")
	}
	if _, ok := s.armed[keep.ID]; !ok {
		t.Fatal("别的项目的任务不该被误撤")
	}
	if _, ok := s.appOf[drop.ID]; ok {
		t.Fatal("appOf 索引也要一起清掉，否则会无界增长")
	}
	select {
	case appId := <-lost:
		t.Fatalf("DropApp 是本地主动撤轮，不该触发 OnLost（收到 %s）", appId)
	default:
	}
}

// 同一时刻的触发只能被一个副本消费：第二个副本 CAS 推进 next_trigger_time 失败，
// 事务回滚，既不产生第二个实例，也不留下半条任务。
func TestTriggerConsumedOnlyOnce(t *testing.T) {
	f := newClusterFixture(t)
	now := store.Now()
	job := f.addJob(t, "app1", "cadence", now)
	s := f.sched("a", nil, nil)

	first := now.Add(time.Minute)
	skipped, id, err := s.fire(job, first, "CRON", "", 0)
	if err != nil || skipped || id == 0 {
		t.Fatalf("首次触发 skipped=%v id=%d err=%v", skipped, id, err)
	}
	// 另一个副本拿的是同一份"还没推进过"的 job 快照
	if _, _, err := s.fire(job, first.Add(time.Second), "CRON", "", 0); !errors.Is(err, ErrTriggerTaken) {
		t.Fatalf("重复消费应被判负，got %v", err)
	}
	if n := f.countInstances(t, job.ID); n != 1 {
		t.Fatalf("实例数 = %d, want 1（抢输的一方必须一行都不留）", n)
	}
	if got := f.reload(t, job.ID).NextTriggerTime; !got.Equal(first) {
		t.Fatalf("next_trigger_time = %v, want 第一次推进的值 %v", got, first)
	}
	var tasks int64
	f.st.DB.Model(&store.InstanceTask{}).Where("job_id = ?", job.ID).Count(&tasks)
	if tasks != 1 {
		t.Fatalf("待拉取任务数 = %d, want 1", tasks)
	}
}

// 僵尸主：租约已被接管，哪怕它手里的计划时间仍然对得上（CAS 本可以通过），
// 事务内的 fencing 也必须先把它挡下来。这正是 PowerJob 明确没有防护的场景。
func TestZombieOwnerCannotWrite(t *testing.T) {
	f := newClusterFixture(t)
	now := store.Now()
	job := f.addJob(t, "app1", "zombie", now)

	zombie := cluster.NewLeases(f.st.DB, "server-zombie", cluster.LeaseTTL)
	zombie.Sync()
	if !zombie.Owns("app1") {
		t.Fatal("zombie 起初确实是持有者")
	}
	thief := cluster.NewLeases(f.st.DB, "server-thief", cluster.LeaseTTL)
	// 假时钟让 zombie 的租约"过期"，而 zombie 自己不再 Sync（这就是分区里的旧主）
	zombie.SetClock(func() time.Time { return now.Add(-time.Hour) })
	zombie.Sync() // 用过期时钟续约：写下的 lease_expire 落在过去
	thief.Sync()  // 接管，epoch+1
	if !thief.Owns("app1") {
		t.Fatal("server-thief 应已接管 app1")
	}

	s := f.sched("zombie", zombie, nil)
	if _, _, err := s.fire(job, now.Add(time.Minute), "CRON", "", 0); err == nil {
		t.Fatal("僵尸主的触发必须被拒")
	}
	if n := f.countInstances(t, job.ID); n != 0 {
		t.Fatalf("僵尸主写出了 %d 个实例", n)
	}
	if got := f.reload(t, job.ID).NextTriggerTime; !got.Equal(now) {
		t.Fatalf("被拒的触发不该推进 next_trigger_time：%v", got)
	}
	// 新主照常能触发
	s2 := f.sched("thief", thief, nil)
	if _, id, err := s2.fire(job, now.Add(time.Minute), "CRON", "", 0); err != nil || id == 0 {
		t.Fatalf("接管后的新主应能正常触发，id=%d err=%v", id, err)
	}
}

// 手动触发与依赖触发不在任何副本独占的时间轴上：一次请求就是一次事实，
// 所以不要求归属（否则控制台点"运行一次"会被随机一台副本拒掉）。
func TestManualTriggerNeedsNoOwnership(t *testing.T) {
	f := newClusterFixture(t)
	job := f.addJob(t, "app1", "manual", store.Now())
	none := cluster.NewLeases(f.st.DB, "server-idle", cluster.LeaseTTL) // 一次都不 Sync
	s := f.sched("idle", none, nil)
	if err := s.TriggerManually(job); err != nil {
		t.Fatalf("无归属副本手动触发失败: %v", err)
	}
	if n := f.countInstances(t, job.ID); n != 1 {
		t.Fatalf("实例数 = %d, want 1", n)
	}
	if got := f.reload(t, job.ID).NextTriggerTime; !got.Equal(job.NextTriggerTime) {
		t.Fatalf("手动触发不该推进计划时间：%v -> %v", job.NextTriggerTime, got)
	}
}
