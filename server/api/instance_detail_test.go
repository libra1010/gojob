package api

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"gojob/internal/protocol"
	"gojob/server/store"
)

// customQuery 是控制台里用户能自由输入、又被直接拿去查库的唯一入口。
// 后端不拼 SQL 只认白名单，所以这张表既是功能用例也是注入用例。
func TestParseTaskQuery(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantSQL string // 拼接后的条件，用 | 分隔
		wantArg string // 每个条件的首个参数，用 | 分隔
		order   string
		wantErr bool
	}{
		{"空=不过滤", "", "", "", "id asc", false},
		// 排序列必须是**真实列名**：控制台沿用兼容协议的默认那句，用的是响应字段名
		// （last_modified_time），库里叫 modify_time。这一条以前按输入名断言，单测全绿而
		// 实机一开详情弹窗就报 no such column——排序列的映射只有真查库才暴露，所以
		// TestDetailPlusReturnsTree 里也照样查一次。
		{"兼容协议的默认那句（排序列映射到真实列）", "status in (5, 6) order by last_modified_time desc",
			"status IN ?", "[FAILED SUCCEED]", "modify_time desc", false},
		{"created_time 映射到 create_time", "order by created_time desc", "", "", "create_time desc", false},
		{"单个等值", "status = 6", "status = ?", "SUCCEED", "id asc", false},
		{"序号 1 映射到等待派发", "status = 1", "status = ?", "WAITING", "id asc", false},
		{"未知序号原样比较（命中 0 行而不是猜）", "status = 42", "status = ?", "42", "id asc", false},
		{"and 串两个条件", "status = 5 and task_name = 'SPLIT'",
			"status = ?|task_name = ?", "FAILED|SPLIT", "id asc", false},
		{"带 where 前缀也认（用户会连着抄）", "where status = 5", "status = ?", "FAILED", "id asc", false},
		{"带分号结尾", "status = 5;", "status = ?", "FAILED", "id asc", false},
		{"task_name in", "task_name in ('A', 'B')", "task_name IN ?", "[A B]", "id asc", false},
		{"parent_task_id", "parent_task_id = 7", "parent_task_id = ?", "7", "id asc", false},
		{"limit 尾巴被忽略", "status = 5 limit 10", "status = ?", "FAILED", "id asc", false},
		{"注入：分号后接 DROP", "status = 5; drop table instance_tasks", "", "", "", true},
		{"注入：UNION 拖库", "task_name = 'x' union select password from app_infos", "", "", "", true},
		{"注入：注释符", "status = 6 -- ", "", "", "", true},
		{"注入：布尔永真", "status = 6 or 1=1", "", "", "", true},
		{"不支持的列", "result like '%boom%'", "", "", "", true},
		{"不支持的排序列", "order by password desc", "", "", "", true},
		{"status in 里放字符串", "status in ('a')", "", "", "", true},
		{"task_name in 少引号", "task_name in (A)", "", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, args, order, err := parseTaskQuery(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("应拒绝，却解析成 where=%v order=%s", where, order)
				}
				// 拒绝时必须把支持的写法告诉用户，否则这个输入框等于黑盒
				if !strings.Contains(err.Error(), "status =") {
					t.Fatalf("错误信息没给出可用语法：%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse %q: %v", tc.in, err)
			}
			if order != tc.order {
				t.Fatalf("order = %q, want %q", order, tc.order)
			}
			gotSQL := strings.Join(where, "|")
			if gotSQL != tc.wantSQL {
				t.Fatalf("where = %q, want %q", gotSQL, tc.wantSQL)
			}
			if tc.wantArg == "" && len(where) == 0 {
				return
			}
			if got := formatArgs(args); got != tc.wantArg {
				t.Fatalf("args = %q, want %q", got, tc.wantArg)
			}
		})
	}
}

func formatArgs(args []any) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		if s, ok := a.(string); ok {
			parts = append(parts, s)
			continue
		}
		parts = append(parts, fmt.Sprintf("%v", a)) // 切片（IN 的参数列表）按 [a b] 比
	}
	return strings.Join(parts, "|")
}

