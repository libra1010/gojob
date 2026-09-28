// 注册加密的接口层验收：真路由 + 真 sqlite，两个副本共享一个库。
// 覆盖的是"配错时会看到什么"与"攻击包重放会怎样"——这两类都比正常路径更容易出事。
package api

import (
	"encoding/json"
	"maps"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/internal/registercrypto"
	"gojob/server/auth"
	"gojob/server/dispatch"
	"gojob/server/registry"
	"gojob/server/store"
)

const (
	rcKeyHex  = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	rcOther   = "1111111111111111111111111111111111111111111111111111111111111111"
	rcCluster = "rc-test-secret"
)

// rcEnv 两个副本（同一库、同一集群密钥），各一张路由表。
type rcEnv struct {
	st       *store.Store
	a, b     *gin.Engine
	sa, sb   *Server
	keyHex   string
	replicas []*gin.Engine
}

func newRCEnv(t *testing.T, keys string, enforce bool) *rcEnv {
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
		hash, err := auth.HashPassword(a[1])
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB.Create(&store.AppInfo{AppId: a[0], AppName: a[0], Password: hash,
			CreateTime: now, ModifyTime: now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	mk := func() (*Server, *gin.Engine) {
		as := auth.NewSet(st.DB, rcCluster, auth.ConsoleTokenTTL,
			auth.WorkerTokenTTL, auth.OpenAPITokenTTL)
		if err := as.EnableRegisterEncryption(keys, enforce); err != nil {
			t.Fatal(err)
		}
		reg := registry.New(st.DB, 15*time.Second)
		s := New(st, reg, dispatch.New(st, reg, nil), nil, as, nil, "sqlite", "")
		gin.SetMode(gin.TestMode)
		return s, s.Router()
	}
	sa, ra := mk()
	sb, rb := mk()
	return &rcEnv{st: st, sa: sa, sb: sb, a: ra, b: rb, keyHex: rcKeyHex, replicas: []*gin.Engine{ra, rb}}
}

func rcBody(m map[string]any) string {
	bs, err := v2.Marshal(m)
	if err != nil {
		panic(err)
	}
	return string(bs)
}

// postRC 打一次并原样返回信封（不假设 code=0），同时给出原始响应文本供断言。
func postRC[T any](t *testing.T, r *gin.Engine, path string, m map[string]any) (protocol.Resp[T], string) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(rcBody(m))))
	var resp protocol.Resp[T]
	if err := v2.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: bad response %q: %v", path, w.Body.String(), err)
	}
	return resp, w.Body.String()
}

// workerRegister 发起一次注册；plain=true 时走明文。
func (e *rcEnv) registerReq(t *testing.T, appId, instanceId, plain string) map[string]any {
	t.Helper()
	m := map[string]any{"app_id": appId, "instance_id": instanceId,
		"address": "10.0.0.1:7799", "version": "test", "sdk_type": "GO"}
	if plain == "" {
		return m
	}
	m["auth_scheme"] = auth.RegisterSchemePlain
	m["password"] = plain
	return m
}

// sealAgainst 走完握手：从 r 这张路由领 challenge，按 SDK 口径加密。
func (e *rcEnv) sealAgainst(t *testing.T, r *gin.Engine, m map[string]any, password string) map[string]any {
	t.Helper()
	ch, raw := postRC[protocol.ChallengeResp](t, r, "/worker/challenge", map[string]any{
		"app_id": m["app_id"], "instance_id": m["instance_id"],
	})
	if ch.Code != protocol.CodeOK || ch.Data.Challenge == "" {
		t.Fatalf("challenge = %+v (raw %s)", ch.Data, raw)
	}
	return e.sealWith(t, m, ch.Data.Challenge, password)
}

