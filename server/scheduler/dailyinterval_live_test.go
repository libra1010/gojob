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

// 回归：停摆 3 小时后重启，DAILY_TIME_INTERVAL 与 CRON/FIXED_RATE 一样只补一次，
// 不允许把每个错过的时间点都变成实例。
func TestDailyTimeIntervalStaleNextDoesNotBurst(t *testing.T) {
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "daily-stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	expr := `{"interval":1,"intervalUnit":"SECONDS","startTimeOfDay":"00:00:00","endTimeOfDay":"23:59:59"}`
	job := store.JobInfo{
		AppId: "demo", JobName: "daily-stale", Processor: "x",
		TimeExpressionType: "DAILY_TIME_INTERVAL", TimeExpression: expr,
		Enable: true, NextTriggerTime: now.Add(-3 * time.Hour), CreateTime: now, ModifyTime: now,
	}
	if err := st.Create(&job); err != nil {
		t.Fatal(err)
	}

	s := New(st, nil, func() {}, nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
	defer cancel()
	s.Run(ctx)

	var n int64
	if err := st.DB.Model(&store.JobInstance{}).Where("job_id = ?", job.ID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	// 正常最多是"启动补一次 + 1s 后按节奏再一次"；旧实现会在这 1.2s 内产出成百上千个
	if n > 5 {
		t.Fatalf("停摆重启后 1.2s 内生成 %d 个实例，按历史间隔连续追补了", n)
	}
	var refreshed store.JobInfo
	if err := st.DB.First(&refreshed, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if refreshed.NextTriggerTime.Before(now.Add(-time.Minute)) {
		t.Fatalf("next_trigger_time 仍停在历史时刻 %s", refreshed.NextTriggerTime)
	}
}
