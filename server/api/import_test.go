package api

import (
	"strings"
	"testing"

	"gojob/internal/protocol"
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
	job, err := (&jobConverter{appId: "app1"}).convert(&protocol.PowerJobJob{
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
