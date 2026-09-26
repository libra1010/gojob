// SDK 侧的加密握手：配了 AesKey 之后，口令不该再出现在链路上，
// 而"两侧只配了一边"必须报得清楚——这是本功能最容易踩到的运维坑。
//
// 这里的服务端是假的（SDK 不能反过来依赖 server/ 包），但它按同一份线格式实现解密，
// 所以 Go SDK 的字节口径在服务端测试之外又被独立对了一次。
package worker

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/internal/registercrypto"
)

const sdkKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

// 与 auth.RegisterScheme* 同值。值本身是协议的一部分，所以 SDK 测试里写死而不是引 server 包。
const (
	schemePlain  = "plain"
	schemeAesGcm = "aes-gcm"
)

type encServer struct {
	t       *testing.T
	keyHex  string // 空 = 未启用注册加密
	enforce bool

	mu        sync.Mutex
	issued    int
	registers int
	valid     map[string]bool // 当前有效的数据面 token（清空 = 服务重启）
	used      map[string]bool // 已消费的 challenge（服务端语义：一次性）
	decoded   []registercrypto.Payload
	plainGot  []string // 收到过明文口令的注册请求
}

func newEncServer(t *testing.T, keyHex string, enforce bool) *encServer {
	return &encServer{t: t, keyHex: keyHex, enforce: enforce,
		valid: map[string]bool{}, used: map[string]bool{}}
}

func (f *encServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bs, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Fatalf("read %s: %v", r.URL.Path, err)
	}
	var generic map[string]any
	if err := v2.Unmarshal(bs, &generic); err != nil {
		f.t.Fatalf("decode %s (%s): %v", r.URL.Path, bs, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(code int, msg string, data any) {
		out, err := v2.Marshal(protocol.Resp[any]{Code: code, Message: msg, Data: data})
		if err != nil {
			f.t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}
	newChallenge := func() string {
		f.issued++
		return fmt.Sprintf("ch-%d", f.issued)
	}
	switch r.URL.Path {
	case "/worker/challenge":
		if f.keyHex == "" {
			write(protocol.CodeBadReq,
				"本服务端未启用注册加密（GOJOB_WORKER_AES_KEYS 未配置），请直接明文注册", nil)
			return
		}
		schemes := []string{schemePlain, schemeAesGcm}
		if f.enforce {
			schemes = []string{schemeAesGcm}
		}
		write(protocol.CodeOK, "", protocol.ChallengeResp{
			Challenge: newChallenge(), ExpiresIn: 30, Schemes: schemes})

	case "/worker/register":
		var req protocol.RegisterReq
		if err := v2.Unmarshal(bs, &req); err != nil {
			f.t.Fatalf("decode register: %v", err)
		}
		switch req.AuthScheme {
		case "", schemePlain:
			f.plainGot = append(f.plainGot, req.Password)
			if f.enforce {
				write(protocol.CodeChallengeRequired, "this deployment requires encrypted registration",
					protocol.ChallengeResp{Challenge: newChallenge(), ExpiresIn: 30,
						Schemes: []string{schemeAesGcm}})
				return
			}
			if req.Password != "pw1" || req.AppId != "app1" {
				write(protocol.CodeAuthErr, "register rejected: password mismatch", nil)
				return
			}
		case schemeAesGcm:
			if f.keyHex == "" {
				write(protocol.CodeAuthErr,
					"server has no worker AES key configured, plaintext registration only", nil)
				return
			}
			if req.Challenge == "" || f.used[req.Challenge] {
				write(protocol.CodeAuthErr, "challenge invalid, expired, or already used", nil)
				return
			}
			key, err := registercrypto.ParseKey(f.keyHex)
			if err != nil {
				f.t.Fatal(err)
			}
			p, err := registercrypto.Open(key, req.Kid, req.AppId, req.Iv, req.Ciphertext)
			if err != nil {
				write(protocol.CodeAuthErr, "register rejected: "+err.Error(), nil)
				return
			}
			if p.InstanceId != req.InstanceId || p.Challenge != req.Challenge {
				write(protocol.CodeAuthErr, "register rejected: payload mismatch", nil)
				return
			}
			f.used[req.Challenge] = true
			f.decoded = append(f.decoded, p)
			if p.Password != "pw1" {
				write(protocol.CodeAuthErr, "register rejected: password mismatch", nil)
				return
			}
		default:
			write(protocol.CodeBadReq, fmt.Sprintf("unknown auth_scheme %q", req.AuthScheme), nil)
			return
		}
		f.registers++
		tk := fmt.Sprintf("tk-%d", f.registers)
		f.valid[tk] = true
		write(protocol.CodeOK, "", protocol.RegisterResp{Token: tk, TtlSeconds: 600})

	default: // 数据面：只认最近签发的票
		tk, _ := generic["token"].(string)
		if !f.valid[tk] || generic["app_id"] != "app1" {
			write(protocol.CodeAuthErr, "worker token missing or expired, re-register", nil)
			return
		}
		write(protocol.CodeOK, "", protocol.CancelTasks{})
	}
}

// invalidate 模拟服务重启：票表清空。
func (f *encServer) invalidate() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.valid = map[string]bool{}
}

func (f *encServer) counts() (issued, registers int, plain []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issued, f.registers, append([]string(nil), f.plainGot...)
}

func (f *encServer) decrypted() []registercrypto.Payload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]registercrypto.Payload(nil), f.decoded...)
}

func sdkWorker(url, aesKey string) *Worker {
	return New(Config{ServerAddress: url, AppId: "app1", Password: "pw1",
		Address: "10.0.0.1:7799", AesKey: aesKey})
}

