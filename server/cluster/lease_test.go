// 调度归属的状态机：抢占、续约、接管、fencing，以及优雅下线交出租约。
// 这里用同一进程内的两个 Leases 实例代表两个副本——它们共用一个库，
// 归属仲裁全部发生在 SQL 的条件 UPDATE 上，所以这已经是在测真实竞争。
package cluster

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gojob/server/store"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func newLeaseDB(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

func addApp(t *testing.T, st *store.Store, appId string) {
	t.Helper()
	now := store.Now()
	if err := st.DB.Create(&store.AppInfo{AppId: appId, AppName: appId, Password: "x",
		CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
}

// 两副本抢同一个项目：只有一个能拿到，且拿到之后续约不改 epoch。
func TestLeaseSingleOwnerPerApp(t *testing.T) {
	st := newLeaseDB(t)
	addApp(t, st, "app1")
	clk := &fakeClock{t: store.Now()}
	a := NewLeases(st.DB, "server-a", LeaseTTL)
	b := NewLeases(st.DB, "server-b", LeaseTTL)
	a.now, b.now = clk.Now, clk.Now

	a.Sync()
	b.Sync()
	if !a.Owns("app1") {
		t.Fatal("server-a 应先抢到 app1")
	}
	if b.Owns("app1") {
		t.Fatal("server-b 抢到了同一个活租约，条件 UPDATE 没起作用")
	}
	ep0, _ := a.Epoch("app1")
	if ep0 != 1 {
		t.Fatalf("首次抢占 epoch = %d, want 1", ep0)
	}

	clk.Advance(RenewInterval)
	a.Sync()
	if ep, _ := a.Epoch("app1"); ep != ep0 {
		t.Fatalf("续约不应改变 epoch：%d -> %d", ep0, ep)
	}
	var row store.AppLease
	if err := st.DB.Where("app_id = ?", "app1").First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.RenewCount < 2 {
		t.Fatalf("renew_count = %d, 续约/抢占都应累加", row.RenewCount)
	}
}

// 持有者静默超过 TTL 才可被接管，接管必须把 epoch 往前推（这是 fencing 的依据）。
func TestLeaseTakeoverAfterTTL(t *testing.T) {
	st := newLeaseDB(t)
	addApp(t, st, "app1")
	clk := &fakeClock{t: store.Now()}
	a := NewLeases(st.DB, "server-a", LeaseTTL)
	b := NewLeases(st.DB, "server-b", LeaseTTL)
	a.now, b.now = clk.Now, clk.Now

	a.Sync()
	lost := []string{}
	a.OnLost(func(appId string) { lost = append(lost, appId) })

	clk.Advance(LeaseTTL + time.Second)
	b.Sync()
	if !b.Owns("app1") {
		t.Fatal("租约过期后 server-b 应能接管")
	}
	if ep, _ := b.Epoch("app1"); ep != 2 {
		t.Fatalf("接管后 epoch = %d, want 2", ep)
	}

	// 旧主再来续约：应认输并回调撤轮
	clk.Advance(RenewInterval)
	a.Sync()
	if a.Owns("app1") {
		t.Fatal("server-a 不该还以为自己持有 app1")
	}
	if len(lost) != 1 || lost[0] != "app1" {
		t.Fatalf("OnLost 回调 = %v, want [app1]", lost)
	}
	// 新主继续持有不受影响
	b.Sync()
	if !b.Owns("app1") {
		t.Fatal("server-b 续约失败")
	}
}

// fencing：归属易主后，旧主哪怕还"活着"（没走到 Sync）也无法通过写库前的确认。
func TestFenceBlocksZombieOwner(t *testing.T) {
	st := newLeaseDB(t)
	addApp(t, st, "app1")
	clk := &fakeClock{t: store.Now()}
	a := NewLeases(st.DB, "server-a", LeaseTTL)
	b := NewLeases(st.DB, "server-b", LeaseTTL)
	a.now, b.now = clk.Now, clk.Now
	a.Sync()
	if !a.Fence(st.DB, "app1") {
		t.Fatal("持有者自己的 fencing 应通过")
	}

	clk.Advance(LeaseTTL + time.Second)
	b.Sync()
	if a.Fence(st.DB, "app1") {
		t.Fatal("僵尸主的 fencing 必须被拒（兼容协议在这一场景没有防护）")
	}
	// fencing 被拒应顺带交出本地归属，避免后面每次都白撞一次
	if a.Owns("app1") {
		t.Fatal("fence 失败后应撤销本机归属")
	}
	// 从没持有任何项目的副本更不该通过
	c := NewLeases(st.DB, "server-c", LeaseTTL)
	if c.Fence(st.DB, "app1") {
		t.Fatal("非持有者不应通过 fencing")
	}
}

// 租约跟着项目走：新建 app 下一跳被无人持有的自然收下；删掉 app 则主动放手。
func TestLeaseFollowsAppLifecycle(t *testing.T) {
	st := newLeaseDB(t)
	a := NewLeases(st.DB, "server-a", LeaseTTL)
	a.Sync()
	if len(a.Owned()) != 0 {
		t.Fatalf("没有项目时不该持有任何归属: %v", a.Owned())
	}
	addApp(t, st, "app1")
	addApp(t, st, "app2")
	a.Sync()
	if got := a.Owned(); len(got) != 2 {
		t.Fatalf("owned = %v, want 2 个项目", got)
	}
	if err := st.DB.Where("app_id = ?", "app1").Delete(&store.AppInfo{}).Error; err != nil {
		t.Fatal(err)
	}
	a.Sync()
	if got := a.Owned(); len(got) != 1 || got[0] != "app2" {
		t.Fatalf("app 删除后 owned = %v, want [app2]", got)
	}
}

// 优雅下线要立刻交出全部租约，接管方不必空等一个 TTL。
func TestGracefulRelease(t *testing.T) {
	st := newLeaseDB(t)
	addApp(t, st, "app1")
	clk := &fakeClock{t: store.Now()}
	a := NewLeases(st.DB, "server-a", LeaseTTL)
	b := NewLeases(st.DB, "server-b", LeaseTTL)
	a.now, b.now = clk.Now, clk.Now

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	// Run 启动即同步一次，等它拿下归属（不能用 time.Sleep 猜：这里注入的是假时钟，
	// 真实时间过一帧就够 Sync 跑完）。
	for i := 0; i < 200 && !a.Owns("app1"); i++ {
		time.Sleep(time.Millisecond)
	}
	if !a.Owns("app1") {
		t.Fatal("Run 启动后应持有 app1")
	}
	cancel()
	<-done
	b.Sync()
	if !b.Owns("app1") {
		t.Fatal("对方优雅退出后应能立即接管，无需等满 TTL")
	}
}

// Summarize 是只读视图，页面靠它区分"没人调度"与"租约已过期"。
func TestSummarizeDistinguishesStates(t *testing.T) {
	st := newLeaseDB(t)
	addApp(t, st, "held")
	a := NewLeases(st.DB, "server-a", LeaseTTL)
	a.Sync()              // 只有 held 有租约行
	addApp(t, st, "free") // 再建一个 app 但不再 Sync：它一行租约都没有

	rows, err := Summarize(st.DB)
	if err != nil {
		t.Fatal(err)
	}
	byApp := map[string]HolderSummary{}
	for _, r := range rows {
		byApp[r.AppId] = r
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if h := byApp["held"]; h.Holder != "server-a" || !h.Alive || h.Epoch != 1 {
		t.Fatalf("held = %+v", h)
	}
	if h := byApp["free"]; h.Holder != "" || h.Alive {
		t.Fatalf("free = %+v, 无租约行的 app 应显示为无人持有", h)
	}
}
