package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gojob/server/store"
)

// 端到端守住"每日固定间隔真的会产出实例"：单测只覆盖 NextByType 的时间计算，
// 装配/扫描/时间轮任何一环断了，这里会直接看到 0 条实例。
func TestDailyTimeIntervalCreatesInstances(t *testing.T) {
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "daily.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	expr := `{"interval":1,"intervalUnit":"SECONDS","startTimeOfDay":"00:00:00","endTimeOfDay":"23:59:59"}`
	next, err := NextByType("DAILY_TIME_INTERVAL", expr, now, now)
	if err != nil {
		t.Fatal(err)
	}
	job := store.JobInfo{
		AppId: "demo", JobName: "daily-live", Processor: "x",
		TimeExpressionType: "DAILY_TIME_INTERVAL", TimeExpression: expr,
		Enable: true, NextTriggerTime: next, CreateTime: now, ModifyTime: now,
	}
	if err := st.Create(&job); err != nil {
		t.Fatal(err)
	}

	s := New(st, nil, func() {}, nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2800*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	var insts []store.JobInstance
	if err := st.DB.Where("job_id = ?", job.ID).Find(&insts).Error; err != nil {
		t.Fatal(err)
	}
	if len(insts) == 0 {
		t.Fatal("没有生成实例")
	}
	for _, i := range insts {
		if i.Source != "DAILY_TIME_INTERVAL" {
			t.Fatalf("实例 source = %q，想要原值 DAILY_TIME_INTERVAL", i.Source)
		}
	}
}
