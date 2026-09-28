// 无状态令牌的语义：身份绑定、有效期、按项目吊销、跨副本互认、用途隔离。
package auth

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gojob/server/store"
)

// newTokenEnv 起一套令牌体系：真 sqlite 库（吊销基线就在库里），两个同密钥的 Signer
// 用来模拟两个副本。
type tokenEnv struct {
	wt   *WorkerTokens
	mc   *Manager
	rv   *Revocations
	a, b *Signer
	st   *store.Store
}

func newTokenEnv(t *testing.T, secret string) *tokenEnv {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	for _, a := range [][2]string{{"app1", "pw1"}, {"app2", "pw2"}} {
		hash, err := HashPassword(a[1])
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB.Create(&store.AppInfo{AppId: a[0], AppName: a[0], Password: hash,
			CreateTime: now, ModifyTime: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := st.DB.Create(&store.UserInfo{Username: "root", Password: "x", Nickname: "r",
		Role: "SUPER_ADMIN", Status: "ACTIVE", AppScopes: "*",
		CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
	rv := NewRevocationsTTL(st.DB, time.Millisecond) // 测试里不等缓存
	revoked := NewRevoked(st.DB)
	a := NewSigner(secret)
	b := NewSigner(secret)
	return &tokenEnv{
		wt: NewWorkerTokens(a, rv, time.Minute),
		mc: NewManager(b, rv, revoked, time.Hour),
		rv: rv, a: a, b: b, st: st,
	}
}

func mustIssue(t *testing.T, wt *WorkerTokens, appId, instance string) string {
	t.Helper()
	tk, err := wt.Issue(appId, instance)
	if err != nil {
		t.Fatalf("issue token for %s/%s: %v", appId, instance, err)
	}
	return tk
}

func TestWorkerTokenIdentityBinding(t *testing.T) {
	e := newTokenEnv(t, "s3cret")
	tk := mustIssue(t, e.wt, "app1", "w-1")
	if err := e.wt.Validate(tk, "app1", "w-1"); err != nil {
		t.Fatalf("本人自校被拒: %v", err)
	}
	cases := []struct {
		name            string
		appId, instance string
		want            error
	}{
		{"跨项目使用别人的票", "app2", "w-1", ErrTokenImpersonated},
		{"冒充同项目另一节点", "app1", "w-2", ErrTokenImpersonated},
		{"空 token", "", "", ErrTokenStale},
		{"胡造的 token", "app1", "w-1", ErrTokenStale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tk
			if tc.name == "胡造的 token" {
				raw = tk[:len(tk)-8] + "deadbeef"
			}
			if tc.name == "空 token" {
				raw = ""
			}
			err := e.wt.Validate(raw, tc.appId, tc.instance)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// 无状态 = 到点就过期，服务端没有可续的状态；SDK 靠有效期过半主动换票不掉线。
func TestWorkerTokenExpires(t *testing.T) {
	clock := time.Now()
	e := newTokenEnv(t, "s3cret")
	e.a.SetClock(func() time.Time { return clock })
	tk := mustIssue(t, e.wt, "app1", "w-1")
	if err := e.wt.Validate(tk, "app1", "w-1"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Minute + time.Second)
	if err := e.wt.Validate(tk, "app1", "w-1"); !errors.Is(err, ErrTokenStale) {
		t.Fatalf("过期后 err = %v, want ErrTokenStale", err)
	}
}

// 改口令/删项目 = 推进项目代号，整个项目此前签发的票一起失效，别的项目不受影响。
func TestRevokeAppInvalidatesWholeProject(t *testing.T) {
	e := newTokenEnv(t, "s3cret")
	a1 := mustIssue(t, e.wt, "app1", "w-1")
	a2 := mustIssue(t, e.wt, "app1", "w-2")
	b1 := mustIssue(t, e.wt, "app2", "w-1")

	if err := e.wt.RevokeApp("app1"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tk, appId, instance string
		ok                  bool
	}{
		{a1, "app1", "w-1", false},
		{a2, "app1", "w-2", false},
		{b1, "app2", "w-1", true},
	} {
		err := e.wt.Validate(tc.tk, tc.appId, tc.instance)
		if tc.ok != (err == nil) {
			t.Fatalf("%s/%s err = %v, want ok=%v", tc.appId, tc.instance, err, tc.ok)
		}
	}
	// 吊销之后重新注册的票应立刻可用（读到的就是新代号）
	if err := e.wt.Validate(mustIssue(t, e.wt, "app1", "w-1"), "app1", "w-1"); err != nil {
		t.Fatalf("新签发的票被拒: %v", err)
	}
}

// 重新注册不会凭"内存里删条目"作废旧票——这是无状态的固有边界，写进测试以免日后误以为还成立。
func TestReIssueKeepsOldTokenUsableUntilExpiry(t *testing.T) {
	e := newTokenEnv(t, "s3cret")
	old := mustIssue(t, e.wt, "app1", "w-1")
	fresh := mustIssue(t, e.wt, "app1", "w-1")
	if fresh == old {
		t.Fatal("重复注册应发不同载荷（JTI 不同）")
	}
	if err := e.wt.Validate(old, "app1", "w-1"); err != nil {
		t.Fatalf("旧票在自身有效期内仍可用心跳者身份: %v", err)
	}
}

// 多副本互认：同一个集群密钥下，A 副本签的票 B 副本直接能用；密钥不同则一律不认。
func TestTokensCrossReplica(t *testing.T) {
	e := newTokenEnv(t, "shared-secret")
	tk := mustIssue(t, e.wt, "app1", "w-1")

	other := NewWorkerTokens(e.b, e.rv, time.Minute) // 另一个副本：不同 Signer 实例、同一密钥
	if err := other.Validate(tk, "app1", "w-1"); err != nil {
		t.Fatalf("同密钥的另一副本应认这张票: %v", err)
	}

	evil := NewSigner("a-different-secret")
	wrong := NewWorkerTokens(evil, e.rv, time.Minute)
	if err := wrong.Validate(tk, "app1", "w-1"); !errors.Is(err, ErrTokenStale) {
		t.Fatalf("不同密钥的副本不应认这张票, err = %v", err)
	}
	// 反向也不通：拿别人集群的票来本机
	if err := e.wt.Validate(mustIssue(t, wrong, "app1", "w-1"), "app1", "w-1"); !errors.Is(err, ErrTokenStale) {
		t.Fatalf("外部集群签发的票本机不应承认, err = %v", err)
	}
}

// 三种用途互不相通：worker 票打不开管理面，管理面票也领不到任务。
func TestPurposeIsolation(t *testing.T) {
	e := newTokenEnv(t, "s3cret")
	workerTk := mustIssue(t, e.wt, "app1", "w-1")
	consoleTk, err := e.mc.Issue("root")
	if err != nil {
		t.Fatal(err)
	}
	openTk, err := NewOpenAPITokens(e.a, e.rv, time.Hour).Issue("app1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.wt.Validate(consoleTk, "app1", "w-1"); !errors.Is(err, ErrTokenStale) {
		t.Fatalf("管理面票不该能领任务: %v", err)
	}
	if _, ok := e.mc.Claims(workerTk); ok {
		t.Fatal("worker 票不该能打开管理面")
	}
	o := NewOpenAPITokens(e.a, e.rv, time.Hour)
	if appId, ok := o.Lookup(workerTk); ok || appId != "" {
		t.Fatalf("worker 票不该能通过 OpenAPI 校验: %q %v", appId, ok)
	}
	if appId, ok := o.Lookup(openTk); !ok || appId != "app1" {
		t.Fatalf("OpenAPI 票校验异常: %q %v", appId, ok)
	}
}

// 控制台会话：与用户行里的代号比对完成吊销，跨副本即时；退出登录只废这一张。
func TestConsoleSessionLifecycle(t *testing.T) {
	e := newTokenEnv(t, "s3cret")
	tk, err := e.mc.Issue("root")
	if err != nil {
		t.Fatal(err)
	}
	claims, ok := e.mc.Claims(tk)
	if !ok {
		t.Fatal("刚签发的会话票应可验")
	}
	if e.mc.Stale(claims, 0) {
		t.Fatal("代号还没变，不该判成作废")
	}
	// 超管重置密码 / 禁用账号：代号 +1，旧票即刻不被承认（任何副本读同一列）
	if err := e.rv.BumpUser("root"); err != nil {
		t.Fatal(err)
	}
	c2, ok := e.mc.Claims(tk)
	if !ok {
		t.Fatal("验签本身仍应通过（票没坏）")
	}
	// requireLogin 的比对口径：拿票里的代号，与它本来就要读的那一行用户记录比
	var nowEpoch int64
	if err := e.st.DB.Model(&store.UserInfo{}).Where("username = ?", "root").
		Pluck("token_epoch", &nowEpoch).Error; err != nil {
		t.Fatal(err)
	}
	if nowEpoch != 1 {
		t.Fatalf("token_epoch = %d, want 1", nowEpoch)
	}
	if !e.mc.Stale(c2, nowEpoch) {
		t.Fatalf("票里的代号 %d 已落后于用户行的 %d，必须判废", c2.Rev, nowEpoch)
	}
	// 退出登录：只废这一张，别的会话继续可用
	fresh, err := e.mc.Issue("root")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.mc.Claims(fresh); !ok {
		t.Fatal("新会话应可用")
	}
	e.mc.Revoke(fresh)
	if _, ok := e.mc.Claims(fresh); ok {
		t.Fatal("已登出的票不应再被接受")
	}
	if _, ok := e.mc.Claims(tk); !ok {
		t.Fatal("登出一张票不应连带废掉同一用户的其它会话")
	}
}

// 剩余不足一半才换票；换出来的新票带着当前代号。
func TestRenewOnlyNearExpiry(t *testing.T) {
	e := newTokenEnv(t, "s3cret")
	clock := time.Now()
	e.a.SetClock(func() time.Time { return clock })
	e.b.SetClock(func() time.Time { return clock })
	tk, err := e.mc.Issue("root")
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := e.mc.Claims(tk)
	if got := e.mc.RenewIfStale(claims, 0); got != "" {
		t.Fatal("刚签发的票不该换")
	}
	clock = clock.Add(40 * time.Minute) // TTL 1h，过半
	claims, _ = e.mc.Claims(tk)
	fresh := e.mc.RenewIfStale(claims, 0)
	if fresh == "" || fresh == tk {
		t.Fatalf("过半应换发新票, got %q", fresh)
	}
	if c, ok := e.mc.Claims(fresh); !ok || e.mc.Stale(c, 0) {
		t.Fatal("换发的新票应可用")
	}
	// 代号已推进时不再续发：续了也是一张废票，不如让客户端重登
	if got := e.mc.RenewIfStale(claims, 7); got != "" {
		t.Fatal("代号不符时不该续发")
	}
}

// 会话密钥没配（Ephemeral）时仍能自签自验，只是换进程就不认了。
func TestEphemeralSignerStillWorksInProcess(t *testing.T) {
	g := NewSigner("")
	if !g.Ephemeral {
		t.Fatal("空密钥应标记为 Ephemeral")
	}
	tk := g.Issue(PurposeConsole, "root", "", 3, time.Minute)
	c, err := g.Verify(tk, PurposeConsole)
	if err != nil || c.Rev != 3 {
		t.Fatalf("本机自签自验失败: %+v %v", c, err)
	}
	if _, err := NewSigner("").Verify(tk, PurposeConsole); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("另一个随机密钥的副本不该认这张票, err = %v", err)
	}
}
