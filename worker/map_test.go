// SDK 侧的多任务接口：map 派生、子任务负载解码、reduce 分派与结果分页取满。
package worker

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
)

type mrFake struct {
	mu      sync.Mutex
	maps    []protocol.MapTaskReq
	reports []protocol.ReportTaskReq
	results int // /worker/taskResults 被调了几次
	pages   []protocol.TaskResultsResp
	failMap bool
}

func (f *mrFake) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t0(w, r)
			return
		}
		write := func(data any) {
			bs, _ := v2.Marshal(protocol.Resp[any]{Code: protocol.CodeOK, Data: data})
			w.Header().Set("Content-Type", "application/json")
			w.Write(bs)
		}
		switch r.URL.Path {
		case "/worker/register":
			write(protocol.RegisterResp{Token: "tk-1", TtlSeconds: 600})
		case "/worker/mapTask":
			var req protocol.MapTaskReq
			bs, _ := v2.Marshal(body)
			_ = v2.Unmarshal(bs, &req)
			f.mu.Lock()
			f.maps = append(f.maps, req)
			f.mu.Unlock()
			if f.failMap {
				bs, _ := v2.Marshal(protocol.Resp[any]{Code: protocol.CodeErr, Message: "server said no"})
				w.Write(bs)
				return
			}
			keys := make([]string, 0, len(req.SubTasks))
			for i := range req.SubTasks {
				keys = append(keys, "0."+string(rune('a'+i)))
			}
			write(protocol.MapTaskResp{TaskKeys: keys, Created: len(keys)})
		case "/worker/taskResults":
			var req protocol.TaskResultsReq
			bs, _ := v2.Marshal(body)
			_ = v2.Unmarshal(bs, &req)
			f.mu.Lock()
			idx := f.results
			f.results++
			f.mu.Unlock()
			if idx >= len(f.pages) {
				idx = len(f.pages) - 1
			}
			write(f.pages[idx])
		case "/worker/reportLogs":
			// 真服务端回的是"收下列数"（int）。默认分支那个 {} 会让 SDK 判成
			// "cannot unmarshal JSON object into Go int within /data" 而无限重试，
			// 把用例拖到 flush 的 30s 超时上。
			write(1)
		case "/worker/report":
			var req protocol.ReportTaskReq
			bs, _ := v2.Marshal(body)
			_ = v2.Unmarshal(bs, &req)
			f.mu.Lock()
			f.reports = append(f.reports, req)
			f.mu.Unlock()
			write(struct{}{})
		default:
			write(struct{}{})
		}
	})
}

func t0(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "bad json", http.StatusBadRequest)
}

// newMRWorker 起一个只连本地假服务端的 worker，并完成注册（runProcessor 需要有效 token）。
func newMRWorker(t *testing.T, f *mrFake) *Worker {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	w := New(Config{ServerAddress: srv.URL, AppId: "app1", Password: "pw", Address: "w1", MaxConcurrency: 2})
	if err := w.register(t.Context()); err != nil {
		t.Fatalf("register: %v", err)
	}
	return w
}

func taskOf(execType, key, name, content string) protocol.TaskDTO {
	return protocol.TaskDTO{TaskId: 11, JobId: 1, InstanceId: 21, AppId: "app1",
		Processor: "p", ProcessorType: protocol.ProcessorBuiltIn,
		ExecuteType: execType, TaskKey: key, TaskName: name, SubTaskContent: content,
		Attempts: 1, LogLevel: "INFO"}
}

func registerProc(t *testing.T, name string, p Processor) {
	t.Helper()
	// Register 对重名 panic，测试里每个用例用独立名字最省事
	Register(name, p)
	t.Cleanup(func() {
		procMu.Lock()
		delete(procs, name)
		procMu.Unlock()
	})
}

type funcProc struct{ fn func(*TaskContext) error }

func (f funcProc) Process(ctx *TaskContext) error { return f.fn(ctx) }

type subTask struct {
	File string `json:"file"`
	N    int    `json:"n"`
}

