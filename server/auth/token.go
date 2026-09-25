// 无状态签名令牌：把"这张票是谁的、到什么时候、按哪个吊销基线签发"都写进载荷，
// 用集群共享密钥做 HMAC-SHA256 签名。任何副本都能独立验签，不需要共享会话存储
// ——这是多副本下控制台不再随机掉登录、worker 不再随机 401 的前提。
//
// 代价（如实声明）：无状态就意味着"不能单独作废某一张票"。吊销靠两条：
//  1. 吊销基线（token_epoch）：改项目口令 / 改密 / 禁用账号时把库里的代号 +1，
//     此前签发的全部令牌即刻不被承认（各副本读同一列，见 revoke.go）；
//  2. 单票黑名单：只有"主动退出登录"这一种场景需要精确到票，落一张小表（见 Revoked）。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	v2 "encoding/json/v2"
)

// 令牌用途分域：控制台会话票不能拿去领任务，worker 票也不能打开管理面。
// 用途写进载荷并参与签名比对，跨域使用在 Verify 阶段就被拒。
const (
	PurposeConsole = "console" // 控制台用户
	PurposeWorker  = "worker"  // worker 数据面，绑定 app_id + instance_id
	PurposeOpenAPI = "openapi" // PowerJob 兼容层 authApp 换的票
)

// RenewedHeader 会话续期响应头：前端拦截器发现该头就替换本地存的票。
// 无状态令牌的"滑动续期"只能这么表达——服务端没有可改的状态，改的是客户端手里那张。
const RenewedHeader = "X-GoJob-Token-Renewed"

var (
	ErrTokenInvalid = errors.New("token invalid")
	ErrTokenExpired = errors.New("token expired")
	ErrTokenPurpose = errors.New("token issued for a different purpose")
)

// Claims 载荷。Subject/Scope 的含义随 Purpose 变化：
// console: Subject=username；worker: Subject=app_id, Scope=instance_id；openapi: Subject=app_id。
type Claims struct {
	Purpose string `json:"p"`
	Subject string `json:"s"`
	Scope   string `json:"o,omitempty"`
	Issued  int64  `json:"i"`
	Expire  int64  `json:"e"`
	// Rev 签发时的吊销基线（token_epoch）。库里这一列变了，这张票即刻作废。
	Rev int64 `json:"r"`
	// JTI 唯一标识，只为了"退出登录能精确废掉这一张"。
	Jti string `json:"j"`
}

// Signer 用一个集群密钥签发/验签。多副本必须配同一个 GOJOB_CLUSTER_SECRET。
type Signer struct {
	secret []byte
	now    func() time.Time // 测试注入
	// Ephemeral 密钥是本进程临时生成的（未配置集群密钥），单副本可用、多副本会互不认票。
	Ephemeral bool
}

// NewSigner secret 为空时生成一个进程内随机密钥并标记 Ephemeral：
// 这样单机开发不必先配密钥，而多副本部署必须显式配置才能互认。
func NewSigner(secret string) *Signer {
	if strings.TrimSpace(secret) == "" {
		// 每次调用各自生成：随机密钥的意义就是"只有本进程认得"，共享出去就名不副实了。
		return &Signer{secret: randSecret(), Ephemeral: true, now: time.Now}
	}
	sum := sha256.Sum256([]byte(secret))
	return &Signer{secret: sum[:], now: time.Now}
}

// SetClock 注入时间源，供测试在不等待有效期的情况下让令牌过期。
func (g *Signer) SetClock(f func() time.Time) { g.now = f }

func randSecret() []byte {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil { // crypto/rand
		panic("auth: rand unavailable: " + err.Error())
	}
	return buf
}

func newJti() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("auth: rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// Issue 签发一张有效期为 ttl 的令牌（自动补 Jti）。
func (g *Signer) Issue(purpose, subject, scope string, rev int64, ttl time.Duration) string {
	c := Claims{Purpose: purpose, Subject: subject, Scope: scope, Rev: rev, Jti: newJti()}
	return g.IssueClaims(c, ttl)
}

// IssueClaims 用调用方给定的载荷签发（需要复用 Jti 之类的场景用）。
func (g *Signer) IssueClaims(c Claims, ttl time.Duration) string {
	now := g.now()
	c.Issued = now.Unix()
	if c.Expire == 0 {
		c.Expire = now.Add(ttl).Unix()
	}
	if c.Jti == "" {
		c.Jti = newJti()
	}
	body, err := v2.Marshal(c)
	if err != nil {
		return ""
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	return payload + "." + g.sign(payload)
}

// Verify 验签并检查过期与用途，不查库（吊销基线的核对由各 Manager 负责）。
func (g *Signer) Verify(tk, purpose string) (Claims, error) {
	payload, sig, ok := strings.Cut(tk, ".")
	if !ok || payload == "" || sig == "" {
		return Claims{}, ErrTokenInvalid
	}
	if !hmac.Equal([]byte(g.sign(payload)), []byte(sig)) {
		return Claims{}, ErrTokenInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Claims{}, ErrTokenInvalid
	}
	var c Claims
	if err := v2.Unmarshal(body, &c); err != nil {
		return Claims{}, ErrTokenInvalid
	}
	if c.Purpose != purpose {
		return Claims{}, ErrTokenPurpose
	}
	if g.now().Unix() >= c.Expire {
		return Claims{}, ErrTokenExpired
	}
	return c, nil
}

func (g *Signer) sign(payload string) string {
	mac := hmac.New(sha256.New, g.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
