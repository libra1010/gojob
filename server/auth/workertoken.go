// Worker 数据面凭证：worker 用项目口令注册换取短周期 token，
// 此后拉任务/回报/传日志只带 token，不再反复携带项目口令（口令泄露面从每次请求缩小到一次注册）。
//
// 令牌是无状态签名的（见 token.go），所以任一副本都能校验任一 worker 的票——
// LB 后面不再出现"在 A 注册的票打到 B 就 401"。相应地有两处与内存版的差异：
//   - 服务端不再滑动续期，改由 SDK 在有效期过半时主动重注册（Go/Java 两侧同口径）；
//   - 重新注册不能凭"内存里删掉旧条目"来作废旧票，旧票在其自身有效期内仍可用心跳者身份。
//     真正需要即刻踢掉整个项目时用 RevokeApp（推进该项目的 token_epoch）。
package auth

import (
	"errors"
	"time"
)

var (
	// ErrTokenStale token 无效或已过期：worker 应重新注册（服务重启、密钥轮换、改口令都会走到这条）。
	ErrTokenStale = errors.New("worker token missing or expired, re-register")
	// ErrTokenImpersonated token 本身有效，但请求者自述的身份与签发时绑定的不一致：疑似冒用。
	ErrTokenImpersonated = errors.New("worker token does not match this app/instance")
)

// WorkerTokenTTL 默认有效期。worker 心跳（默认 5s）远快于它，SDK 会在过半时主动换票，
// 所以正常节点不会掉线；真正静默超过该时长或项目改口令后，票才失效。
const WorkerTokenTTL = 10 * time.Minute

// WorkerTokens 签发/校验 worker 数据面令牌（无状态，只读吊销基线）。
type WorkerTokens struct {
	g   *Signer
	rv  *Revocations
	ttl time.Duration
}

// NewWorkerTokens ttl<=0 时用 WorkerTokenTTL。
func NewWorkerTokens(g *Signer, rv *Revocations, ttl time.Duration) *WorkerTokens {
	if ttl <= 0 {
		ttl = WorkerTokenTTL
	}
	return &WorkerTokens{g: g, rv: rv, ttl: ttl}
}

// TTL 签发凭证的有效期，注册响应里回给 worker（SDK 据此安排主动换票）。
func (w *WorkerTokens) TTL() time.Duration { return w.ttl }

// Issue 为 (appId, instanceId) 签发一张 token。
// 读不到该项目的吊销基线（项目不存在/库故障）时返回错误，宁可不发也不能发一张验不过的票。
func (w *WorkerTokens) Issue(appId, instanceId string) (string, error) {
	rev, err := w.rv.App(appId)
	if err != nil {
		return "", err
	}
	return w.g.Issue(PurposeWorker, appId, instanceId, rev, w.ttl), nil
}

// Validate 校验 token 与请求者身份一致。
// 绑定的是"这张 token 只能由它签发对象的那个 app + instance_id 使用"，
// 因此拿到别人 token 的节点既领不到别人的任务，也改判不了别人的任务结果。
func (w *WorkerTokens) Validate(tk, appId, instanceId string) error {
	if tk == "" {
		return ErrTokenStale
	}
	c, err := w.g.Verify(tk, PurposeWorker)
	if err != nil {
		return ErrTokenStale
	}
	if c.Subject != appId || c.Scope != instanceId {
		return ErrTokenImpersonated
	}
	rev, err := w.rv.App(appId)
	if err != nil {
		return ErrTokenStale
	}
	if rev != c.Rev {
		return ErrTokenStale // 改过口令/删过项目：整个项目的票一起失效
	}
	return nil
}

// RevokeApp 吊销某项目全部 worker token 与 OpenAPI token（改口令、删项目时调用）。
// 推进的是库里的代号，所以对所有副本同时生效（本机即刻，其余一个缓存周期内）。
func (w *WorkerTokens) RevokeApp(appId string) error {
	return w.rv.BumpApp(appId)
}