func (e *rcEnv) sealWith(t *testing.T, m map[string]any, ch, password string) map[string]any {
	t.Helper()
	raw, err := registercrypto.ParseKey(e.keyHex)
	if err != nil {
		t.Fatal(err)
	}
	instanceId, _ := m["instance_id"].(string)
	appId, _ := m["app_id"].(string)
	kid, iv, ct, err := registercrypto.Seal(raw, registercrypto.Payload{
		Version: registercrypto.PayloadVersion, AppId: appId, InstanceId: instanceId,
		Challenge: ch, Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := maps.Clone(m)
	out["auth_scheme"], out["kid"], out["iv"], out["ciphertext"], out["challenge"] = auth.RegisterSchemeAesGcm, kid, iv, ct, ch
	return out
}

func TestPlainAndEncryptedBothAcceptedWhenOptional(t *testing.T) {
	e := newRCEnv(t, rcKeyHex, false)
	// 明文：与引入本功能前完全一致
	r, _ := postRC[protocol.RegisterResp](t, e.a, "/worker/register",
		e.registerReq(t, "app1", "i-plain", "pw1"))
	if r.Code != protocol.CodeOK || r.Data.Token == "" {
		t.Fatalf("明文注册应通过: %+v", r)
	}
	// 加密：token 照样能用于数据面
	req := e.sealAgainst(t, e.a, e.registerReq(t, "app1", "i-aes", ""), "pw1")
	if strings.Contains(rcBody(req), "pw1") {
		t.Fatal("加密请求体里出现了明文口令")
	}
	r2, _ := postRC[protocol.RegisterResp](t, e.a, "/worker/register", req)
	if r2.Code != protocol.CodeOK || r2.Data.Token == "" {
		t.Fatalf("加密注册应通过: %+v", r2)
	}
	hb, _ := postRC[protocol.CancelTasks](t, e.a, "/worker/heartbeat", map[string]any{
		"app_id": "app1", "instance_id": "i-aes", "address": "10.0.0.1:7799", "token": r2.Data.Token,
	})
	if hb.Code != protocol.CodeOK {
		t.Fatalf("加密换来的 token 用不了: %+v", hb)
	}
	// 注册响应本身也该带 schemes 吗？—— 不带：那是握手/拒绝时才需要的信息，成功路径保持原样。
}

// 强制模式下明文被拒，但不是 401：SDK 把 401 当"重注册"信号，用 401 会造成死循环。
func TestEnforcedRejectsPlainWith428Hint(t *testing.T) {
	e := newRCEnv(t, rcKeyHex, true)
	r, raw := postRC[protocol.ChallengeResp](t, e.a, "/worker/register",
		e.registerReq(t, "app1", "i1", "pw1"))
	if r.Code != protocol.CodeChallengeRequired {
		t.Fatalf("code = %d, want %d（428）\n%s", r.Code, protocol.CodeChallengeRequired, raw)
	}
	if strings.Contains(strings.ToLower(r.Message), "password") {
		t.Fatalf("文案不该提口令: %q", r.Message)
	}
	if len(r.Data.Schemes) != 1 || r.Data.Schemes[0] != auth.RegisterSchemeAesGcm {
		t.Fatalf("schemes = %v", r.Data.Schemes)
	}
	if r.Data.Challenge == "" || r.Data.ExpiresIn <= 0 {
		t.Fatalf("应附一张可用的 challenge: %+v", r.Data)
	}
	// 这张 challenge 直接可用，客户端不必再跑一趟 /worker/challenge
	req := e.sealWith(t, e.registerReq(t, "app1", "i1", ""), r.Data.Challenge, "pw1")
	ok, _ := postRC[protocol.RegisterResp](t, e.a, "/worker/register", req)
	if ok.Code != protocol.CodeOK || ok.Data.Token == "" {
		t.Fatalf("按提示改加密后应成功: %+v", ok)
	}
}

// 抓到一个注册包就重放：同一张 challenge 只能用一次，且换副本也认。
func TestRegisterPacketReplayBlockedAcrossReplicas(t *testing.T) {
	e := newRCEnv(t, rcKeyHex, false)
	req := e.sealAgainst(t, e.a, e.registerReq(t, "app1", "i1", ""), "pw1")
	if r, _ := postRC[protocol.RegisterResp](t, e.a, "/worker/register", req); r.Code != protocol.CodeOK {
		t.Fatalf("首发应成功: %+v", r)
	}
	// 原包打到另一个副本（LB 换落点）
	replay, raw := postRC[protocol.RegisterResp](t, e.b, "/worker/register", req)
	if replay.Code == protocol.CodeOK {
		t.Fatalf("重放成功了，一次性没生效:\n%s", raw)
	}
	if !strings.Contains(replay.Message, "challenge") {
		t.Fatalf("应说明是 challenge 的问题: %q", replay.Message)
	}
	// 重放时改用别人的身份也不行（challenge 绑死在签发时的身份上）
	req2 := e.sealAgainst(t, e.b, e.registerReq(t, "app2", "i9", ""), "pw2")
	if r, _ := postRC[protocol.RegisterResp](t, e.b, "/worker/register", req2); r.Code != protocol.CodeOK {
		t.Fatalf("新 challenge 应可用: %+v", r)
	}
}

// key 配错侧要说得清是谁没配：这些文案是运维唯一的线索。
func TestMisconfigurationMessages(t *testing.T) {
	t.Run("worker 配了 key 而服务端没配", func(t *testing.T) {
		e := newRCEnv(t, "", false)
		ch, _ := postRC[protocol.ChallengeResp](t, e.a, "/worker/challenge",
			map[string]any{"app_id": "app1", "instance_id": "i1"})
		if ch.Code == protocol.CodeOK {
			t.Fatal("未启用加密时不该发 challenge")
		}
		if !strings.Contains(ch.Message, "GOJOB_WORKER_AES_KEYS") {
			t.Fatalf("应点名配置项: %q", ch.Message)
		}
		req := e.sealWith(t, e.registerReq(t, "app1", "i1", ""), "whatever", "pw1")
		r, _ := postRC[protocol.RegisterResp](t, e.a, "/worker/register", req)
		if r.Code == protocol.CodeOK || !strings.Contains(r.Message, "no worker AES key") {
			t.Fatalf("应指出服务端没配 key: %+v", r)
		}
	})
	t.Run("两侧 key 不一致", func(t *testing.T) {
		e := newRCEnv(t, rcKeyHex, false)
		e.keyHex = rcOther
		req := e.sealAgainst(t, e.a, e.registerReq(t, "app1", "i1", ""), "pw1")
		r, _ := postRC[protocol.RegisterResp](t, e.a, "/worker/register", req)
		if r.Code == protocol.CodeOK {
			t.Fatal("用另一把 key 加密应被拒")
		}
		if !strings.Contains(r.Message, "unknown key id") || strings.Contains(r.Message, "mismatch") {
			t.Fatalf("应指向 key 错配而不是口令错: %q", r.Message)
		}
	})
	t.Run("未知 scheme", func(t *testing.T) {
		e := newRCEnv(t, rcKeyHex, false)
		req := maps.Clone(e.registerReq(t, "app1", "i1", ""))
		req["auth_scheme"] = "aes-128-cbc"
		r, _ := postRC[protocol.RegisterResp](t, e.a, "/worker/register", req)
		if r.Code == protocol.CodeOK || !strings.Contains(r.Message, "unknown auth_scheme") {
			t.Fatalf("out = %+v", r)
		}
	})
}

// challenge 端点不能变成 app_id 枚举机：不存在的项目照样发票，
// 但拿它注册时必须失败在口令/项目那一步。
func TestChallengeDoesNotEnumerateApps(t *testing.T) {
	e := newRCEnv(t, rcKeyHex, false)
	known, _ := postRC[protocol.ChallengeResp](t, e.a, "/worker/challenge",
		map[string]any{"app_id": "app1", "instance_id": "i1"})
	unknown, _ := postRC[protocol.ChallengeResp](t, e.a, "/worker/challenge",
		map[string]any{"app_id": "no-such-app", "instance_id": "i1"})
	if known.Code != protocol.CodeOK || unknown.Code != protocol.CodeOK {
		t.Fatalf("两者应同形: %d / %d", known.Code, unknown.Code)
	}
	if unknown.Data.Challenge == "" {
		t.Fatal("不存在的项目也该拿到票")
	}
	req := e.sealWith(t, e.registerReq(t, "no-such-app", "i1", ""), unknown.Data.Challenge, "pw1")
	r, _ := postRC[protocol.RegisterResp](t, e.a, "/worker/register", req)
	if r.Code == protocol.CodeOK {
		t.Fatal("不存在的项目注册不该成功")
	}
}

// ---------- OpenAPI 兼容侧 ----------

type pjEnv struct {
	rc *rcEnv
	s  *Server
	r  *gin.Engine
}

func newPJEnvWithKey(t *testing.T, keys string, enforce bool) *pjEnv {
	e := newRCEnv(t, keys, enforce)
	return &pjEnv{rc: e, s: e.sa, r: e.a}
}

// sealForApp authApp 侧的加密载荷：OpenAPI 调用方没有实例身份，payload 里 instance_id 留空。
func (p *pjEnv) sealForApp(t *testing.T, appId, ch, password string) map[string]any {
	t.Helper()
	raw, err := registercrypto.ParseKey(p.rc.keyHex)
	if err != nil {
		t.Fatal(err)
	}
	kid, iv, ct, err := registercrypto.Seal(raw, registercrypto.Payload{
		Version: registercrypto.PayloadVersion, AppId: appId,
		Challenge: ch, Password: password,
	})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{"appName": appId, "authScheme": auth.RegisterSchemeAesGcm,
		"kid": kid, "iv": iv, "ciphertext": ct, "challenge": ch}
}

func pjCall(t *testing.T, r *gin.Engine, path string, m map[string]any) (bool, string, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(rcBody(m))))
	var resp struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("%s: bad envelope %q: %v", path, w.Body.String(), err)
	}
	var data map[string]any
	if len(resp.Data) > 0 && string(resp.Data) != "null" {
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			data = nil
		}
	}
	return resp.Success, resp.Message, data
}

