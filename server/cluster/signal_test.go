package cluster

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gojob/server/store"
)

func newSignalDB(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "signal.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

// 一个副本入队，另一个副本的监听者要能在一个轮询周期内被叫醒；
// 唤醒只传"变过了"这一个比特，谁去认领仍由 DB 上的条件 UPDATE 决定。
func TestSignalBroadcastWakesPeer(t *testing.T) {
	st := newSignalDB(t)
	a, b := NewSignal(st.DB), NewSignal(st.DB)
	if _, err := a.Seq(); err != nil {
		t.Fatalf("singleton 行应由 Migrate 建好: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	woken := make(chan struct{}, 8)
	go b.watch(ctx, 5*time.Millisecond, func() { woken <- struct{}{} })

	// 等监听者读到基准 seq，否则下面第一次 bump 可能被当成"启动前就变了"
	time.Sleep(30 * time.Millisecond)
	if err := a.Bump(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-woken:
	case <-time.After(time.Second):
		t.Fatal("对端副本没被唤醒")
	}
	if seq, _ := b.Seq(); seq != 1 {
		t.Fatalf("seq = %d, want 1", seq)
	}
}

// 启动时已存在的计数不该立刻触发一次空唤醒。
func TestSignalWatchIgnoresPreexistingSeq(t *testing.T) {
	st := newSignalDB(t)
	a, b := NewSignal(st.DB), NewSignal(st.DB)
	if err := a.Bump(); err != nil {
		t.Fatal(err)
	}
	if err := a.Bump(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls int
	done := make(chan struct{})
	go func() { b.watch(ctx, 5*time.Millisecond, func() { calls++ }); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	if calls != 0 {
		t.Fatalf("刚起来就空唤 %d 次", calls)
	}
}

// 信号行丢失（有人手工清库/迁移漏了）时，Bump 不得 panic，且能自愈重建。
func TestSignalBumpSurvivesMissingRow(t *testing.T) {
	st := newSignalDB(t)
	s := NewSignal(st.DB)
	if err := st.DB.Where("id = ?", store.SignalRowID).Delete(&store.ClusterSignal{}).Error; err != nil {
		t.Fatal(err)
	}
	// 第一次 bump 要能自愈：补回那行并完成本次广播，而不是静默丢掉或 panic
	if err := s.Bump(); err != nil {
		t.Fatalf("缺行时 Bump 应自愈而不是报错: %v", err)
	}
	if seq, err := s.Seq(); err != nil || seq != 1 {
		t.Fatalf("自愈后 seq = %d err = %v, want 1", seq, err)
	}
	// 之后 EnsureRow 幂等、广播照常
	if err := s.EnsureRow(); err != nil {
		t.Fatal(err)
	}
	if err := s.Bump(); err != nil {
		t.Fatal(err)
	}
	if seq, _ := s.Seq(); seq != 2 {
		t.Fatalf("seq = %d, want 2", seq)
	}
}
