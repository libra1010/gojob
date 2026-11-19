package api

import (
	"strings"
	"testing"

	"github.com/libra1010/gojob/internal/protocol"
)

func sp(s string) *string { return &s }

func TestNormalizeProcessorType(t *testing.T) {
	cases := []struct {
		name             string
		ptype, info      string
		jobParams, instP string
		wantType         string
		wantInfo         string
		wantJobParams    string
		wantErr          string
	}{
		{
			name: "内建按原样通过", ptype: "BUILT_IN", info: "com.x.MyProcessor",
			wantType: "BUILT_IN", wantInfo: "com.x.MyProcessor",
		},
		{
			name: "内建缺类名", ptype: "BUILT_IN",
			wantErr: "processorInfo",
		},
		{
			name: "旧的 SPRING_CONTAINER 归一为 BUILT_IN", ptype: "SPRING_CONTAINER", info: "myBean",
			wantType: "BUILT_IN", wantInfo: "myBean",
		},
		{
			name: "SHELL 改写成官方脚本处理器", ptype: "SHELL", info: "echo hi",
			wantType: "BUILT_IN", wantInfo: protocol.OfficialShellProcessor, wantJobParams: "echo hi",
		},
		{
			name: "SHELL 与已有任务参数冲突", ptype: "SHELL", info: "echo hi", jobParams: "已有参数",
			wantErr: "jobParams",
		},
		{
			name: "SHELL 与实例参数冲突（脚本处理器实例参数优先）", ptype: "SHELL", info: "echo hi", instP: "每次传的脚本",
			wantErr: "instanceParams",
		},
		{
			name: "SHELL 没有脚本文本", ptype: "SHELL", info: "  ",
			wantErr: "processorInfo",
		},
		{name: "PYTHON 明确拒绝", ptype: "PYTHON", info: "print 1", wantErr: "processorType"},
		{name: "EXTERNAL 是 JAR 容器，明确拒绝", ptype: "EXTERNAL", info: "1#com.x.My", wantErr: "processorType"},
		{name: "未知值拒绝", ptype: "MYSTERY", info: "x", wantErr: "processorType"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotType, gotInfo, gotParams, err := normalizeProcessorType(c.ptype, c.info, c.jobParams, c.instP)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("应报 %s，实际 err=%v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if gotType != c.wantType || gotInfo != c.wantInfo || gotParams != c.wantJobParams {
				t.Fatalf("得到 (%q,%q,%q)，期望 (%q,%q,%q)",
					gotType, gotInfo, gotParams, c.wantType, c.wantInfo, c.wantJobParams)
			}
		})
	}
}

// 导入的 SHELL 任务落地后必须变成"能跑的那份配置"，而不是静默存成跑不了的类名。
func TestConvertLegacyShellJob(t *testing.T) {
	job, err := (&jobConverter{appId: "app1"}).convert(&protocol.JobDTO{
		JobName:            sp("legacy-shell"),
		TimeExpressionType: sp("API"),
		ProcessorType:      sp("SHELL"),
		ProcessorInfo:      sp("echo hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.ProcessorType != "BUILT_IN" {
		t.Fatalf("processorType=%s", job.ProcessorType)
	}
	if job.Processor != protocol.OfficialShellProcessor {
		t.Fatalf("processor=%q", job.Processor)
	}
	if job.JobParams != "echo hello" {
		t.Fatalf("jobParams=%q，脚本文本没搬过来", job.JobParams)
	}
}

// 回归：审计中间件曾按 4KiB 截断请求体再交还下游，任何超过 4KiB 的写请求
// （任务包导入动辄几十 KB）都会被 bind 判成非法 JSON，只剩 "bad request" 可报。
func TestImportBodyLargerThanFourKiB(t *testing.T) {
	e := newScopeEnv(t)
	tk := e.addUser(t, "root", "SUPER_ADMIN", "*")
	big := strings.Repeat("x", 8<<10)
	body := `{"app_id":"app1","jobs":[{"jobName":"big","timeExpressionType":"CRON",` +
		`"timeExpression":"0 0 * * *","processorType":"BUILT_IN","processorInfo":"p.Big",` +
		`"jobParams":"` + big + `"}]}`
	r := e.post(t, "/openapi/job/import", tk, body)
	if r.Code != protocol.CodeOK {
		t.Fatalf("大包导入被拒：code=%d msg=%q", r.Code, r.Message)
	}
	items, _ := r.Data["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("导入结果条数=%d，want 1：%+v", len(items), r.Data)
	}
	item, _ := items[0].(map[string]any)
	if ok, _ := item["success"].(bool); !ok {
		t.Fatalf("大包导入失败：%v", item["reason"])
	}
}
