// SDK 侧的集群路由：owner 需要连续确认才迁移、连不上要能退回、提示字段不能破坏老 SDK 的响应形状。
package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
)

// hintServer 一个最小可用的假副本：注册发 token，心跳按当前设定回集群提示。
type hintServer struct {
	t *testing.T

	mu         sync.Mutex
	owner      string   // 心跳回给 worker 的 owner 地址
	servers    []string // 心跳回给 worker 的成员地址
	heartbeats int
}

func (h *hintServer) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.URL.Path {
	case "/worker/register":
		h.write(rw, protocol.RegisterResp{Token: "tk-1", TtlSeconds: 600})
	case "/worker/heartbeat":
		h.heartbeats++
		h.write(rw, protocol.HeartbeatResp{Cluster: protocol.ClusterHint{Owner: h.owner, Servers: h.servers}})
	default:
		h.t.Errorf("unexpected path %s", r.URL.Path)
	}
}

func (h *hintServer) write(rw http.ResponseWriter, data any) {
	bs, err := v2.Marshal(protocol.Resp[any]{Code: protocol.CodeOK, Data: data})
	if err != nil {
		h.t.Fatal(err)
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Write(bs)
}

func (h *hintServer) set(owner string, servers []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.owner, h.servers = owner, servers
}

func (h *hintServer) beats() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.heartbeats
}

func newClusterWorker(addrs ...string) *Worker {
	return New(Config{ServerAddresses: addrs, AppId: "app1", Password: "pw1", Address: "10.0.0.1:7799"})
}

// 单跳不改行为：owner 要连续两跳指向同一地址才迁移；已经在 owner 上就不再动。
func TestOwnerNeedsTwoConsecutiveConfirmations(t *testing.T) {
	w := newClusterWorker("http://10.0.0.1:7700", "http://10.0.0.2:7700")
	owner := "http://10.0.0.2:7700"
	hint := protocol.ClusterHint{Owner: owner,
		Servers: []string{"http://10.0.0.1:7700", owner}}

	if w.applyHint(hint) {
		t.Fatal("第 1 跳就迁移了，防抖没生效")
	}
	if got := w.client.current(); got != "http://10.0.0.1:7700" {
		t.Fatalf("第 1 跳后地址变成 %q，应保持原地址", got)
	}
	if !w.applyHint(hint) {
		t.Fatal("第 2 跳确认后应迁移到 owner")
	}
	if got := w.client.current(); got != owner {
		t.Fatalf("current = %q, want owner %q", got, owner)
	}
	if w.applyHint(hint) {
		t.Fatal("已经在 owner 上了，不该再有动作")
	}
}

// 中途提示消失（租约正在交接 / 对端没配对外地址）要重新开始计数，不能攒够 2 跳就搬。
func TestOwnerHintResetWhenHintDisappears(t *testing.T) {
	w := newClusterWorker("http://10.0.0.1:7700", "http://10.0.0.2:7700")
	owner := "http://10.0.0.2:7700"
	hint := protocol.ClusterHint{Owner: owner}

	w.applyHint(hint)
	if w.applyHint(protocol.ClusterHint{}) {
		t.Fatal("owner 为空时不该迁移")
	}
	if w.applyHint(hint) {
		t.Fatal("提示断过一次后计数被重置，第 3 跳仍不该迁移")
	}
	if !w.applyHint(hint) {
		t.Fatal("重新连续 2 跳后应迁移")
	}
	if got := w.client.current(); got != owner {
		t.Fatalf("current = %q, want %q", got, owner)
	}
}

// owner 地址还没进过已知集合（成员表落后一拍）：忽略这一跳，不能凭空连一个没见过的地址。
func TestOwnerNotInKnownSetIsIgnored(t *testing.T) {
	w := newClusterWorker("http://10.0.0.1:7700")
	hint := protocol.ClusterHint{Owner: "http://10.0.0.99:7700"}
	for i := 0; i < 5; i++ {
		if w.applyHint(hint) {
			t.Fatal("不该迁移到不在已知集合里的地址")
		}
	}
	if got := w.client.current(); got != "http://10.0.0.1:7700" {
		t.Fatalf("current = %q，应保持原地址", got)
	}
	// 成员表补上以后就能迁：说明拦下它的是"未知地址"，不是别的状态卡死了
	w.client.note([]string{hint.Owner})
	for i := 0; i <= ownerConfirmBeats; i++ {
		if w.applyHint(hint) {
			break
		}
	}
	if got := w.client.current(); got != hint.Owner {
		t.Fatalf("地址进集合后连续确认应可迁移，current = %q", got)
	}
}

// owner 连不上时必须自动退回另一个已知副本，并且这次请求要成功——这就是"提示不承重"。
// 退回后首选地址会停在活着的副本上；防抖（连续 2 跳才迁）保证这不会变成来回搬家。
func TestFailOverWhenOwnerUnreachable(t *testing.T) {
	live := &hintServer{t: t}
	srvA := httptest.NewServer(live)
	defer srvA.Close()
	dead := httptest.NewServer(&hintServer{t: t})
	deadURL := dead.URL
	dead.Close() // 现在就是一台已经 down 掉的 owner

	w := newClusterWorker(srvA.URL, deadURL)
	ctx := t.Context()
	if err := w.register(ctx); err != nil {
		t.Fatalf("register: %v", err)
	}
	if !w.client.jumpTo(deadURL) {
		t.Fatal("jumpTo 应把首选地址设成已知的 deadURL")
	}
	if _, err := call[protocol.HeartbeatResp](w, ctx, "/worker/heartbeat",
		&protocol.HeartbeatReq{WorkerInfo: protocol.WorkerInfo{InstanceId: w.instanceID, AppId: "app1"}}); err != nil {
		t.Fatalf("owner 不可达时应退回其它已知地址，实得错误: %v", err)
	}
	if live.beats() != 1 {
		t.Fatalf("活着的副本收到 %d 次心跳, want 1", live.beats())
	}
	if got := w.client.current(); got != srvA.URL {
		t.Fatalf("故障转移后应停在可达的副本上，current = %q", got)
	}
}