func TestSDKEncryptedHandshakeKeepsPasswordOffWire(t *testing.T) {
	f := newEncServer(t, sdkKeyHex, false)
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := sdkWorker(srv.URL, sdkKeyHex)

	if err := w.register(t.Context()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if w.currentToken() != "tk-1" {
		t.Fatalf("token = %q", w.currentToken())
	}
	issued, registers, plain := f.counts()
	if issued != 1 || registers != 1 {
		t.Fatalf("issued=%d registers=%d, want 1/1", issued, registers)
	}
	if len(plain) != 0 {
		t.Fatalf("注册请求带了明文口令: %v", plain)
	}
	got := f.decrypted()
	if len(got) != 1 {
		t.Fatalf("decoded = %+v", got)
	}
	if got[0].Password != "pw1" || got[0].AppId != "app1" ||
		got[0].InstanceId != w.instanceID || got[0].Version != registercrypto.PayloadVersion {
		t.Fatalf("解出的载荷 = %+v", got[0])
	}
	if got[0].Challenge != "ch-1" {
		t.Fatalf("challenge = %q, want 刚领到的那张", got[0].Challenge)
	}
}

// 每次注册都重新领 challenge、重新随机 iv ⇒ 同一个口令两次注册的密文互不相同。
func TestSDKReRegistersWithFreshChallenge(t *testing.T) {
	f := newEncServer(t, sdkKeyHex, false)
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := sdkWorker(srv.URL, sdkKeyHex)
	ctx := t.Context()
	for range 3 {
		if err := w.register(ctx); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	issued, registers, _ := f.counts()
	if issued != 3 || registers != 3 {
		t.Fatalf("issued=%d registers=%d, want 3/3", issued, registers)
	}
	seen := map[string]bool{}
	for _, p := range f.decrypted() {
		if seen[p.Challenge] {
			t.Fatalf("challenge 被重复使用: %s", p.Challenge)
		}
		seen[p.Challenge] = true
	}
}

// 自愈路径也要走握手：票被作废后重注册不能退回明文。
func TestSDKSelfHealKeepsEncryption(t *testing.T) {
	f := newEncServer(t, sdkKeyHex, false)
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := sdkWorker(srv.URL, sdkKeyHex)
	ctx := t.Context()
	if err := w.register(ctx); err != nil {
		t.Fatal(err)
	}
	f.invalidate()
	if _, err := call[protocol.CancelTasks](w, ctx, "/worker/heartbeat", beatBody(w.instanceID)); err != nil {
		t.Fatalf("自愈失败: %v", err)
	}
	if _, n, plain := f.counts(); n != 2 || len(plain) != 0 {
		t.Fatalf("registers=%d plain=%v, want 2 次注册且都不带明文", n, plain)
	}
}

// 服务端没启用加密 ⇒ 配了 key 的节点明确失败，绝不静默退回明文
// （静默降级等于给链路上的中间人一个"抹掉加密"的开关）。
func TestSDKDoesNotSilentlyDowngrade(t *testing.T) {
	f := newEncServer(t, "", false)
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := sdkWorker(srv.URL, sdkKeyHex)

	err := w.Run(t.Context())
	if err == nil {
		t.Fatal("服务端没配 key 时，配了 key 的节点应注册失败")
	}
	if !strings.Contains(err.Error(), "GOJOB_WORKER_AES_KEYS") {
		t.Fatalf("err = %v, want 点名服务端配置项", err)
	}
	if _, n, plain := f.counts(); n != 0 || len(plain) != 0 {
		t.Fatalf("不该有一次明文注册成功: registers=%d plain=%v", n, plain)
	}
}

// 服务端强制而本节点没配 key：报错要指到 worker 自己的配置上。
func TestSDKReportsMissingKeyWhenEnforced(t *testing.T) {
	f := newEncServer(t, sdkKeyHex, true)
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := sdkWorker(srv.URL, "")

	err := w.register(t.Context())
	if err == nil {
		t.Fatal("强制模式下没配 key 应失败")
	}
	if !strings.Contains(err.Error(), "AesKey") {
		t.Fatalf("err = %v, want 指向 worker.Config.AesKey", err)
	}
	// 配上 key 之后同一条链路立刻能注册
	if err := sdkWorker(srv.URL, sdkKeyHex).register(t.Context()); err != nil {
		t.Fatalf("配了 key 后应成功: %v", err)
	}
}

// key 写成非法 hex 要在注册这一刻报出来，而不是解出一堆垃圾再报"口令错"。
func TestSDKRejectsMalformedKey(t *testing.T) {
	f := newEncServer(t, sdkKeyHex, false)
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := sdkWorker(srv.URL, "not-hex")

	err := w.register(t.Context())
	if err == nil || !strings.Contains(err.Error(), "Config.AesKey") {
		t.Fatalf("err = %v, want 指出 AesKey 配置有误", err)
	}
	if issued, n, _ := f.counts(); issued != 0 || n != 0 {
		t.Fatalf("配置非法时不该去打扰服务端: issued=%d registers=%d", issued, n)
	}
}

// 口令错了：加密路径上仍然是一次正常的注册失败，错误原样抛给调用方。
func TestSDKWrongPasswordStillFails(t *testing.T) {
	f := newEncServer(t, sdkKeyHex, false)
	srv := httptest.NewServer(f)
	defer srv.Close()
	w := sdkWorker(srv.URL, sdkKeyHex)
	w.cfg.Password = "wrong"

	err := w.register(t.Context())
	if err == nil || !strings.Contains(err.Error(), "password mismatch") {
		t.Fatalf("err = %v, want 服务端原样的口令错误", err)
	}
}
