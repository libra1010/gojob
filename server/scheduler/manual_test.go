package scheduler

import (
	"errors"
	"path/filepath"
	"testing"

	"gojob/server/store"
)

// 暂停的任务不接受手动触发：认领层按 enable 硬过滤，放行只会留下一个永远拉不到的实例。
func TestManualTriggerRejectsPausedJob(t *testing.T) {
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "paused.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	s := New(st, nil, func() {}, nil, nil, nil)
	now := store.Now()
	job := store.JobInfo{AppId: "demo", JobName: "paused", Processor: "x",
		TimeExpressionType: "API", Enable: false, CreateTime: now, ModifyTime: now}
	if err := st.Create(&job); err != nil {
		t.Fatal(err)
	}
	// enable 列带 default:true，Create 会跳过零值；先建再显式改成暂停态。
	if err := st.DB.Model(&job).Update("enable", false).Error; err != nil {
		t.Fatal(err)
	}
	job.Enable = false

	if err := s.TriggerManually(&job); !errors.Is(err, ErrJobPaused) {
		t.Fatalf("TriggerManually err = %v, want ErrJobPaused", err)
	}
	if _, err := s.TriggerRun(&job, "{}"); !errors.Is(err, ErrJobPaused) {
		t.Fatalf("TriggerRun err = %v, want ErrJobPaused", err)
	}

	var n int64
	if err := st.DB.Model(&store.JobInstance{}).Where("job_id = ?", job.ID).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("暂停任务被触发后留下了 %d 个实例", n)
	}
}
