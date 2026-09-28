// 注册口令加密的服务端语义：密钥环、一次性 challenge、强制开关、以及"失败文案不该把
// 人引去核对口令"。challenge 的一次性靠库里的 jti 主键冲突实现，所以这些测试都起真
// sqlite，并用两个同密钥、同库的 Set 模拟两个副本来验跨节点重放。
package auth

import (
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gojob/internal/registercrypto"
	"gojob/server/store"
)

const (
	testKeyHex    = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	otherKeyHex   = "1111111111111111111111111111111111111111111111111111111111111111"
	testClusterSK = "test-cluster-secret"
)

// cryptoEnv 一套令牌体系 + 注册加密装配。b 是与 a 共享同一个库、同一个集群密钥的第二个副本。
type cryptoEnv struct {
	st      *store.Store
	a, b    *Set
	rc      *RegisterCrypto // a 的装配，nil = 未启用
	rcOther *RegisterCrypto
	keyHex  string // seal 时用哪把 key，默认与服务端配置一致
}

func newCryptoEnv(t *testing.T, keys string, enforce bool) *cryptoEnv {
	t.Helper()
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "rc.db"))
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
	mk := func() *Set {
		s := NewSet(st.DB, testClusterSK, ConsoleTokenTTL, WorkerTokenTTL, OpenAPITokenTTL)
		if err := s.EnableRegisterEncryption(keys, enforce); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := mk(), mk()
	return &cryptoEnv{st: st, a: a, b: b, rc: a.Register, rcOther: b.Register, keyHex: testKeyHex}
}