// detailPlus 要给出实例行、任务统计与一页子任务；查询写歪时不白屏而是带回原因。
func TestDetailPlusReturnsTree(t *testing.T) {
	e := newScopeEnv(t)
	tk := e.addUser(t, "boss", "SUPER_ADMIN", "")
	now := store.Now()
	inst := store.JobInstance{JobId: 1, AppId: "app1", JobName: "mr", Status: protocol.InstanceRunning,
		ExecuteType: protocol.ExecuteMapReduce, RunningTimes: 1, TotalTaskNum: 3,
		CreateTime: now, ModifyTime: now}
	if err := e.s.st.DB.Create(&inst).Error; err != nil {
		t.Fatal(err)
	}
	rows := []store.InstanceTask{
		{InstanceId: inst.ID, JobId: 1, AppId: "app1", Processor: "mr",
			ExecuteType: protocol.ExecuteMapReduce, TaskKey: protocol.RootTaskKey,
			TaskName: protocol.RootTaskName, Status: protocol.TaskSucceed, Result: "已切分 2 片",
			Attempts: 1, MaxAttempts: 1, DispatchTime: now, CreateTime: now, ModifyTime: now},
		{InstanceId: inst.ID, JobId: 1, AppId: "app1", Processor: "mr",
			ExecuteType: protocol.ExecuteMapReduce, ParentTaskId: 1, MapBatch: 1, MapAttempt: 1,
			TaskKey: "0.1", TaskName: "SPLIT", SubTaskContent: `{"i":1}`, WorkerId: "w1",
			Status: protocol.TaskSucceed, Result: "ok-1",
			Attempts: 1, MaxAttempts: 1, DispatchTime: now, CreateTime: now, ModifyTime: now},
		{InstanceId: inst.ID, JobId: 1, AppId: "app1", Processor: "mr",
			ExecuteType: protocol.ExecuteMapReduce, ParentTaskId: 1, MapBatch: 1, MapAttempt: 1,
			TaskKey: "0.2", TaskName: "SPLIT", SubTaskContent: `{"i":2}`,
			Status: protocol.TaskWaiting, Attempts: 1, MaxAttempts: 1,
			DispatchTime: now, CreateTime: now, ModifyTime: now},
	}
	if err := e.s.st.DB.CreateInBatches(rows, 10).Error; err != nil {
		t.Fatal(err)
	}

	r := e.post(t, "/openapi/instance/detailPlus", tk, body(map[string]any{
		"instance_id": inst.ID, "app_id": "app1", "custom_query": ""}))
	if r.Code != 0 {
		t.Fatalf("detailPlus code=%d msg=%s", r.Code, r.Message)
	}
	data := r.Data
	if d, _ := data["instance"].(map[string]any); d["execute_type"] != protocol.ExecuteMapReduce {
		t.Fatalf("实例行缺失或执行类型不对：%v", data["instance"])
	}
	stats, _ := data["task_stats"].(map[string]any)
	if stats["total"].(float64) != 3 || stats["succeed"].(float64) != 2 || stats["waiting"].(float64) != 1 {
		t.Fatalf("Task 统计 = %v, want total 3 / succeed 2 / waiting 1", stats)
	}
	tasks, _ := data["tasks"].([]any)
	if len(tasks) != 3 {
		t.Fatalf("返回 %d 行子任务, want 3", len(tasks))
	}
	first, _ := tasks[1].(map[string]any)
	if first["task_id"] != "0.1" || first["task_content"] != `{"i":1}` || first["status_str"] != "成功" {
		t.Fatalf("子任务行字段不完整：%v", first)
	}

	// 过滤：只看失败的（兼容协议序号 5）
	r = e.post(t, "/openapi/instance/detailPlus", tk, body(map[string]any{
		"instance_id": inst.ID, "custom_query": "status = 5"}))
	if r.Code != 0 {
		t.Fatalf("过滤查询 code=%d", r.Code)
	}
	if got := r.Data["tasks"]; len(got.([]any)) != 0 {
		t.Fatalf("没有失败任务却返回了行：%v", got)
	}
	if r.Data["total"].(float64) != 0 {
		t.Fatalf("total = %v, want 0", r.Data["total"])
	}

	// 控制台默认填的那句原样打进来必须能查：兼容协议的详情弹窗一打开就是这句话，
	// 排序列名只要对不上真实列，用户看到的就是一个报错的空表格（实机验收踩过一次）。
	r = e.post(t, "/openapi/instance/detailPlus", tk, body(map[string]any{
		"instance_id":  inst.ID,
		"custom_query": "status in (5, 6) order by last_modified_time desc"}))
	if r.Code != 0 {
		t.Fatalf("默认查询 code=%d msg=%s", r.Code, r.Message)
	}
	if qe, _ := r.Data["query_error"].(string); qe != "" {
		t.Fatalf("默认查询被服务端拒绝：%q", qe)
	}
	tasks, _ = r.Data["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("默认查询返回 %d 行，want 2（两个已终态的子任务）：%v", len(tasks), tasks)
	}
	// 三行的 modify_time 相同，排序结果不稳定，这里只核对命中了哪两行
	ids := make([]string, 0, len(tasks))
	for _, x := range tasks {
		ids = append(ids, x.(map[string]any)["task_id"].(string))
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "0,0.1" {
		t.Fatalf("默认查询命中行 = %v, want 0 与 0.1（WAITING 那行不该出现）", ids)
	}

	// 写歪的查询不能让弹窗白屏：照常返回实例信息 + query_error
	r = e.post(t, "/openapi/instance/detailPlus", tk, body(map[string]any{
		"instance_id": inst.ID, "custom_query": "1=1"}))
	if r.Code != 0 {
		t.Fatalf("被拒绝的查询不该让整个请求失败，code=%d msg=%s", r.Code, r.Message)
	}
	if qe, _ := r.Data["query_error"].(string); !strings.Contains(qe, "支持的写法") {
		t.Fatalf("query_error 没说明支持什么：%q", qe)
	}
	if r.Data["instance"] == nil {
		t.Fatal("查询失败时实例信息也应照常返回")
	}
}
