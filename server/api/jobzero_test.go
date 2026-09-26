package api

// 显式 0 能不能真的落库。这些字段的 0 都是有意义的配置（不重试 / 不限实例数），
// 而 gorm 的 Create 会跳过"带 default 标签且值为零"的列，把 0 交给库里的 DEFAULT——
// 于是 API 层修好了"0 被当成没传"之后，0 仍然会被 DB 默认值静默换成 3。
// 实机翻车现场：verify-shell-agent.sh 里一个 max_retry_times=0 的任务跑满了 4 次尝试。

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/server/store"
)

type jobCounters struct {
	MaxRetryTimes    int
	InstanceRetryNum int
	InstanceNumLimit int
}

// readCounters 从库里读，不信 Create 之后内存里的那个结构体——那正是这个 bug 藏身的地方。
func readCounters(t *testing.T, e *scopeEnv, jobID uint64) jobCounters {
	t.Helper()
	var row jobCounters
	if err := e.s.st.DB.Model(&store.JobInfo{}).Where("id = ?", jobID).
		Select("max_retry_times", "instance_retry_num", "instance_num_limit").
		Scan(&row).Error; err != nil {
		t.Fatalf("read counters: %v", err)
	}
	return row
}

// createJobViaAPI 走真实 HTTP 入口。job/create 的 data 是裸的任务 id（不是对象），
// 所以这里不复用 scopeEnv.post——它的 data 是按 map 解的。
func createJobViaAPI(t *testing.T, e *scopeEnv, token, body string) uint64 {
	t.Helper()
	r := httptest.NewRequest("POST", "/openapi/job/create", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-GoJob-Token", token)
	w := httptest.NewRecorder()
	e.r.ServeHTTP(w, r)
	var out protocol.Resp[uint64]
	if err := v2.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad response %q: %v", w.Body.String(), err)
	}
	if out.Code != 0 {
		t.Fatalf("job/create 失败 code=%d message=%q", out.Code, out.Message)
	}
	return out.Data
}

func TestCreateJobHonoursExplicitZeros(t *testing.T) {
	e := newScopeEnv(t)
	tok := e.addUser(t, "root", "SUPER_ADMIN", "")

	id := createJobViaAPI(t, e, tok, `{"app_id":"app1","job_name":"zeros","processor":"p.zeros",`+
		`"time_expression_type":"API","max_retry_times":0,"instance_retry_num":0,"instance_num_limit":0}`)
	if got := readCounters(t, e, id); got != (jobCounters{}) {
		t.Errorf("显式传的 0 没落库： %+v，want 全 0", got)
	}
}

// TestCreateJobFillsDefaultsWhenAbsent 反向保险：字段缺席时仍按默认值填，
// 打通"0"这条通道不能顺手把"没配置"也变成 0。
func TestCreateJobFillsDefaultsWhenAbsent(t *testing.T) {
	e := newScopeEnv(t)
	tok := e.addUser(t, "root", "SUPER_ADMIN", "")

	id := createJobViaAPI(t, e, tok, `{"app_id":"app1","job_name":"plain","processor":"p.plain",`+
		`"time_expression_type":"API"}`)
	want := jobCounters{MaxRetryTimes: 3, InstanceRetryNum: 3, InstanceNumLimit: 3}
	if got := readCounters(t, e, id); got != want {
		t.Errorf("缺席字段没拿到默认值： %+v, want %+v", got, want)
	}
}

// TestModifyJobAcceptsZeroRetry 修改路径也要能把重试次数改回 0：兼容协议的
// taskRetryNum=0 很常见，导入进来的任务同样得能表达。
func TestModifyJobAcceptsZeroRetry(t *testing.T) {
	e := newScopeEnv(t)
	tok := e.addUser(t, "root", "SUPER_ADMIN", "")

	id := createJobViaAPI(t, e, tok, `{"app_id":"app1","job_name":"down","processor":"p.down",`+
		`"time_expression_type":"API","max_retry_times":5,"instance_retry_num":5,"instance_num_limit":5}`)
	if got := readCounters(t, e, id); got != (jobCounters{5, 5, 5}) {
		t.Fatalf("前置条件： %+v", got)
	}
	out := e.post(t, "/openapi/job/modify", tok, `{"job_id":`+strconv.FormatUint(id, 10)+
		`,"max_retry_times":0,"instance_retry_num":0,"instance_num_limit":0}`)
	if out.Code != 0 {
		t.Fatalf("job/modify 失败 code=%d message=%q", out.Code, out.Message)
	}
	if got := readCounters(t, e, id); got != (jobCounters{}) {
		t.Errorf("改不回 0： %+v", got)
	}
}