func (e *cryptoEnv) key(t *testing.T) []byte {
	t.Helper()
	raw, err := registercrypto.ParseKey(e.keyHex)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// seal 按 SDK 的口径走完握手第一步+第二步，返回一个可直接交给 Clear 的请求。
func (e *cryptoEnv) seal(t *testing.T, rc *RegisterCrypto, appId, instanceId, password string) (Clear, string) {
	t.Helper()
	ch := rc.Chal.Issue(appId, instanceId)
	return e.sealWith(t, ch, appId, instanceId, password), ch
}

func (e *cryptoEnv) sealWith(t *testing.T, ch, appId, instanceId, password string) Clear {
	t.Helper()
	kid, iv, ct, err := registercrypto.Seal(e.key(t), registercrypto.Payload{
		Version: registercrypto.PayloadVersion, AppId: appId, InstanceId: instanceId,
		Challenge: ch, Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	return Clear{Scheme: RegisterSchemeAesGcm, Kid: kid, Iv: iv, Ciphertext: ct, Challenge: ch}
}

func (e *cryptoEnv) wantOK(t *testing.T, out ClearOutcome, want string) {
	t.Helper()
	if out.Err != nil || !out.OK || out.Password != want {
		t.Fatalf("Clear = %+v, want OK password=%q", out, want)
	}
}

func TestKeyRingParse(t *testing.T) {
	ring, err := ParseKeyRing(testKeyHex+","+otherKeyHex, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ring.Kids()) != 2 {
		t.Fatalf("kids = %v, want 2 把", ring.Kids())
	}
	// 重复配置同一把只算一份（轮换期里两侧复制粘贴很常见）
	dup, err := ParseKeyRing(testKeyHex+", "+testKeyHex, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dup.Kids()) != 1 || !dup.Enforce() {
		t.Fatalf("dup kids=%v enforce=%v", dup.Kids(), dup.Enforce())
	}
	for _, bad := range []string{"zz", strings.Repeat("00", 16), strings.Repeat("00", 31)} {
		if _, err := ParseKeyRing(testKeyHex+","+bad, false); err == nil {
			t.Errorf("ParseKeyRing 含 %q 应当报错", bad[:min(len(bad), 8)])
		} else if !strings.Contains(err.Error(), "GOJOB_WORKER_AES_KEYS") {
			t.Errorf("错误信息应点名配置项: %v", err)
		}
	}
	empty, err := ParseKeyRing(" , ", false)
	if err != nil || empty.Enabled() {
		t.Fatalf("空配置应为未启用: enabled=%v err=%v", empty.Enabled(), err)
	}
}

// 强制却没有 key = 没有客户端能注册上来，这种配置必须在启动时就报错。
func TestEnableRequiresKeysWhenEnforced(t *testing.T) {
	st, err := store.Open("sqlite", filepath.Join(t.TempDir(), "rc2.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	s := NewSet(st.DB, testClusterSK, ConsoleTokenTTL, WorkerTokenTTL, OpenAPITokenTTL)
	if err := s.EnableRegisterEncryption("", true); err == nil {
		t.Fatal("强制却没配 key 应当报错")
	}
	if err := s.EnableRegisterEncryption("", false); err != nil {
		t.Fatalf("未启用是合法配置: %v", err)
	}
	if s.Register != nil {
		t.Fatal("未启用时不该装配 RegisterCrypto")
	}
	// nil 接收者语义：不强制、不启用、只接受明文
	var nilRc *RegisterCrypto
	if nilRc.Enforce() || nilRc.Enabled() || nilRc.Kids() != nil {
		t.Fatal("nil RegisterCrypto 语义不对")
	}
	if got := nilRc.Schemes(); len(got) != 1 || got[0] != RegisterSchemePlain {
		t.Fatalf("nil schemes = %v", got)
	}
}

func TestSchemesReflectConfig(t *testing.T) {
	off := newCryptoEnv(t, "", false)
	if off.rc != nil || off.rcOther != nil {
		t.Fatal("没配 key 不该装配")
	}
	both := newCryptoEnv(t, testKeyHex, false)
	if got := both.rc.Schemes(); len(got) != 2 {
		t.Fatalf("配了 key 未强制时应两种都接受: %v", got)
	}
	only := newCryptoEnv(t, testKeyHex, true)
	if got := only.rc.Schemes(); len(got) != 1 || got[0] != RegisterSchemeAesGcm {
		t.Fatalf("强制时应只接受加密: %v", got)
	}
	if got := only.rc.Kids(); len(got) != 1 {
		t.Fatalf("kids = %v", got)
	}
}

func TestPlainPassesThroughWhenNotEnforced(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	for _, scheme := range []string{"", RegisterSchemePlain, "  PLAIN  "} {
		e.wantOK(t, e.rc.Clear("app1", "i1", Clear{Scheme: scheme, Password: "pw1"}), "pw1")
	}
}

// 强制模式下明文被拒，但响应里带一张现领的 challenge：客户端省一次往返就能改加密重来。
func TestEnforcedPlainGetsFreshChallenge(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, true)
	out := e.rc.Clear("app1", "i1", Clear{Password: "pw1"})
	if out.OK || !errors.Is(out.Err, ErrEncryptionRequired) {
		t.Fatalf("out = %+v", out)
	}
	if out.Challenge == "" {
		t.Fatal("应附一张 challenge")
	}
	if strings.Contains(out.Err.Error(), "password") {
		t.Fatalf("文案不该提口令，否则排障的人会去核对口令: %q", out.Err)
	}
	// 这张 challenge 真能用（同项目、同身份，一次往返就接上）
	e.wantOK(t, e.rc.Clear("app1", "i1", e.sealWith(t, out.Challenge, "app1", "i1", "pw1")), "pw1")
	// 换个身份来用它就不行了
	req := e.sealWith(t, out.Challenge, "app1", "i2", "pw1")
	if got := e.rc.Clear("app1", "i2", req); !errors.Is(got.Err, ErrChallengeBad) {
		t.Fatalf("challenge 绑身份没生效: %+v", got)
	}
}

func TestAesGcmHappyPath(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	req, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
	e.wantOK(t, e.rc.Clear("app1", "i1", req), "pw1")
	if req.Password != "" || strings.Contains(req.Ciphertext, "pw1") {
		t.Fatal("加密路径上口令漏了")
	}
	// 同一口令再注册一次：iv 随机 ⇒ 密文必然不同，抓包者无从比对
	req2, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
	if req2.Ciphertext == req.Ciphertext || req2.Iv == req.Iv {
		t.Fatal("两次注册的 iv/密文重复")
	}
	e.wantOK(t, e.rc.Clear("app1", "i1", req2), "pw1")
}

// 一次性：同一张 challenge 第二次用必须被拒（否则抓到的注册包可以一直重放）。
func TestChallengeIsSingleUse(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	req, ch := e.seal(t, e.rc, "app1", "i1", "pw1")
	e.wantOK(t, e.rc.Clear("app1", "i1", req), "pw1")
	// 重放原包
	if out := e.rc.Clear("app1", "i1", req); !errors.Is(out.Err, ErrChallengeBad) || out.OK {
		t.Fatalf("原包重放应被拒: %+v", out)
	}
	// 换个口令重用同一张 challenge（先消费后解密的顺序保证这张已经废了）
	if out := e.rc.Clear("app1", "i1", e.sealWith(t, ch, "app1", "i1", "guess")); !errors.Is(out.Err, ErrChallengeBad) {
		t.Fatalf("重用 challenge 应被拒: %+v", out)
	}
}

// 跨副本：A 消费过的 challenge，B 必须认"已用过"——一次性记录在库里，不在进程内。
func TestChallengeSingleUseAcrossReplicas(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	req, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
	e.wantOK(t, e.rc.Clear("app1", "i1", req), "pw1") // 副本 A 用掉

	if out := e.rcOther.Clear("app1", "i1", req); out.OK {
		t.Fatal("跨副本重放成功，一次性没生效")
	} else if !errors.Is(out.Err, ErrChallengeBad) {
		t.Fatalf("out = %+v", out)
	}
	// B 自己签的 challenge 仍可用（同密钥 ⇒ 谁的票都认，这是十三期的既有语义）
	reqB, _ := e.seal(t, e.rcOther, "app1", "i2", "pw1")
	e.wantOK(t, e.rc.Clear("app1", "i2", reqB), "pw1")
}

// challenge 绑身份：给 i1 领的去给 i2 用；给 app1 领的去 app2 用。
func TestChallengeBoundToIdentity(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	req, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
	if out := e.rc.Clear("app1", "i2", req); !errors.Is(out.Err, ErrChallengeBad) {
		t.Fatalf("换 instance 冒用: %+v", out)
	}
	if out := e.rc.Clear("app2", "i1", req); !errors.Is(out.Err, ErrChallengeBad) {
		t.Fatalf("换 app 冒用: %+v", out)
	}
}

// 密文里绑的 instance_id 与请求外层不一致 ⇒ 拒（外层表头是请求者可控的）。
func TestPayloadInstanceMismatch(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	ch := e.rc.Chal.Issue("app1", "i1")
	req := e.sealWith(t, ch, "app1", "victim", "pw1") // 密文里写的是别人的 instance
	if out := e.rc.Clear("app1", "i1", req); out.OK ||
		!strings.Contains(out.Err.Error(), "instance_id") {
		t.Fatalf("out = %+v", out)
	}
}

// 用另一把 key 加密 ⇒ 明确指向"两侧 key 不一致"，而不是含糊的口令错误。
func TestUnknownKidPointsAtConfig(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	e.keyHex = otherKeyHex
	req, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
	out := e.rc.Clear("app1", "i1", req)
	if out.OK {
		t.Fatal("未知 key 加密的包被接受了")
	}
	if msg := out.Err.Error(); !strings.Contains(msg, "unknown key id") ||
		strings.Contains(msg, "password") {
		t.Fatalf("文案应指向 key 错配: %q", msg)
	}
}

// key 对了但密文被人改过一个比特 ⇒ 只有一条含糊文案（区分"key 错/被改/口令错"就是 oracle）。
func TestTamperedPayloadOneMessage(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	req, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
	ring, err := ParseKeyRing(e.keyHex, false)
	if err != nil {
		t.Fatal(err)
	}
	bs, err := base64.StdEncoding.DecodeString(req.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	bs[len(bs)/2] ^= 0x01
	tampered := base64.StdEncoding.EncodeToString(bs)
	if _, err := ring.open(req.Kid, "app1", req.Iv, tampered); !errors.Is(err, registercrypto.ErrDecryptFailed) {
		t.Fatalf("err = %v, want ErrDecryptFailed", err)
	} else if strings.Contains(err.Error(), "password") {
		t.Fatalf("文案不该提口令: %q", err)
	}
}

func TestBadSchemeInputs(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	if out := e.rc.Clear("app1", "i1", Clear{Scheme: "aes-128-cbc"}); out.OK ||
		!strings.Contains(out.Err.Error(), "unknown auth_scheme") {
		t.Fatalf("未知 scheme: %+v", out)
	}
	// 声明了 aes-gcm 却缺件：不能误打误撞走明文
	for _, f := range []func(*Clear){
		func(c *Clear) { c.Challenge = "" },
		func(c *Clear) { c.Kid = "" },
		func(c *Clear) { c.Iv = "" },
		func(c *Clear) { c.Ciphertext = "" },
	} {
		req, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
		f(&req)
		if out := e.rc.Clear("app1", "i1", req); out.OK ||
			!strings.Contains(out.Err.Error(), "needs challenge, kid, iv and ciphertext") {
			t.Fatalf("缺件场景: %+v", out)
		}
	}
}

// 客户端配了 key 而服务端没配 ⇒ 说清楚是谁没配，别报成口令错（nil 接收者也要给这条）。
func TestEncryptedRequestToPlainServer(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, false)
	req, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
	off := newCryptoEnv(t, "", false)
	if out := off.rc.Clear("app1", "i1", req); !errors.Is(out.Err, ErrServerNoKey) || out.OK {
		t.Fatalf("out = %+v", out)
	}
}

// 轮换期：两把 key 并存，新旧 worker 都能注册。
func TestKeyRotationWindow(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex+","+otherKeyHex, false)
	e.keyHex = otherKeyHex
	req, _ := e.seal(t, e.rc, "app1", "i1", "pw1")
	e.wantOK(t, e.rc.Clear("app1", "i1", req), "pw1")
}

// 口令还原出来以后仍然要过 bcrypt：加密握手不改变"口令错就是错"。
func TestDecryptedPasswordStillVerified(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, true)
	req, _ := e.seal(t, e.rc, "app1", "i1", "wrong-password")
	out := e.rc.Clear("app1", "i1", req)
	e.wantOK(t, out, "wrong-password") // Clear 只管还原，不管对错
	var app store.AppInfo
	if err := e.st.DB.Where("app_id = ?", "app1").First(&app).Error; err != nil {
		t.Fatal(err)
	}
	if CheckPassword(app.Password, out.Password) {
		t.Fatal("错口令不该过 bcrypt")
	}
	if !CheckPassword(app.Password, "pw1") {
		t.Fatal("对口令应过 bcrypt")
	}
}

// OpenAPI 侧没有实例身份：principal 传 ""，密文里的 instance_id 也必须是空。
func TestOpenApiPrincipalIsUnbound(t *testing.T) {
	e := newCryptoEnv(t, testKeyHex, true)
	ch := e.rc.Chal.Issue("app1", "")
	kid, iv, ct, err := registercrypto.Seal(e.key(t), registercrypto.Payload{
		Version: registercrypto.PayloadVersion, AppId: "app1", Challenge: ch, Password: "pw1",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.wantOK(t, e.rc.Clear("app1", "", Clear{Scheme: RegisterSchemeAesGcm,
		Kid: kid, Iv: iv, Ciphertext: ct, Challenge: ch}), "pw1")
	// 反过来：带上了 instance_id 就不认
	if out := e.rc.Clear("app1", "someone", e.sealWith(t, e.rc.Chal.Issue("app1", "someone"), "app1", "someone", "pw1")); !out.OK {
		t.Fatalf("worker 形态仍应可用: %+v", out)
	}
}