// 端到端：worker 只配了 A，靠 A 下发的成员表 + owner 提示自己迁到 B。
func TestBeatConvergesToOwnerEndToEnd(t *testing.T) {
	srvB := &hintServer{t: t}
	b := httptest.NewServer(srvB)
	defer b.Close()

	a := &hintServer{t: t}
	srvA := httptest.NewServer(a)
	defer srvA.Close()
	a.set(b.URL, []string{srvA.URL, b.URL})

	w := newClusterWorker(srvA.URL)
	ctx := t.Context()
	if err := w.register(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < ownerConfirmBeats; i++ {
		w.beat(ctx)
	}
	if got := w.client.current(); got != b.URL {
		t.Fatalf("%d 跳后 current = %q, want owner %q", ownerConfirmBeats, got, b.URL)
	}
	if got := a.beats(); got != ownerConfirmBeats {
		t.Fatalf("A 收到 %d 次心跳, want %d", got, ownerConfirmBeats)
	}
	w.beat(ctx) // 之后每一跳都打在 owner 上，A 不再收到
	if got := a.beats(); got != ownerConfirmBeats {
		t.Fatalf("迁移后 A 又收到心跳（%d 次），首选地址没生效", got)
	}
	if got := srvB.beats(); got != 1 {
		t.Fatalf("B 收到 %d 次心跳, want 1", got)
	}
}

// 老 SDK 只解 CancelTasks：内联之后 cancel_task_ids 必须还在 data 顶层。
func TestHeartbeatRespKeepsTopLevelCancelTaskIds(t *testing.T) {
	bs, err := v2.Marshal(protocol.Resp[protocol.HeartbeatResp]{Data: protocol.HeartbeatResp{
		CancelTasks: protocol.CancelTasks{CancelTaskIds: []uint64{7}},
		Cluster:     protocol.ClusterHint{Owner: "http://x:7700"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bs), `"cancel_task_ids":[7]`) {
		t.Fatalf("响应形状变了，老 SDK 会读不到停止指令: %s", bs)
	}
	var old protocol.Resp[protocol.CancelTasks]
	if err := v2.Unmarshal(bs, &old); err != nil {
		t.Fatal(err)
	}
	if len(old.Data.CancelTaskIds) != 1 || old.Data.CancelTaskIds[0] != 7 {
		t.Fatalf("解成 CancelTasks = %+v, want [7]", old.Data.CancelTaskIds)
	}
}

// 故障转移只按"这次失败的那台"往后挪。心跳把首选挪到活的那台之后，
// 一笔挂在旧地址上、后知后觉才超时的拉取不能又把地址挪回去（实测过这个来回弹）。
func TestRotateFromDoesNotPingPong(t *testing.T) {
	c := newClient([]string{"http://a:7700", "http://b:7700"})
	if got := c.rotateFrom("http://a:7700"); got != "http://b:7700" {
		t.Fatalf("a 失败应挪到 b，实得 %q", got)
	}
	if got := c.rotateFrom("http://a:7700"); got != "http://b:7700" {
		t.Fatalf("同一台再失败一次，结果必须还是 b（来回弹了），实得 %q", got)
	}
	if got := c.rotateFrom("http://b:7700"); got != "http://a:7700" {
		t.Fatalf("换 b 也失败了才该挪到 a，实得 %q", got)
	}
	// 失败者已经不在已知集合里：不该动当前地址
	c.jumpTo("http://b:7700")
	if got := c.rotateFrom("http://gone:7700"); got != "http://b:7700" {
		t.Fatalf("未知地址失败不该改动 current，实得 %q", got)
	}
}

// 单地址与地址列表可以混用，去重、去尾斜杠后的顺序就是启动时的首选顺序。
func TestNormalizeServers(t *testing.T) {
	got := normalizeServers("http://a:7700/", []string{" http://b:7700", "", "http://a:7700"})
	want := fmt.Sprint([]string{"http://a:7700", "http://b:7700"})
	if fmt.Sprint(got) != want {
		t.Fatalf("normalizeServers = %v, want %v", got, want)
	}
}

// 换副本的判据。被 SIGSTOP 冻结的副本只会表现为"请求发出去、到点超时"，
// 它必须算"这台没答上话"，否则 worker 会一直挂在僵尸副本上追不到新 owner。
func TestReplicaUnreachableClassifier(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"连接被拒", errors.New("dial tcp: connection refused"), true},
		{"对端不收口（超时）", context.DeadlineExceeded, true},
		{"本地主动取消（正在退出）", context.Canceled, false},
		{"父 ctx 取消包在 http 错误里", fmt.Errorf("Post: %w", &url.Error{Op: "Post", URL: "x", Err: context.Canceled}), false},
		{"5xx", &httpStatusError{Status: 503, Body: "db busy"}, true},
		{"4xx 是明确答复", &httpStatusError{Status: 404, Body: "no such path"}, false},
	}
	for _, tc := range cases {
		if got := replicaUnreachable(tc.err); got != tc.want {
			t.Errorf("%s: replicaUnreachable = %v, want %v", tc.name, got, tc.want)
		}
	}
}
