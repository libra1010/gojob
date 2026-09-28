// 节点监控接口的校验：多副本聚合、判活与陈旧行清理、worker runtime 指标透传。
package api

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/server/node"
	"gojob/server/store"
)

func getAs[T any](t *testing.T, e *workerEnv, path, token string) protocol.Resp[T] {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	if token != "" {
		r.Header.Set("X-GoJob-Token", token)
	}
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, r)
	var resp protocol.Resp[T]
	if err := v2.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: bad response %q: %v", path, w.Body.String(), err)
	}
	return resp
}

func (e *workerEnv) seedAdmin(t *testing.T) string {
	t.Helper()
	now := store.Now()
	if err := e.s.st.DB.Create(&store.UserInfo{Username: "root", Password: "x",
		Nickname: "监控", Role: "SUPER_ADMIN", Status: "ACTIVE",
		CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
	tk, err := e.s.am.Issue("root")
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func seedNode(st *store.Store, id string, silent time.Duration, samples string) {
	now := store.Now()
	ts := now.Add(-silent)
	if err := st.DB.Create(&store.ServerNode{NodeId: id, Hostname: "h-" + id, Address: ":7788",
		BootTime: ts.Add(-time.Hour), SampleTime: ts, CPUPercent: 1.5, HeapInuseMb: 8,
		GcCount: 7, GcPauseMs: 3.2, ConnsActive: 2, ConnsIdle: 3, Samples: samples,
		CreateTime: ts, ModifyTime: ts}).Error; err != nil {
		panic(err)
	}
}

func TestSystemNodesAggregatesReplicas(t *testing.T) {
	e := newWorkerEnv(t)
	tk := e.seedAdmin(t)
	good, _ := v2.Marshal(node.Series{Time: []int64{1, 2}, CPU: []float64{1, 2}, Mem: []float64{8, 8},
		Gc: []uint32{7, 8}, Conns: []int{5, 5}, QPS: []float64{0, 1}, Poll: []int{1, 1}})
	seedNode(e.s.st, "fresh", 2*time.Second, string(good))
	seedNode(e.s.st, "silent", 2*time.Minute, `{"坏数据":true}`)
	seedNode(e.s.st, "ancient", 8*24*time.Hour, "")

	r := getAs[nodesResp](t, e, "/openapi/system/nodes", tk)
	if r.Code != protocol.CodeOK {
		t.Fatalf("code = %d(%q)", r.Code, r.Message)
	}
	if r.Data.IntervalMs != node.ReportInterval.Milliseconds() {
		t.Fatalf("interval_ms = %d, want %d", r.Data.IntervalMs, node.ReportInterval.Milliseconds())
	}
	if len(r.Data.Servers) != 2 {
		t.Fatalf("servers = %d 行, want 2（静默 8 天的那行应被清掉）", len(r.Data.Servers))
	}
	var got [2]bool
	for _, s := range r.Data.Servers {
		switch s.NodeId {
		case "fresh":
			got[0] = true
			if !s.Online || s.Samples == nil || len(s.Samples.CPU) != 2 {
				t.Fatalf("fresh = online=%v samples=%+v", s.Online, s.Samples)
			}
			if s.GcCount != 7 || s.ConnsIdle != 3 || s.HeapInuseMb != 8 {
				t.Fatalf("fresh 指标错乱: %+v", s)
			}
		case "silent":
			got[1] = true
			if s.Online {
				t.Fatal("静默 2min 的副本应判离线")
			}
			if s.Samples != nil {
				t.Fatalf("脏 samples 应解析为 nil，实际 %+v", s.Samples)
			}
			if s.SilentMs < node.OfflineAfter.Milliseconds() {
				t.Fatalf("silent_ms = %d, 应超过判活阈值", s.SilentMs)
			}
		}
	}
	if got != [2]bool{true, true} {
		t.Fatalf("缺副本行: %v", got)
	}
	var left int64
	e.s.st.DB.Model(&store.ServerNode{}).Count(&left)
	if left != 2 {
		t.Fatalf("表内剩 %d 行, want 2（陈旧行须真删）", left)
	}
}

// worker 的 runtime 指标从心跳一路透传到监控页，缺项保持 0 由前端显示"—"。
func TestSystemNodesCarriesWorkerMetrics(t *testing.T) {
	e := newWorkerEnv(t)
	tk := e.seedAdmin(t)
	wtk := e.register(t, "w-1", "app1", "pw1")
	if r := callAs[protocol.CancelTasks](t, e, "/worker/heartbeat", body(map[string]any{
		"instance_id": "w-1", "app_id": "app1", "address": "10.0.0.1:7799", "token": wtk,
		"cpu_percent": 12.5, "gc_count": 34, "gc_pause_ms": 9.8, "threads": 21,
		"open_fds": 64, "running_tasks": 2,
	})); r.Code != protocol.CodeOK {
		t.Fatalf("heartbeat code = %d(%q)", r.Code, r.Message)
	}

	r := getAs[nodesResp](t, e, "/openapi/system/nodes", tk)
	if r.Code != protocol.CodeOK {
		t.Fatalf("code = %d(%q)", r.Code, r.Message)
	}
	if len(r.Data.Workers) != 1 {
		t.Fatalf("workers = %d, want 1", len(r.Data.Workers))
	}
	w := r.Data.Workers[0]
	if !w.Online || w.CpuPercent != 12.5 || w.GcCount != 34 || w.GcPauseMs != 9.8 ||
		w.Threads != 21 || w.OpenFDs != 64 || w.RunningTasks != 2 {
		t.Fatalf("worker 指标丢失或错乱: %+v", w)
	}
	if w.RssMb != 0 {
		t.Fatalf("rss_mb = %v, 平台取不到时应保持 0", w.RssMb)
	}
	if strings.TrimSpace(w.InstanceId) == "" || w.AppId != "app1" {
		t.Fatalf("worker 身份字段丢失: %+v", w)
	}
}

// 监控页能看到进程指标，必须是管理员；未登录直接 401。
func TestSystemNodesRequiresRole(t *testing.T) {
	e := newWorkerEnv(t)
	if r := getAs[nodesResp](t, e, "/openapi/system/nodes", ""); r.Code != protocol.CodeAuthErr {
		t.Fatalf("匿名 code = %d(%q), want 401", r.Code, r.Message)
	}
	e.seedAdmin(t)
	now := store.Now()
	if err := e.s.st.DB.Create(&store.UserInfo{Username: "viewer", Password: "x", Role: "READONLY",
		Status: "ACTIVE", CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
	viewerTk, err := e.s.am.Issue("viewer")
	if err != nil {
		t.Fatal(err)
	}
	if r := getAs[nodesResp](t, e, "/openapi/system/nodes", viewerTk); r.Code != protocol.CodeAuthErr {
		t.Fatalf("只读账号 code = %d(%q), want 403/401", r.Code, r.Message)
	}
}
