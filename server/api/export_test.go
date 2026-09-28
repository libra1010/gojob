package api

// 批量导出的行为与授权边界。读侧的 GET 不进 authorize 中间件，所以"整项目一次拉走"这种
// 放大过的读必须在处理器里自己判项目授权。

import (
	"testing"

	"gojob/internal/protocol"
)

func exportURL(qs string) string { return "/openapi/job/export?" + qs }

func pstr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func codeOf(t *testing.T, e *scopeEnv, path, token string) int {
	t.Helper()
	return e.get(t, path, token).Code
}

func TestExportBatchShapes(t *testing.T) {
	e := newScopeEnv(t)
	tk := e.addUser(t, "root", "SUPER_ADMIN", "*")
	env := &workerEnv{s: e.s, r: e.r}
	a := e.addJob(t, "app1", "a")
	b := e.addJob(t, "app1", "b")

	// 单条仍是对象（既有调用方与导入框靠这个形状）
	one := getAs[protocol.JobDTO](t, env, exportURL("job_id="+uitoa(a)), tk)
	if one.Code != protocol.CodeOK || pstr(one.Data.JobName) != "a" {
		t.Fatalf("单任务导出异常：code=%d data=%+v", one.Code, one.Data)
	}

	// 多选：数组，顺序按传入的 id
	multi := getAs[[]protocol.JobDTO](t, env, exportURL("job_ids="+uitoa(b)+","+uitoa(a)), tk)
	if multi.Code != protocol.CodeOK {
		t.Fatalf("批量导出 code=%d msg=%q", multi.Code, multi.Message)
	}
	if len(multi.Data) != 2 {
		t.Fatalf("批量导出条数 = %d, want 2", len(multi.Data))
	}
	if pstr(multi.Data[0].JobName) != "b" || pstr(multi.Data[1].JobName) != "a" {
		t.Errorf("批量导出顺序错：%s, %s", pstr(multi.Data[0].JobName), pstr(multi.Data[1].JobName))
	}

	// 整项目：app1 的三条都要在，app2 的一条都不能混进来
	e.addJob(t, "app1", "c")
	e.addJob(t, "app2", "other")
	all := getAs[[]protocol.JobDTO](t, env, exportURL("app_id=app1"), tk)
	if all.Code != protocol.CodeOK || len(all.Data) != 3 {
		t.Fatalf("整项目导出 code=%d 条数=%d, want 3", all.Code, len(all.Data))
	}
	for _, j := range all.Data {
		if pstr(j.JobName) == "other" {
			t.Errorf("跨项目泄漏：导出了 app2 的任务")
		}
	}

	// 三种选法都没给
	if got := codeOf(t, e, exportURL(""), tk); got != protocol.CodeBadReq {
		t.Errorf("缺参数应 400，实得 code=%d", got)
	}
}

func TestExportRespectsAppScope(t *testing.T) {
	e := newScopeEnv(t)
	jobIn1 := e.addJob(t, "app1", "in-scope")
	jobIn2 := e.addJob(t, "app2", "out-of-scope")
	scoped := e.addUser(t, "ops", "ADMIN", "app1")

	// 授权项目内：可以导
	ok := getAs[[]protocol.JobDTO](t, &workerEnv{s: e.s, r: e.r},
		exportURL("job_ids="+uitoa(jobIn1)), scoped)
	if ok.Code != protocol.CodeOK || len(ok.Data) != 1 {
		t.Fatalf("授权项目内的导出失败：code=%d msg=%q", ok.Code, ok.Message)
	}

	// 整项目指向未授权项目：拒
	if got := codeOf(t, e, exportURL("app_id=app2"), scoped); got != protocol.CodeAuthErr {
		t.Errorf("未授权项目整项目导出应被拒，实得 code=%d", got)
	}
	// 未授权项目的单个 id 混在批量里：整次请求都拒，不能"能给的都给"
	mixed := exportURL("job_ids=" + uitoa(jobIn1) + "," + uitoa(jobIn2))
	if got := codeOf(t, e, mixed, scoped); got != protocol.CodeAuthErr {
		t.Errorf("批量里混入未授权任务应整次拒绝，实得 code=%d", got)
	}
}
