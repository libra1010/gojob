// SDK 侧的凭证自愈：token 被服务端作废（重启/过期）后，下一次数据面调用应自动重注册并重放，
// 业务代码与任务回报都不该因为"票到期"而丢请求。
package worker

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
)

type fakeServer struct {
	t     *testing.T
	plain string

	mu        sync.Mutex
	token     string // 当前唯一有效 token；置空等价于服务重启
	registers int
	rejected  []string // 被拒的请求身份，用于断言冒用场景
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bs, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Fatalf("read %s: %v", r.URL.Path, err)
	}
	var body map[string]any
	if err := v2.Unmarshal(bs, &body); err != nil {
		f.t.Fatalf("decode %s (%s): %v", r.URL.Path, bs, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(code int, msg string, data any) {
		bs, err := v2.Marshal(protocol.Resp[any]{Code: code, Message: msg, Data: data})
		if err != nil {
			f.t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(bs)
	}
	switch r.URL.Path {
	case "/worker/register":
		if f.plain != body["password"] || body["app_id"] != "app1" {
			write(protocol.CodeAuthErr, "wrong password", nil)
			return
		}
		f.registers++
		f.token = fmt.Sprintf("tk-%d", f.registers)
		write(protocol.CodeOK, "", protocol.RegisterResp{Token: f.token, TtlSeconds: 600})
	case "/worker/heartbeat", "/worker/fetchTask", "/worker/report":
		who := fmt.Sprintf("%s/%s", body["app_id"], body["token"])
		if body["token"] != f.token || body["app_id"] != "app1" {
			f.rejected = append(f.rejected, who)
			write(protocol.CodeAuthErr, "worker token missing or expired, re-register", nil)
			return
		}
		write(protocol.CodeOK, "", protocol.CancelTasks{})
	default:
		f.t.Errorf("unexpected path %s", r.URL.Path)
	}
}

func (f *fakeServer) dropTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = ""
}

func (f *fakeServer) registerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers
}

func newTestWorker(url string) *Worker {
	return New(Config{ServerAddress: url, AppId: "app1", Password: "pw1", Address: "10.0.0.1:7799"})
}

func beatBody(inst string) *protocol.HeartbeatReq {
	return &protocol.HeartbeatReq{WorkerInfo: protocol.WorkerInfo{
		InstanceId: inst, AppId: "app1", Address: "10.0.0.1:7799"}}
}

func TestSDKRegistersOnceAndCarriesToken(t *testing.T) {
	f := &fakeServer{t: t, plain: "pw1"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := newTestWorker(srv.URL)
	ctx := t.Context()

	if err := w.register(ctx); err != nil {
		t.Fatalf("register: %v", err)
	}
	if w.currentToken() != "tk-1" {
		t.Fatalf("token = %q, want tk-1", w.currentToken())
	}
	if _, err := call[protocol.CancelTasks](w, ctx, "/worker/heartbeat", beatBody(w.instanceID)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if n := f.registerCount(); n != 1 {
		t.Fatalf("register 次数 = %d, want 1（心跳不该重复注册）", n)
	}
}

// 服务端票表清空（重启/过期）后，业务请求应自己把票续上，不向调用方抛错。
func TestSDKReRegistersAfterTokenInvalidated(t *testing.T) {
	f := &fakeServer{t: t, plain: "pw1"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := newTestWorker(srv.URL)
	ctx := t.Context()

	if err := w.register(ctx); err != nil {
		t.Fatal(err)
	}
	f.dropTokens()
	if _, err := call[protocol.CancelTasks](w, ctx, "/worker/fetchTask", &protocol.FetchTaskReq{
		InstanceId: w.instanceID, AppId: "app1", WaitSeconds: 1,
	}); err != nil {
		t.Fatalf("token 作废后应自动重注册恢复: %v", err)
	}
	if n := f.registerCount(); n != 2 {
		t.Fatalf("register 次数 = %d, want 2", n)
	}

	// 口令被改掉后无法再自愈：错误必须原样抛给调用方，不能静默卡住
	f.mu.Lock()
	f.plain = "rotated"
	f.token = ""
	f.mu.Unlock()
	if _, err := call[protocol.CancelTasks](w, ctx, "/worker/heartbeat", beatBody(w.instanceID)); err == nil {
		t.Fatal("口令失效时 call 应返回错误")
	} else if !strings.Contains(err.Error(), "password") {
		t.Fatalf("err = %v, want 指出口令问题", err)
	}
}

// 多个 goroutine 同时撞上 401 时只注册一次，其余复用新票。
func TestConcurrentReRegisterRegistersOnce(t *testing.T) {
	f := &fakeServer{t: t, plain: "pw1"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := newTestWorker(srv.URL)
	ctx := t.Context()

	if err := w.register(ctx); err != nil {
		t.Fatal(err)
	}
	f.dropTokens()

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = call[protocol.CancelTasks](w, ctx, "/worker/heartbeat", beatBody(w.instanceID))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发请求失败: %v", i, err)
		}
	}
	if n := f.registerCount(); n != 2 {
		t.Fatalf("register 次数 = %d, want 2（初次 + 一轮自愈）", n)
	}
}

// 启动即注册失败（口令错/项目不存在）必须让 Run 立即报错，而不是默默空跑。
func TestRunFailsFastWithoutRegistration(t *testing.T) {
	f := &fakeServer{t: t, plain: "never-matches"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := newTestWorker(srv.URL)

	err := w.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "register") {
		t.Fatalf("Run err = %v, want 注册失败", err)
	}
}

// 冷启动撞上"服务端还没起来 / accept 队列被打满"：连不上要退避重试，不能就此永久缺席。
// 这条是 200 节点实机压出来的：本机 kern.ipc.somaxconn=128，200 个节点同时注册时
// 稳定有 6~23 个在 /worker/register 上吃到 connection reset，旧行为是这些节点直接退出。
func TestStartupRegisterRetriesUntilServerAppears(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil { // 先占号再释放：前几次注册必然 connection refused
		t.Fatal(err)
	}

	f := &fakeServer{t: t, plain: "pw1"}
	srv := httptest.NewUnstartedServer(f)
	go func() {
		time.Sleep(500 * time.Millisecond) // 留出至少一次"连不上"
		l, e := net.Listen("tcp", addr)
		if e != nil {
			t.Errorf("稍后补起的监听失败: %v", e)
			return
		}
		srv.Listener = l
		srv.Start()
	}()
	defer srv.Close()

	w := newTestWorker("http://" + addr)
	if err := w.registerAtStartup(t.Context()); err != nil {
		t.Fatalf("服务端补起后注册应当成功, got %v", err)
	}
	if n := f.registerCount(); n != 1 {
		t.Fatalf("成功注册次数 = %d, want 1（成功即止，不该再重试一轮）", n)
	}
}

// 与之相对：服务端明确答了"不行"（口令错、要求加密、没配 key）时不许重试——
// 重试不会让配置变对，只会把配置故障伪装成网络故障，拖住启动。
func TestStartupRegisterDoesNotRetryRejection(t *testing.T) {
	f := &fakeServer{t: t, plain: "never-matches"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := newTestWorker(srv.URL)

	start := time.Now()
	err := w.registerAtStartup(t.Context())
	if err == nil {
		t.Fatal("口令不对时注册应失败")
	}
	var se *serverError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want 服务端业务错误（不该被当成连不上）", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("业务拒绝被重试了（耗时 %v）", d)
	}
}
