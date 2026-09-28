// 项目级授权（ADMIN 的 AppScopes）与任务列表分页/导出的行为校验。
package api

import (
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/server/auth"
	"gojob/server/dispatch"
	"gojob/server/registry"
	"gojob/server/scheduler"
	"gojob/server/store"
)

type scopeEnv struct {
	s *Server
	r *gin.Engine
}

func newScopeEnv(t *testing.T) *scopeEnv {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "scope.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(st.DB, 15*time.Second)
	disp := dispatch.New(st, reg, nil)
	as := auth.NewSet(st.DB, "test-cluster-secret", auth.ConsoleTokenTTL,
		auth.WorkerTokenTTL, auth.OpenAPITokenTTL)
	s := New(st, reg, disp, scheduler.New(st, nil, func() {},
		func(*store.JobInfo) string { return "" },
		func(*store.JobInfo) []string { return nil }), as, nil, "sqlite", "")
	now := store.Now()
	for _, a := range []string{"app1", "app2"} {
		hash, err := auth.HashPassword("pw")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB.Create(&store.AppInfo{AppId: a, AppName: a, Password: hash,
			CreateTime: now, ModifyTime: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	gin.SetMode(gin.TestMode)
	return &scopeEnv{s: s, r: s.Router()}
}

func (e *scopeEnv) addUser(t *testing.T, name, role, scopes string) string {
	t.Helper()
	hash, err := auth.HashPassword("pw123456")
	if err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	if err := e.s.st.DB.Create(&store.UserInfo{Username: name, Password: hash, Nickname: name,
		Role: role, Status: "ACTIVE", AppScopes: scopes,
		CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
	tk, err := e.s.am.Issue(name)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func (e *scopeEnv) addJob(t *testing.T, appId, name string) uint64 {
	t.Helper()
	now := store.Now()
	job := store.JobInfo{AppId: appId, JobName: name, Processor: "p." + name,
		TimeExpressionType: "CRON", TimeExpression: "0 0 * * *", LogLevel: "INFO",
		InstanceParams: `{"k":1}`, MaxRetryTimes: 2, InstanceRetryNum: 1, Concurrency: 5,
		InstanceNumLimit: 3, DispatchStrategy: protocol.DispatchRoundRobin, Enable: true,
		NextTriggerTime: now, CreateTime: now, ModifyTime: now}
	if err := e.s.st.DB.Create(&job).Error; err != nil {
		t.Fatal(err)
	}
	return job.ID
}

func (e *scopeEnv) post(t *testing.T, path, token, req string) protocol.Resp[map[string]any] {
	t.Helper()
	r := httptest.NewRequest("POST", path, strings.NewReader(req))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("X-GoJob-Token", token)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, r)
	var out protocol.Resp[map[string]any]
	if err := v2.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: bad response %q", path, w.Body.String())
	}
	return out
}

func (e *scopeEnv) get(t *testing.T, path, token string) protocol.Resp[map[string]any] {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if token != "" {
		r.Header.Set("X-GoJob-Token", token)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, r)
	var out protocol.Resp[map[string]any]
	if err := v2.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s: bad response %q", path, w.Body.String())
	}
	return out
}

func (e *scopeEnv) enable(t *testing.T, jobId uint64) bool {
	t.Helper()
	var job store.JobInfo
	if err := e.s.st.DB.First(&job, jobId).Error; err != nil {
		t.Fatal(err)
	}
	return job.Enable
}

// ADMIN 的项目授权必须真的能用：三期起就只在超管身上验证过，
// 中间件写入 appId 的时机写错（在 c.Next() 之后）会让 authorize 永远看到空串，
// 结果是"被授权了但每个写操作都被拒"。
func TestScopedAdminCanWriteOwnApp(t *testing.T) {
	e := newScopeEnv(t)
	tk := e.addUser(t, "op1", "ADMIN", "app1")
	jobId := e.addJob(t, "app1", "own")

	r := e.post(t, "/openapi/job/operate", tk, body(map[string]any{"job_id": jobId, "action": "pause"}))
	if r.Code != protocol.CodeOK {
		t.Fatalf("授权项目内的暂停被拒：code=%d msg=%q", r.Code, r.Message)
	}
	if e.enable(t, jobId) {
		t.Fatal("任务没被真的暂停")
	}
	r = e.post(t, "/openapi/job/modify", tk, body(map[string]any{"job_id": jobId, "job_name": ptr("renamed")}))
	if r.Code != protocol.CodeOK {
		t.Fatalf("授权项目内的修改被拒：code=%d msg=%q", r.Code, r.Message)
	}
}

// 越权防护：客户端在请求体里声明哪个 app_id 不算数，归属由 job_id 反查库决定。
// 否则 scope=app1 的 ADMIN 只要写 app_id=app1 就能去停 app2 的任务。
func TestScopedAdminCannotCrossApp(t *testing.T) {
	e := newScopeEnv(t)
	tk := e.addUser(t, "op1", "ADMIN", "app1")
	mine := e.addJob(t, "app1", "mine")
	theirs := e.addJob(t, "app2", "theirs")

	if r := e.post(t, "/openapi/job/operate", tk,
		body(map[string]any{"job_id": mine, "app_id": "app1", "action": "pause"})); r.Code != protocol.CodeOK {
		t.Fatalf("自己项目内的操作被拒：%+v", r)
	}
	r := e.post(t, "/openapi/job/operate", tk,
		body(map[string]any{"job_id": theirs, "app_id": "app1", "action": "pause"}))
	if r.Code != protocol.CodeAuthErr {
		t.Fatalf("跨项目操作竟被放行：code=%d msg=%q", r.Code, r.Message)
	}
	if !e.enable(t, theirs) {
		t.Fatal("被拒的操作仍然改了 app2 的任务")
	}
	// 删除同理
	if r := e.post(t, "/openapi/job/delete", tk,
		body(map[string]any{"job_id": theirs, "app_id": "app1"})); r.Code != protocol.CodeAuthErr {
		t.Fatalf("跨项目删除竟被放行：%+v", r)
	}
}

// 只读账号一律拒写，超管不受项目限制。
func TestReadonlyAndSuperAdmin(t *testing.T) {
	e := newScopeEnv(t)
	ro := e.addUser(t, "ro", "READONLY", "*")
	super := e.addUser(t, "root", "SUPER_ADMIN", "*")
	jobId := e.addJob(t, "app2", "x")

	if r := e.post(t, "/openapi/job/operate", ro,
		body(map[string]any{"job_id": jobId, "action": "pause"})); r.Code != protocol.CodeAuthErr {
		t.Fatalf("READONLY 写操作未被拒：%+v", r)
	}
	if r := e.post(t, "/openapi/job/operate", super,
		body(map[string]any{"job_id": jobId, "action": "pause"})); r.Code != protocol.CodeOK {
		t.Fatalf("超管被拒：%+v", r)
	}
}

// 任务列表语义沿用兼容协议的会话接口：index 从 0 起、keyword 命中任务名/描述、
// page_size<=0 一次给全（建单抽屉的上游候选要全量，不能被分页切掉）。
func TestListJobsPagingAndKeyword(t *testing.T) {
	e := newScopeEnv(t)
	tk := e.addUser(t, "root", "SUPER_ADMIN", "*")
	for i := 0; i < 5; i++ {
		e.addJob(t, "app1", strings.Repeat("j", 1)+string(rune('a'+i)))
	}
	e.s.st.DB.Model(&store.JobInfo{}).Where("job_name = ?", "jc").
		Update("job_description", "夜间归档")

	page := getAs[protocol.PageResult[store.JobInfo]](t, &workerEnv{s: e.s, r: e.r},
		"/openapi/job/list?app_id=app1&index=1&page_size=2", tk)
	if page.Code != protocol.CodeOK {
		t.Fatalf("list code=%d msg=%q", page.Code, page.Message)
	}
	if page.Data.TotalItems != 5 || page.Data.TotalPages != 3 || page.Data.Index != 1 {
		t.Fatalf("分页统计 = %+v, want total=5 pages=3 index=1", page.Data)
	}
	if len(page.Data.Data) != 2 {
		t.Fatalf("第二页给了 %d 条, want 2", len(page.Data.Data))
	}

	kw := getAs[protocol.PageResult[store.JobInfo]](t, &workerEnv{s: e.s, r: e.r},
		"/openapi/job/list?app_id=app1&keyword=归档", tk)
	if len(kw.Data.Data) != 1 || kw.Data.Data[0].JobName != "jc" {
		t.Fatalf("keyword=归档 命中 %+v, want 只有 jc", kw.Data.Data)
	}

	all := getAs[protocol.PageResult[store.JobInfo]](t, &workerEnv{s: e.s, r: e.r},
		"/openapi/job/list?app_id=app1&page_size=0", tk)
	if len(all.Data.Data) != 5 || all.Data.TotalPages != 1 {
		t.Fatalf("page_size=0 应给全量：%d 条 / %d 页", len(all.Data.Data), all.Data.TotalPages)
	}
}

// 导出必须能原样导回来：导出→导入是幂等的配置搬迁，字段一个都不能掉。
func TestExportThenImportRoundTrip(t *testing.T) {
	e := newScopeEnv(t)
	tk := e.addUser(t, "root", "SUPER_ADMIN", "*")
	jobId := e.addJob(t, "app1", "roundtrip")

	got := getAs[protocol.JobDTO](t, &workerEnv{s: e.s, r: e.r},
		"/openapi/job/export?job_id="+uitoa(jobId), tk)
	if got.Code != protocol.CodeOK {
		t.Fatalf("export code=%d msg=%q", got.Code, got.Message)
	}
	bs, err := v2.Marshal(got.Data)
	if err != nil {
		t.Fatal(err)
	}
	r := e.post(t, "/openapi/job/import", tk, `{"app_id":"app1","jobs":[`+string(bs)+`]}`)
	if r.Code != protocol.CodeOK {
		t.Fatalf("导入导出 JSON 失败：%+v", r)
	}
	var jobs []store.JobInfo
	e.s.st.DB.Where("job_name = ?", "roundtrip").Find(&jobs)
	if len(jobs) != 2 {
		t.Fatalf("导入后 roundtrip 任务有 %d 条, want 2（原 + 导入）", len(jobs))
	}
	imported := jobs[1]
	want := jobs[0]
	if imported.InstanceParams != want.InstanceParams || imported.MaxRetryTimes != want.MaxRetryTimes ||
		imported.InstanceRetryNum != want.InstanceRetryNum || imported.DispatchStrategy != want.DispatchStrategy ||
		imported.LogLevel != want.LogLevel || imported.Concurrency != want.Concurrency {
		t.Fatalf("往返后配置漂移：\n got %+v\nwant %+v", imported, want)
	}
}

func uitoa(v uint64) string { return strconv.FormatUint(v, 10) }

func ptr(s string) *string { return &s }
