// 注册口令加密的服务端一侧：一次性 challenge + AES-256-GCM。
// 字节口径（HKDF salt、AAD、iv/tag 摆放）只有一份实现，在 internal/registercrypto
// ——Go SDK 与服务端共用，Java SDK 由那边的 golden 向量钉住。这里只管"接受哪些 key、
// challenge 能不能用、这次注册还原成什么口令"。
//
// 目标窄得很明确：**让项目口令本身不出现在链路上**，并且让抓到一个注册包的人无法重用它。
// 它不是传输加密——链路仍是 HTTP 明文，任务参数与日志照旧明文，那些要靠内网或前置 TLS。
// 把这段当成"开了加密就安全了"是误用。
//
// 三件东西各管一件事，缺一个整套就白做：
//   - challenge：服务端现发、30s 过期、**一次性**（消费记录落在 revoked_tokens 那张表里，
//     主键冲突即"已被用过"，所以跨副本也认）。固定字符串不行：固定因子 + 固定 key
//     会得到确定性的密文，那串密文就成了口令的替身，抓到即可永久冒用。
//   - AES-256-GCM：CBC 没有完整性，解出垃圾时分不清"key 错/被改过/口令错"，
//     把这几类返成不同文案就是 padding oracle 的入口；GCM 的 tag 校验失败只有一个结局。
//   - 明文里带 app_id/instance_id/challenge，且 AAD 绑 version+kid+app_id：
//     "拿 A 项目算好的密文配着 B 项目的表头来注册"、换 kid 冒用，都会当场认证失败。
package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gojob/internal/registercrypto"
)

// 注册口令的两种送达方式。
const (
	RegisterSchemePlain  = registercrypto.SchemePlain  // 明文口令（未配置 AES key 时的唯一方式）
	RegisterSchemeAesGcm = registercrypto.SchemeAesGcm // 一次性 challenge + AES-256-GCM
)

// ChallengeTTL 一次性 challenge 的有效期：够一次注册往返，又短到"抓包重放"没有可用窗口。
const ChallengeTTL = 30 * time.Second

var (
	// ErrServerNoKey 服务端没配 key 却收到加密注册——不能报成"口令错误"，
	// 那会把排障的人引去核对口令，而真正没对齐的是配置。
	ErrServerNoKey = errors.New("server has no worker AES key configured, plaintext registration only")
	// ErrEncryptionRequired 服务端已强制加密，而客户端发的是明文。
	ErrEncryptionRequired = errors.New("this deployment requires encrypted registration")
	// ErrChallengeBad challenge 不是本服务端签的、已过期、或已被用过。
	ErrChallengeBad = errors.New("challenge invalid, expired, or already used")
	// errUnknownKid kid 不在接受集合里：多半是两侧 key 不一致或轮换期间旧 key 已被摘掉。
	errUnknownKid = errors.New("unknown key id")
	// errPayloadMismatch 密文里绑的 instance_id 与请求外层不一致。
	errPayloadMismatch = errors.New("encrypted payload does not match instance_id")
)

// KeyRing 服务端接受的一组注册 key（多把是为了轮换：新旧并行期内两边的 worker 都能注册）。
type KeyRing struct {
	keys    map[string][]byte
	kids    []string // 稳定顺序，只用于错误提示与启动日志
	enforce bool
}

// ParseKeyRing 解析 GOJOB_WORKER_AES_KEYS：逗号分隔的 64 位 hex（32 字节 AES-256 key）。
// enforce=true 时明文注册一律拒（"配了就要客户端也必须"这条由运维在两侧对齐）。
func ParseKeyRing(spec string, enforce bool) (*KeyRing, error) {
	ring := &KeyRing{keys: map[string][]byte{}, enforce: enforce}
	for item := range strings.SplitSeq(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		raw, err := registercrypto.ParseKey(item)
		if err != nil {
			return nil, fmt.Errorf("GOJOB_WORKER_AES_KEYS 每一项需为 64 位 hex（AES-256）：%q", firstN(item, 8))
		}
		kid := registercrypto.KeyId(raw)
		if _, dup := ring.keys[kid]; !dup {
			ring.keys[kid] = raw
			ring.kids = append(ring.kids, kid)
		}
	}
	return ring, nil
}

func (r *KeyRing) Enabled() bool  { return r != nil && len(r.keys) > 0 }
func (r *KeyRing) Enforce() bool  { return r != nil && r.enforce }
func (r *KeyRing) Kids() []string { return r.kids }

// open 按 kid 挑 key，再交给统一的线格式实现解密。未知 kid 单独一条文案（它指向配置错配，
// 而解密失败那条故意含糊——含糊是为了不给 oracle，不是为了不给线索）。
func (r *KeyRing) open(kid, appId, ivB64, ctB64 string) (registercrypto.Payload, error) {
	raw, ok := r.keys[kid]
	if !ok {
		return registercrypto.Payload{}, fmt.Errorf("%w: %s（服务端接受 %s）", errUnknownKid, kid,
			strings.Join(r.kids, ","))
	}
	return registercrypto.Open(raw, kid, appId, ivB64, ctB64)
}

// Challenges 一次性 challenge：签名令牌（见 token.go 的 PurposeChallenge）+ 消费记录。
type Challenges struct {
	g   *Signer
	sp  *Revoked
	ttl time.Duration
}

func NewChallenges(g *Signer, sp *Revoked) *Challenges {
	return &Challenges{g: g, sp: sp, ttl: ChallengeTTL}
}

