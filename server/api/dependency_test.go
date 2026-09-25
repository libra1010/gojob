// 依赖触发的保存期校验：成环/跨项目/悬空上游必须在写库前拒绝。
package api

import (
	"path/filepath"
	"strings"
	"testing"

	"gojob/server/auth"
	"gojob/server/store"
)

func newDepServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "dep.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 这几个测试只碰保存期校验与 authApp，但 Server 一旦装配就得有完整令牌体系，
	// 免得"某个 handler 里 s.pj 是 nil"这种问题只能在 panic 时才发现。
	as := auth.NewSet(st.DB, "dep-test-secret", auth.ConsoleTokenTTL,
		auth.WorkerTokenTTL, auth.OpenAPITokenTTL)
	return &Server{st: st, am: as.Console, wtk: as.Worker, pj: as.OpenAPI}
}

func addDepJob(t *testing.T, st *store.Store, appId, name string, upstream uint64) uint64 {
	t.Helper()
	now := store.Now()
	job := store.JobInfo{AppId: appId, JobName: name, Processor: "p." + name,
		TimeExpressionType: "API", UpstreamJobId: upstream,
		NextTriggerTime: now, CreateTime: now, ModifyTime: now}
	if err := st.DB.Create(&job).Error; err != nil {
		t.Fatalf("create job %s: %v", name, err)
	}
	return job.ID
}

func TestValidateUpstreamAcceptsLegalChain(t *testing.T) {
	s := newDepServer(t)
	a := addDepJob(t, s.st, "app1", "a", 0)
	b := addDepJob(t, s.st, "app1", "b", a)

	if err := s.validateUpstream(0, "app1", a); err != nil {
		t.Fatalf("挂到无上游的任务下应合法: %v", err)
	}
	if err := s.validateUpstream(0, "app1", b); err != nil {
		t.Fatalf("挂到两层链末端应合法: %v", err)
	}
	if err := s.validateUpstream(b, "app1", 0); err != nil {
		t.Fatalf("upstream=0（取消依赖）应直接通过: %v", err)
	}
}

func TestValidateUpstreamRejects(t *testing.T) {
	s := newDepServer(t)
	a := addDepJob(t, s.st, "app1", "a", 0)
	other := addDepJob(t, s.st, "app2", "other", 0)
	cases := []struct {
		name            string
		jobId, upstream uint64
		wantMsg         string
	}{
		{"依赖自己", a, a, "自己"},
		{"上游不存在", 0, 9999, "不存在"},
		{"跨项目", 0, other, "只能同项目内"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.validateUpstream(tc.jobId, "app1", tc.upstream)
			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("err = %v, want 含 %q", err, tc.wantMsg)
			}
		})
	}
}

func TestValidateDependencySchedule(t *testing.T) {
	cases := []struct {
		name      string
		teType    string
		upstream  uint64
		wantError bool
	}{
		{"依赖触发必须带上游", "DEPENDENCY", 0, true},
		{"依赖触发带上游合法", "DEPENDENCY", 7, false},
		{"小写也认", "dependency", 7, false},
		{"只写依赖不写上游的默认类型", "", 0, false},
		{"定时任务不允许留悬空上游", "CRON", 7, true},
		{"定时任务无上游", "CRON", 0, false},
		{"API 型无上游", "API", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDependencySchedule(tc.teType, tc.upstream)
			if (err != nil) != tc.wantError {
				t.Fatalf("validateDependencySchedule(%q, %d) = %v, wantError=%v",
					tc.teType, tc.upstream, err, tc.wantError)
			}
		})
	}
}

// A→B 已成立时再让 B→A 就成环，必须拒绝。
func TestValidateUpstreamRejectsCycle(t *testing.T) {
	s := newDepServer(t)
	a := addDepJob(t, s.st, "app1", "a", 0)
	b := addDepJob(t, s.st, "app1", "b", a)

	if err := s.validateUpstream(a, "app1", b); err == nil {
		t.Fatal("成环配置必须被拒绝")
	} else if !strings.Contains(err.Error(), "成环") {
		t.Fatalf("err = %v, want 成环提示", err)
	}
	// 三层环 A→B→C→A
	c := addDepJob(t, s.st, "app1", "c", b)
	if err := s.validateUpstream(a, "app1", c); err == nil {
		t.Fatal("三层成环必须被拒绝")
	}
}

// 直接改库留下的环不能让保存流程死循环，长度上限要拦住。
func TestValidateUpstreamStopsAtChainLimit(t *testing.T) {
	s := newDepServer(t)
	x := addDepJob(t, s.st, "app1", "x", 0)
	y := addDepJob(t, s.st, "app1", "y", x)
	if err := s.st.DB.Model(&store.JobInfo{}).Where("id = ?", x).Update("upstream_job_id", y).Error; err != nil {
		t.Fatal(err)
	}
	err := s.validateUpstream(0, "app1", x)
	if err == nil || !strings.Contains(err.Error(), "成环") {
		t.Fatalf("err = %v, want 链长上限保护", err)
	}
}