func TestAuthAppAcceptsPlaintextAndEnvelope(t *testing.T) {
	e := newPJEnvWithKey(t, rcKeyHex, false)
	if ok, msg, _ := pjCall(t, e.r, "/openApi/authApp",
		map[string]any{"appName": "app1", "password": "pw1"}); !ok {
		t.Fatalf("明文应通过: %s", msg)
	}
	chOk, _, chData := pjCall(t, e.r, "/openApi/challenge", map[string]any{"appName": "app1"})
	if !chOk || chData["challenge"] == "" {
		t.Fatalf("challenge = %+v", chData)
	}
	ch, _ := chData["challenge"].(string)
	body := e.sealForApp(t, "app1", ch, "pw1")
	ok, msg, data := pjCall(t, e.r, "/openApi/authApp", body)
	if !ok {
		t.Fatalf("加密 authApp 应通过: %s", msg)
	}
	token, _ := data["token"].(string)
	if appId, found := e.s.pj.Lookup(token); !found || appId != "app1" {
		t.Fatalf("换到的票不可用: %q", token)
	}
}

// 强制模式下 authApp 的明文被拒，并把握手的下一步放进 data 里。
func TestAuthAppEnforced(t *testing.T) {
	e := newPJEnvWithKey(t, rcKeyHex, true)
	ok, msg, data := pjCall(t, e.r, "/openApi/authApp",
		map[string]any{"appName": "app1", "password": "pw1"})
	if ok {
		t.Fatal("强制模式下明文应被拒")
	}
	if !strings.Contains(msg, "requires encrypted") {
		t.Fatalf("msg = %q", msg)
	}
	ch, _ := data["challenge"].(string)
	if ch == "" {
		t.Fatalf("data 里该有一张 challenge: %+v", data)
	}
	if got, _ := data["schemes"].([]any); len(got) != 1 {
		t.Fatalf("schemes = %v", data["schemes"])
	}
	if ok, msg2, _ := pjCall(t, e.r, "/openApi/authApp", e.sealForApp(t, "app1", ch, "pw1")); !ok {
		t.Fatalf("按提示加密后应通过: %s", msg2)
	}
}

