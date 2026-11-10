// 数据面凭证链路的端到端校验：注册 → token → 心跳/领任务/回报，
// 以及冒用他人身份、口令直连数据面、服务重启后重注册这些边界。
package api

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/server/auth"
	"gojob/server/dispatch"
	"gojob/server/registry"
	"gojob/server/store"
)

type workerEnv struct {
	s *Server
	r *gin.Engine
}

// body 造请求体；token/password 按需给。
func body(kv map[string]any) string {
	bs, err := v2.Marshal(kv)
	if err != nil {
		panic(err)
	}
	return string(bs)
}

// callAs 打一次接口并按 T 解开信封。
func callAs[T any](t *testing.T, e *workerEnv, path, req string) protocol.Resp[T] {
	t.Helper()
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(req)))
	var r protocol.Resp[T]
	if err := v2.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%s: bad response %q: %v", path, w.Body.String(), err)
	}
	return r
}

func newWorkerEnv(t *testing.T) *workerEnv {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "wk.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	reg := registry.New(st.DB, 15*time.Second)
	// 测试里显式给一个集群密钥：无状态令牌的跨副本互认靠的就是"各副本同密钥"
	as := auth.NewSet(st.DB, "test-cluster-secret", auth.ConsoleTokenTTL,
		auth.WorkerTokenTTL, auth.OpenAPITokenTTL)
	s := New(st, reg, dispatch.New(st, reg, nil), nil, as, nil, "sqlite", "")
	for _, a := range [][2]string{{"app1", "pw1"}, {"app2", "pw2"}} {
		hash, err := auth.HashPassword(a[1])
		if err != nil {
			t.Fatal(err)
		}
		now := store.Now()
		if err := st.DB.Create(&store.AppInfo{AppId: a[0], AppName: a[0], Password: hash,
			CreateTime: now, ModifyTime: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	gin.SetMode(gin.TestMode)
	return &workerEnv{s: s, r: s.Router()}
}

func (e *workerEnv) register(t *testing.T, instanceId, appId, plain string) string {
	t.Helper()
	r := callAs[protocol.RegisterResp](t, e, "/worker/register", body(map[string]any{
		"instance_id": instanceId, "app_id": appId, "password": plain,
		"address": "10.0.0.1:7799", "version": "test", "sdk_type": "GO",
	}))
	if r.Code != protocol.CodeOK || r.Data.Token == "" || r.Data.TtlSeconds <= 0 {
		t.Fatalf("register = %+v (code=%d msg=%q)", r.Data, r.Code, r.Message)
	}
	return r.Data.Token
}

func (e *workerEnv) heartbeat(t *testing.T, instanceId, appId, tk string) protocol.Resp[protocol.CancelTasks] {
	t.Helper()
	return callAs[protocol.CancelTasks](t, e, "/worker/heartbeat", body(map[string]any{
		"instance_id": instanceId, "app_id": appId, "address": "10.0.0.1:7799", "token": tk,
	}))
}

// seedTask 造一条到点待派发的任务（等价于调度器入队），返回 task id。
func (e *workerEnv) seedTask(t *testing.T, appId string, instanceId uint64) uint64 {
	t.Helper()
	now := store.Now()
	// 认领层按 job_infos.enable 过滤：任务对应的 Job 必须存在且启用。
	job := store.JobInfo{ID: 1, AppId: appId, JobName: "demo", Processor: "p.demo",
		TimeExpressionType: "API", Enable: true, CreateTime: now, ModifyTime: now}
	if err := e.s.st.DB.FirstOrCreate(&job, 1).Error; err != nil {
		t.Fatal(err)
	}
	err := e.s.st.DB.Create(&store.JobInstance{ID: instanceId, JobId: 1, AppId: appId,
		JobName: "demo", Processor: "p.demo", Status: protocol.InstanceWaiting,
		TotalTaskNum: 1, Source: "API", CreateTime: now, ModifyTime: now}).Error
	if err != nil {
		t.Fatal(err)
	}
	task := store.InstanceTask{JobId: 1, InstanceId: instanceId, AppId: appId, Processor: "p.demo",
		Status: protocol.TaskWaiting, Attempts: 1, MaxAttempts: 1,
		DispatchTime: now, CreateTime: now, ModifyTime: now}
	if err := e.s.st.DB.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	return task.ID
}

func TestWorkerRegisterRejects(t *testing.T) {
	e := newWorkerEnv(t)
	cases := []struct {
		name, req string
		want      int
	}{
		{"口令错误", `{"instance_id":"w-1","app_id":"app1","password":"pw2"}`, protocol.CodeAuthErr},
		{"缺 instance_id", `{"instance_id":"","app_id":"app1","password":"pw1"}`, protocol.CodeBadReq},
		{"项目不存在", `{"instance_id":"w-1","app_id":"nope","password":"pw1"}`, protocol.CodeAuthErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if r := callAs[struct{}](t, e, "/worker/register", tc.req); r.Code != tc.want {
				t.Fatalf("code = %d(%q), want %d", r.Code, r.Message, tc.want)
			}
		})
	}
}

// 完整链路：注册 → 心跳 → 领任务 → 回报；这条路上口令只在注册出现一次。
func TestWorkerDataPlaneWithToken(t *testing.T) {
	e := newWorkerEnv(t)
	tk := e.register(t, "w-1", "app1", "pw1")
	taskId := e.seedTask(t, "app1", 100)

	if r := e.heartbeat(t, "w-1", "app1", tk); r.Code != protocol.CodeOK {
		t.Fatalf("heartbeat code = %d(%q)", r.Code, r.Message)
	}
	r := callAs[protocol.FetchTaskResp](t, e, "/worker/fetchTask", body(map[string]any{
		"instance_id": "w-1", "app_id": "app1", "address": "10.0.0.1:7799",
		"token": tk, "max_task_num": 5, "wait_seconds": 1,
	}))
	if r.Code != protocol.CodeOK {
		t.Fatalf("fetchTask code = %d(%q)", r.Code, r.Message)
	}
	if len(r.Data.Tasks) != 1 || r.Data.Tasks[0].TaskId != taskId {
		t.Fatalf("tasks = %+v, want 1 条 task=%d", r.Data.Tasks, taskId)
	}

	if r := callAs[struct{}](t, e, "/worker/report", body(map[string]any{
		"instance_id": "w-1", "app_id": "app1", "token": tk,
		"task_id": taskId, "success": true,
	})); r.Code != protocol.CodeOK {
		t.Fatalf("report code = %d(%q)", r.Code, r.Message)
	}
	var task store.InstanceTask
	if err := e.s.st.DB.First(&task, taskId).Error; err != nil {
		t.Fatal(err)
	}
	if task.Status != protocol.TaskSucceed || task.WorkerId != "w-1" {
		t.Fatalf("task = %s by %q, want SUCCEED/w-1", task.Status, task.WorkerId)
	}
}

// 冒用：token 只能由签发时绑定的 app_id + instance_id 使用。
func TestWorkerTokenCannotImpersonate(t *testing.T) {
	e := newWorkerEnv(t)
	tk := e.register(t, "w-1", "app1", "pw1")

	t.Run("冒充别的 worker 领任务", func(t *testing.T) {
		r := callAs[protocol.FetchTaskResp](t, e, "/worker/fetchTask", body(map[string]any{
			"instance_id": "w-2", "app_id": "app1", "token": tk, "wait_seconds": 1,
		}))
		if r.Code != protocol.CodeAuthErr {
			t.Fatalf("code = %d(%q), want 401", r.Code, r.Message)
		}
	})
	t.Run("跨项目使用", func(t *testing.T) {
		r := callAs[protocol.FetchTaskResp](t, e, "/worker/fetchTask", body(map[string]any{
			"instance_id": "w-1", "app_id": "app2", "token": tk, "wait_seconds": 1,
		}))
		if r.Code != protocol.CodeAuthErr {
			t.Fatalf("code = %d(%q), want 401", r.Code, r.Message)
		}
	})
	t.Run("冒用身份传日志", func(t *testing.T) {
		r := callAs[any](t, e, "/worker/reportLogs", body(map[string]any{
			"instance_id": "w-2", "app_id": "app1", "token": tk,
			"entries": []any{map[string]any{"task_id": 1, "seq": 1, "level": "INFO",
				"content": "x", "time": 1}},
		}))
		if r.Code != protocol.CodeAuthErr {
			t.Fatalf("code = %d(%q), want 401", r.Code, r.Message)
		}
	})
}

// 口令不再是数据面的通行证：没带有效 token 一律 401，只能回到 /worker/register。
func TestPasswordNoLongerWorksOnDataPlane(t *testing.T) {
	e := newWorkerEnv(t)
	r := e.heartbeat(t, "w-1", "app1", "")
	if r.Code != protocol.CodeAuthErr {
		t.Fatalf("无 token 心跳 code = %d(%q), want 401", r.Code, r.Message)
	}
	if !strings.Contains(r.Message, "register") {
		t.Fatalf("报错应引导去注册，got %q", r.Message)
	}
}

// 无状态令牌不再"重启即全灭"：滚动重启期间 worker 不必重注册。
// 但集群密钥一变（换密钥=换身份），旧票即刻不认，SDK 靠 401 自愈重注册。
func TestServerRestartKeepsTokensUntilSecretChanges(t *testing.T) {
	e := newWorkerEnv(t)
	tk := e.register(t, "w-1", "app1", "pw1")

	// 同密钥重启：换一套全新的令牌体系对象（内存态清零），票仍然有效
	e.s.wtk = auth.NewSet(e.s.st.DB, "test-cluster-secret", auth.ConsoleTokenTTL,
		auth.WorkerTokenTTL, auth.OpenAPITokenTTL).Worker
	if r := e.heartbeat(t, "w-1", "app1", tk); r.Code != protocol.CodeOK {
		t.Fatalf("同密钥重启后旧 token code = %d(%q), want 0", r.Code, r.Message)
	}

	// 换密钥：等价于"这张票不是我们这个集群签的"
	e.s.wtk = auth.NewSet(e.s.st.DB, "rotated-cluster-secret", auth.ConsoleTokenTTL,
		auth.WorkerTokenTTL, auth.OpenAPITokenTTL).Worker
	if r := e.heartbeat(t, "w-1", "app1", tk); r.Code != protocol.CodeAuthErr {
		t.Fatalf("换密钥后旧 token code = %d(%q), want 401", r.Code, r.Message)
	}
	fresh := e.register(t, "w-1", "app1", "pw1")
	if fresh == tk {
		t.Fatal("重注册应拿到新 token")
	}
	if r := e.heartbeat(t, "w-1", "app1", fresh); r.Code != protocol.CodeOK {
		t.Fatalf("新 token code = %d(%q), want 0", r.Code, r.Message)
	}
}

// 改项目口令要立即作废该项目全部在跑 token，否则"改口令踢掉旧凭证"不成立。
func TestAppPasswordRotationRevokesTokens(t *testing.T) {
	e := newWorkerEnv(t)
	other := e.register(t, "w-9", "app2", "pw2")
	tk := e.register(t, "w-1", "app1", "pw1")

	if err := e.s.wtk.RevokeApp("app1"); err != nil {
		t.Fatal(err)
	}
	if r := e.heartbeat(t, "w-1", "app1", tk); r.Code != protocol.CodeAuthErr {
		t.Fatalf("被吊销的 token code = %d(%q), want 401", r.Code, r.Message)
	}
	if r := e.heartbeat(t, "w-9", "app2", other); r.Code != protocol.CodeOK {
		t.Fatalf("别的项目的 token 不应受影响，code = %d(%q)", r.Code, r.Message)
	}
}
