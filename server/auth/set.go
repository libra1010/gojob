// 三类令牌的装配：一个集群密钥 + 一份吊销基线，派生出控制台会话、worker 数据面、
// PowerJob OpenAPI 三种用途互不相通的无状态令牌。
package auth

import (
	"time"

	"gorm.io/gorm"
)

// TTL 约定（与引入集群前保持一致）：
//   - 控制台会话 12h，剩余不足一半时经响应头换新票（见 Manager.Renew）；
//   - worker 数据面 10min，无状态后不再滑动续期，由 SDK 过半主动重注册；
//   - OpenAPI 24h 绝对有效期：PowerJob 官方客户端会把 authApp 换到的票缓存整个进程生命周期，
//     给短了会让长期跑着的调用方在某一刻突然全 401；无状态票没法悄悄续，
//     所以既给足 24h，也在过半时经响应头续发一张新的（见 OpenAPITokens.RenewIfStale）。
const (
	ConsoleTokenTTL = 12 * time.Hour
	OpenAPITokenTTL = 24 * time.Hour
)

// Set 一套令牌体系。多副本必须用同一个 secret 构造，否则互相不认票。
type Set struct {
	Signer      *Signer
	Revocations *Revocations
	Revoked     *Revoked

	Console *Manager
	Worker  *WorkerTokens
	OpenAPI *OpenAPITokens
}

func NewSet(db *gorm.DB, secret string, consoleTTL, workerTTL, openapiTTL time.Duration) *Set {
	g := NewSigner(secret)
	rv := NewRevocations(db)
	revoked := NewRevoked(db)
	return &Set{
		Signer:      g,
		Revocations: rv,
		Revoked:     revoked,
		Console:     NewManager(g, rv, revoked, consoleTTL),
		Worker:      NewWorkerTokens(g, rv, workerTTL),
		OpenAPI:     NewOpenAPITokens(g, rv, openapiTTL),
	}
}

// OpenAPITokens PowerJob 兼容层的 app 级令牌（请求头 X-GoJob-Token / energy_job_schedule）。
type OpenAPITokens struct {
	g   *Signer
	rv  *Revocations
	ttl time.Duration
}

func NewOpenAPITokens(g *Signer, rv *Revocations, ttl time.Duration) *OpenAPITokens {
	if ttl <= 0 {
		ttl = OpenAPITokenTTL
	}
	return &OpenAPITokens{g: g, rv: rv, ttl: ttl}
}

// Issue 供 authApp 换票用；吊销基线走项目的 token_epoch。
func (o *OpenAPITokens) Issue(appId string) (string, error) {
	rev, err := o.rv.App(appId)
	if err != nil {
		return "", err
	}
	return o.g.Issue(PurposeOpenAPI, appId, "", rev, o.ttl), nil
}

// RenewIfStale 票剩余不足一半时续发一张新的（同代号），否则返回空串。
// 官方客户端不会读响应头，所以这只是给自研调用方的便利，不当它作依赖。
func (o *OpenAPITokens) RenewIfStale(c Claims) string {
	if c.Purpose != PurposeOpenAPI {
		return ""
	}
	if c.Expire-o.g.now().Unix() > int64(o.ttl.Seconds()/2) {
		return ""
	}
	return o.g.Issue(PurposeOpenAPI, c.Subject, "", c.Rev, o.ttl)
}

// ClaimsOf 解出载荷（供续期判断用）；无效票返回 ok=false。
func (o *OpenAPITokens) ClaimsOf(tk string) (Claims, bool) {
	c, err := o.g.Verify(tk, PurposeOpenAPI)
	if err != nil {
		return Claims{}, false
	}
	return c, true
}

// Lookup 返回令牌绑定的 app_id；无效/过期/已被吊销时 ok=false。
func (o *OpenAPITokens) Lookup(tk string) (string, bool) {
	c, err := o.g.Verify(tk, PurposeOpenAPI)
	if err != nil {
		return "", false
	}
	rev, err := o.rv.App(c.Subject)
	if err != nil || rev != c.Rev {
		return "", false
	}
	return c.Subject, true
}