// 根任务里 map 出子任务：负载必须是 JSON、批次号从 1 起、返回值里带服务端分配的任务号。
func TestProcessorMapRoundTrip(t *testing.T) {
	f := &mrFake{}
	w := newMRWorker(t, f)
	var gotKeys []string
	p := funcProc{func(ctx *TaskContext) error {
		if !ctx.IsRootTask() {
			return errors.New("根任务应被判定为 IsRootTask")
		}
		keys, err := ctx.Map([]any{subTask{File: "a"}, subTask{File: "b", N: 2}}, "SPLIT")
		if err != nil {
			return err
		}
		gotKeys = keys
		ctx.SetResult("已派生 2 片")
		return nil
	}}
	registerProc(t, "p", p)

	res, err := w.runProcessor(t.Context(), taskOf(protocol.ExecuteMap, "0", protocol.RootTaskName, ""), nil)
	if err != nil {
		t.Fatalf("runProcessor: %v", err)
	}
	if res != "已派生 2 片" {
		t.Fatalf("结果 = %q, want SetResult 的内容", res)
	}
	if len(f.maps) != 1 {
		t.Fatalf("mapTask 被调用 %d 次, want 1", len(f.maps))
	}
	req := f.maps[0]
	if req.TaskName != "SPLIT" || req.BatchNo != 1 || req.TaskId != 11 || req.AppId != "app1" {
		t.Fatalf("map 请求字段不对：%+v", req)
	}
	if req.InstanceId != w.instanceID {
		t.Fatalf("map 请求的 instance_id = %q, want worker 自己的 %q", req.InstanceId, w.instanceID)
	}
	var back subTask
	if err := json.Unmarshal([]byte(req.SubTasks[1]), &back); err != nil || back.File != "b" || back.N != 2 {
		t.Fatalf("子任务负载没按 JSON 传：%q (%v)", req.SubTasks[1], err)
	}
	if len(gotKeys) != 2 {
		t.Fatalf("Map 返回 %v, want 2 个任务号", gotKeys)
	}
	// token 必须随请求带上：数据面不再接受项目口令
	if req.Token != "tk-1" {
		t.Fatalf("map 请求没带 token：%q", req.Token)
	}
}

// 同一任务里连续两次 map 要用不同批次号，否则服务端会把第二批当成第一批的重放。
func TestMapBatchNumberIncrements(t *testing.T) {
	f := &mrFake{}
	w := newMRWorker(t, f)
	p := funcProc{func(ctx *TaskContext) error {
		for _, n := range [][]any{{subTask{File: "a"}}, {subTask{File: "b"}}} {
			if _, err := ctx.Map(n, "SPLIT"); err != nil {
				return err
			}
		}
		return nil
	}}
	registerProc(t, "p", p)
	if _, err := w.runProcessor(t.Context(), taskOf(protocol.ExecuteMap, "0", protocol.RootTaskName, ""), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.maps) != 2 || f.maps[0].BatchNo != 1 || f.maps[1].BatchNo != 2 {
		t.Fatalf("批次号 = %v, want 1、2", []int{f.maps[0].BatchNo, f.maps[1].BatchNo})
	}
}