// Issue 签发 challenge。principal 是 worker 的 instance_id（OpenAPI 侧传空）。
// 故意**不查项目是否存在**、也不带吊销基线：这一步对任何人开放，
// 项目存不存在只在真正注册那一刻才看得出来（否则这里就成了一台 app_id 枚举机）。
func (c *Challenges) Issue(appId, principal string) string {
	return c.g.IssueClaims(Claims{
		Purpose: PurposeChallenge, Subject: appId, Scope: principal, Rev: 0,
	}, c.ttl)
}

// Redeem 校验并当场消费一个 challenge。一次性靠 revoked_tokens 的主键冲突实现：
// 插入成功=第一次用，冲突=已经被用过了（在任何副本上用的都算，因为记录在库里）。
func (c *Challenges) Redeem(tk, appId, principal string) error {
	if tk == "" {
		return ErrChallengeBad
	}
	claims, err := c.g.Verify(tk, PurposeChallenge)
	if err != nil {
		return ErrChallengeBad
	}
	if claims.Subject != appId || claims.Scope != principal {
		return ErrChallengeBad
	}
	ok, err := c.sp.Spend(claims)
	if err != nil {
		return err
	}
	if !ok {
		return ErrChallengeBad
	}
	return nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// RegisterCrypto 注册口令加密的装配：密钥环 + 一次性 challenge。
// 没配 key 时它是 nil，注册行为与引入本功能之前完全一致（明文口令）。
type RegisterCrypto struct {
	Ring *KeyRing
	Chal *Challenges
}

func (rc *RegisterCrypto) Enabled() bool { return rc != nil && rc.Ring.Enabled() }

// Enforce 是否只接受加密注册。nil 接收者不强制（功能未开）。
func (rc *RegisterCrypto) Enforce() bool { return rc != nil && rc.Ring.Enforce() }

// Kids 服务端接受的 key 指纹，只用于启动日志。kid 本来就在每次注册请求里传，列出来
// 只为让运维核对"两侧配的是不是同一把"。
func (rc *RegisterCrypto) Kids() []string {
	if rc == nil {
		return nil
	}
	return rc.Ring.Kids()
}

// Schemes 服务端当前接受的注册方式。回给客户端是为了让错配报得出原因——
// "两侧只配了一把 key 的另一把"这类问题，含混的 401 是查不出来的。
func (rc *RegisterCrypto) Schemes() []string {
	switch {
	case !rc.Enabled():
		return []string{RegisterSchemePlain}
	case rc.Enforce():
		return []string{RegisterSchemeAesGcm}
	default:
		return []string{RegisterSchemePlain, RegisterSchemeAesGcm}
	}
}

// Clear 一次注册里"口令是怎么送上来的"。
type Clear struct {
	Scheme, Kid, Iv, Ciphertext, Challenge, Password string
}

// ClearOutcome 还原结果，三态互斥：
//   - OK：Password 就是可用口令；
//   - Challenge 非空：这次发的是明文而服务端要求加密，把这张现领的 challenge 一起回给
//     客户端，它能省一次往返直接按 aes-gcm 重来；Err 说明为什么；
//   - 只有 Err：明确拒绝（原因见各 Err 的注释，都不写成"口令错误"）。
type ClearOutcome struct {
	OK        bool
	Password  string
	Challenge string
	Err       error
}

// Clear 把注册请求还原成明文口令。principal 是 worker 的 instance_id（OpenAPI 侧传 ""，
// 那边没有实例身份，密文里的 instance_id 就必须是空）。
//
// 顺序有讲究：**先消费 challenge 再解密**。反过来做的话，一张 challenge 就能被拿去
// 无限次试不同的 key；先消费则每次尝试都要先领一张新的，而领到用掉只有一次机会——
// 猜错 key 的代价从"无限"变成"每次都要留下一条消费记录"。
func (rc *RegisterCrypto) Clear(appId, principal string, in Clear) ClearOutcome {
	switch scheme := strings.ToLower(strings.TrimSpace(in.Scheme)); scheme {
	case "", RegisterSchemePlain:
		if rc.Enforce() {
			return ClearOutcome{Challenge: rc.Chal.Issue(appId, principal), Err: ErrEncryptionRequired}
		}
		return ClearOutcome{OK: true, Password: in.Password}
	case RegisterSchemeAesGcm:
	default:
		return ClearOutcome{Err: fmt.Errorf("unknown auth_scheme %q", in.Scheme)}
	}
	if !rc.Enabled() {
		return ClearOutcome{Err: ErrServerNoKey}
	}
	if in.Challenge == "" || in.Kid == "" || in.Iv == "" || in.Ciphertext == "" {
		return ClearOutcome{Err: errors.New("aes-gcm registration needs challenge, kid, iv and ciphertext")}
	}
	if err := rc.Chal.Redeem(in.Challenge, appId, principal); err != nil {
		return ClearOutcome{Err: err}
	}
	p, err := rc.Ring.open(in.Kid, appId, in.Iv, in.Ciphertext)
	if err != nil {
		return ClearOutcome{Err: err}
	}
	if p.InstanceId != principal {
		return ClearOutcome{Err: errPayloadMismatch}
	}
	return ClearOutcome{OK: true, Password: p.Password}
}

// Seal 用一把 key 加密注册载荷。给测试与自研 OpenAPI 调用方用；
// 生产上的 worker 由各自 SDK 完成同一步（同一份线格式实现）。
func Seal(keyHex string, p registercrypto.Payload) (kid, iv, ct string, err error) {
	raw, err := registercrypto.ParseKey(keyHex)
	if err != nil {
		return "", "", "", err
	}
	return registercrypto.Seal(raw, p)
}