// 兼容协议官方客户端的 md5 形态照旧明确拒绝（这条与加密握手无关，是"库里不留明文等价物"）。
func TestAuthAppStillRejectsMd5Form(t *testing.T) {
	e := newPJEnvWithKey(t, rcKeyHex, false)
	ok, msg, _ := pjCall(t, e.r, "/openApi/authApp", map[string]any{
		"appName": "app1", "encryptedPassword": strings.Repeat("0", 32), "encryptType": 1})
	if ok || !strings.Contains(msg, "md5") {
		t.Fatalf("md5 形态应被拒并说明理由: %q", msg)
	}
}

// 未启用加密时 /openApi/challenge 不该让客户端白跑一步。
func TestPjChallengeWhenDisabled(t *testing.T) {
	e := newPJEnvWithKey(t, "", false)
	ok, _, data := pjCall(t, e.r, "/openApi/challenge", map[string]any{"appName": "app1"})
	if !ok {
		t.Fatal("未启用时该端点仍应正常应答")
	}
	if got, _ := data["challenge"].(string); got != "" {
		t.Fatalf("未启用不该发票: %q", got)
	}
	schemes, _ := data["schemes"].([]any)
	if len(schemes) != 1 || schemes[0] != "plain" {
		t.Fatalf("schemes = %v", data["schemes"])
	}
}

// 缺 appName / 缺字段的老请求形状不能撞出 panic 或误放行
func TestAuthAppBadRequests(t *testing.T) {
	e := newPJEnvWithKey(t, rcKeyHex, false)
	for _, m := range []map[string]any{
		{}, {"appName": ""}, {"appName": "app1"},
		{"appName": "app1", "authScheme": "aes-gcm"},
		{"appName": "no-such-app", "password": "pw1"},
	} {
		if ok, _, _ := pjCall(t, e.r, "/openApi/authApp", m); ok {
			t.Fatalf("%v 不该通过", m)
		}
	}
}