// STANDALONE 里调 map 是配置写错，必须报错而不是静默忽略（兼容协议同语义）。
func TestMapRejectedOutsideMapTypes(t *testing.T) {
	f := &mrFake{}
	w := newMRWorker(t, f)
	for i, execType := range []string{protocol.ExecuteStandalone, protocol.ExecuteBroadcast} {
		name := "p" + string(rune('0'+i))
		p := funcProc{func(ctx *TaskContext) error {
			_, err := ctx.Map([]any{1}, "SPLIT")
			if err == nil || !strings.Contains(err.Error(), execType) {
				t.Fatalf("%s 里 map 应报错并点名执行类型，实际 %v", execType, err)
			}
			return nil
		}}
		registerProc(t, name, p)
		dto := taskOf(execType, "0", protocol.RootTaskName, "")
		dto.Processor = name
		if _, err := w.runProcessor(t.Context(), dto, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.maps) != 0 {
		t.Fatalf("被拒绝的 map 仍然发出了请求：%+v", f.maps)
	}
}

func TestMapRejectsReservedNamesAndEmptyList(t *testing.T) {
	f := &mrFake{}
	w := newMRWorker(t, f)
	p := funcProc{func(ctx *TaskContext) error {
		if _, err := ctx.Map([]any{1}, protocol.LastTaskName); err == nil {
			t.Fatal("保留名应被拒绝")
		}
		if _, err := ctx.Map([]any{}, "X"); err == nil {
			t.Fatal("空列表应被拒绝")
		}
		return nil
	}}
	registerProc(t, "p", p)
	if _, err := w.runProcessor(t.Context(), taskOf(protocol.ExecuteMap, "0", protocol.RootTaskName, ""), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.maps) != 0 {
		t.Fatalf("非法 map 不该发出请求：%+v", f.maps)
	}
}

// 服务端拒绝时错误要原样冒给处理器，别让它以为已经派生成功。
func TestMapSurfacesServerError(t *testing.T) {
	f := &mrFake{failMap: true}
	w := newMRWorker(t, f)
	p := funcProc{func(ctx *TaskContext) error {
		_, err := ctx.Map([]any{1}, "SPLIT")
		if err == nil || !strings.Contains(err.Error(), "server said no") {
			t.Fatalf("服务端原因没传出来：%v", err)
		}
		return nil
	}}
	registerProc(t, "p", p)
	if _, err := w.runProcessor(t.Context(), taskOf(protocol.ExecuteMap, "0", protocol.RootTaskName, ""), nil); err != nil {
		t.Fatal(err)
	}
}

// 子任务负载解码：根任务没有负载，要给明确错误而不是 panic。
func TestSubTaskDecode(t *testing.T) {
	f := &mrFake{}
	w := newMRWorker(t, f)
	p := funcProc{func(ctx *TaskContext) error {
		var s subTask
		if err := ctx.SubTask(&s); err != nil {
			return err
		}
		if s.File != "report.csv" || s.N != 3 {
			t.Fatalf("解码结果 %+v", s)
		}
		if ctx.IsRootTask() {
			t.Fatal("子任务不该被判定为根任务")
		}
		ctx.SetResult("处理完 " + s.File)
		return nil
	}}
	registerProc(t, "p", p)
	res, err := w.runProcessor(t.Context(),
		taskOf(protocol.ExecuteMap, "0.7", "SPLIT", `{"file":"report.csv","n":3}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res != "处理完 report.csv" {
		t.Fatalf("结果 = %q", res)
	}

	rootErr := funcProc{func(ctx *TaskContext) error {
		var s subTask
		if err := ctx.SubTask(&s); err == nil || !strings.Contains(err.Error(), "没有子任务负载") {
			t.Fatalf("根任务 SubTask 应明确报错，实际 %v", err)
		}
		return nil
	}}
	registerProc(t, "p2", rootErr)
	dto := taskOf(protocol.ExecuteMap, "0", protocol.RootTaskName, "")
	dto.Processor = "p2"
	if _, err := w.runProcessor(t.Context(), dto, nil); err != nil {
		t.Fatal(err)
	}
}

type mrProc struct {
	seen []TaskResult
	err  error
}

func (m *mrProc) Process(ctx *TaskContext) error { return nil }

func (m *mrProc) Reduce(ctx *TaskContext, results []TaskResult) error {
	m.seen = results
	ctx.SetResult("汇总 " + string(rune('0'+len(results))) + " 片")
	return m.err
}

// reduce 任务分派给 Reducer，并且必须把分页结果取满才交给业务。
func TestReduceDispatchPagesResults(t *testing.T) {
	f := &mrFake{pages: []protocol.TaskResultsResp{
		{Results: []protocol.TaskResult{{TaskKey: "0.1", TaskName: "S", Success: true, Result: "a"},
			{TaskKey: "0.2", TaskName: "S", Success: false, Result: "boom"}},
			Total: 3, HasMore: true},
		{Results: []protocol.TaskResult{{TaskKey: "0.3", TaskName: "S", Success: true, Result: "c"}},
			Total: 3, HasMore: false},
	}}
	w := newMRWorker(t, f)
	p := &mrProc{}
	registerProc(t, "p", p)

	res, err := w.runProcessor(t.Context(),
		taskOf(protocol.ExecuteMapReduce, protocol.LastTaskKey, protocol.LastTaskName, ""), nil)
	if err != nil {
		t.Fatalf("reduce 分派失败: %v", err)
	}
	if len(p.seen) != 3 {
		t.Fatalf("Reduce 只看到 %d 条结果, want 3（分页没取满）", len(p.seen))
	}
	if p.seen[1].Success || p.seen[1].Result != "boom" {
		t.Fatalf("子任务成败/结果丢失：%+v", p.seen[1])
	}
	if f.results != 2 {
		t.Fatalf("taskResults 调了 %d 次, want 2 页", f.results)
	}
	if res != "汇总 3 片" {
		t.Fatalf("实例结果应取 Reduce 的 SetResult，实际 %q", res)
	}
	// Reduce 返回 error ⇒ 任务失败，错误原因就是结果文本
	p2 := &mrProc{err: errors.New("汇总不出来")}
	registerProc(t, "p2", p2)
	dto := taskOf(protocol.ExecuteMapReduce, protocol.LastTaskKey, protocol.LastTaskName, "")
	dto.Processor = "p2"
	if _, err := w.runProcessor(t.Context(), dto, nil); err == nil || err.Error() != "汇总不出来" {
		t.Fatalf("Reduce 的错误要原样上报，实际 %v", err)
	}
}

// 没实现 Reducer 却把任务配成 MAP_REDUCE：明确报错，不能静默当普通任务跑掉。
func TestReduceTaskWithoutReducerFails(t *testing.T) {
	f := &mrFake{}
	w := newMRWorker(t, f)
	registerProc(t, "p", funcProc{func(ctx *TaskContext) error { return nil }})
	_, err := w.runProcessor(t.Context(),
		taskOf(protocol.ExecuteMapReduce, protocol.LastTaskKey, protocol.LastTaskName, ""), nil)
	if err == nil || !strings.Contains(err.Error(), "Reducer") {
		t.Fatalf("应提示未实现 Reducer，实际 %v", err)
	}
}

// 执行处理器时不碰日志通道也不能 panic（gojob-agent 之外还有人在管道外调用处理器）。
func TestProcessorWithoutLogger(t *testing.T) {
	f := &mrFake{}
	w := newMRWorker(t, f)
	registerProc(t, "p", funcProc{func(ctx *TaskContext) error {
		ctx.Infof("带参数的日志 %d", 1)
		ctx.Errorf("错误日志")
		return nil
	}})
	if _, err := w.runProcessor(t.Context(), taskOf(protocol.ExecuteStandalone, "0", protocol.RootTaskName, ""), nil); err != nil {
		t.Fatalf("logger 为 nil 时不该 panic：%v", err)
	}
}
