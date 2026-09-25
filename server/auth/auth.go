// Package auth 登录态与凭据：无状态签名令牌（控制台会话 / worker 数据面 / OpenAPI）、
// 基于数据库列的吊销基线、bcrypt 口令。
package auth

import (
	"log"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm/clause"

	"gojob/server/store"
)

// Manager 控制台会话：一张 Purpose=console 的签名令牌，绑定用户名。
//
// 与内存版会话表的行为差异都是"为了跨副本"而来的，逐条写清楚：
//   - 任何副本都能独立验签，登录不再挑节点；
//   - 滑动续期不再改服务端状态，而是剩余不足一半时经响应头发一张新票（见 RenewIfStale）；
//   - Revoke（退出登录）落黑名单表，跨副本生效；
//   - RevokeUser 从"删掉内存里的条目"改为把该用户的 token_epoch +1，
//     本机即刻生效、其他副本最多滞后一个吊销缓存周期。
type Manager struct {
	g       *Signer
	rv      *Revocations
	revoked *Revoked
	ttl     time.Duration
}

func NewManager(g *Signer, rv *Revocations, revoked *Revoked, ttl time.Duration) *Manager {
	if ttl <= 0 {
		ttl = ConsoleTokenTTL
	}
	return &Manager{g: g, rv: rv, revoked: revoked, ttl: ttl}
}

// TTL 会话有效期（绝对值）。
func (m *Manager) TTL() time.Duration { return m.ttl }

// Issue 为某用户签发一张会话票。读不到吊销基线（账号不存在/库故障）时返回错误，
// 因为一张无法校验的票发出去只会让用户在第一次请求时才发现没登录上。
func (m *Manager) Issue(username string) (string, error) {
	rev, err := m.rv.User(username)
	if err != nil {
		return "", err
	}
	return m.g.Issue(PurposeConsole, username, "", rev, m.ttl), nil
}

// Claims 验签一张会话票：签名、用途、过期、是否被登出，全部不查库。
// 返回的载荷里带着签发时的 token_epoch，交给调用方与它本来就要读的用户行比对
// ——这样"禁用账号/改密"的吊销在所有副本上都是即时的，也不为了校验多打一次库。
func (m *Manager) Claims(tk string) (Claims, bool) {
	c, err := m.g.Verify(tk, PurposeConsole)
	if err != nil {
		return Claims{}, false
	}
	if m.revoked.Has(c.Jti) {
		return Claims{}, false
	}
	return c, true
}

// SameUser 载荷是否属于这个用户。
func (c Claims) SameUser(username string) bool { return c.Subject == username }

// Expired 相对某个已过世的基线是否作废（用户行里的 token_epoch 变了）。
func (m *Manager) Stale(c Claims, currentEpoch int64) bool { return c.Rev != currentEpoch }

// RenewIfStale 会话票剩余不足一半时换发一张新的（沿用当前代号），否则返回空串。
// 旧票不拉黑：同一账号可能同时开着两个标签页，只把新票给正在用这张票的客户端。
func (m *Manager) RenewIfStale(c Claims, currentEpoch int64) string {
	if c.Purpose != PurposeConsole || c.Rev != currentEpoch {
		return "" // 代号已推进，续发只会得到一张同样作废的票
	}
	if c.Expire-m.g.now().Unix() > int64(m.ttl.Seconds()/2) {
		return "" // 还剩一半以上有效期，不必换票
	}
	return m.g.Issue(PurposeConsole, c.Subject, "", c.Rev, m.ttl)
}

// Revoke 作废单张票（退出登录）。写入黑名单直到它自然过期。
func (m *Manager) Revoke(tk string) {
	c, err := m.g.Verify(tk, PurposeConsole)
	if err != nil {
		return // 本来就无效的票不需要拉黑
	}
	if err := m.revoked.Add(c); err != nil {
		log.Printf("revoke session token: %v", err)
	}
}

// RevokeUser 吊销某用户名下全部会话（禁用账号、改密、删除时调用）。
func (m *Manager) RevokeUser(username string) {
	if err := m.rv.BumpUser(username); err != nil {
		log.Printf("revoke sessions of user %s: %v", username, err)
	}
}

// ---------- 用户口令 ----------

func HashPassword(plain string) (string, error) {
	bs, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(bs), err
}

func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ---------- 项目（app）口令 ----------
//
// 项目口令只有 bcrypt 一种形态，库里不存在任何明文或"明文等价物"（md5 摘要之类）。
// 这也意味着 PowerJob authApp 兼容层必须收到明文口令才能校验，见 api.pjAuthApp 的差异声明。

// IsPasswordHashed 判断存量值是否已是 bcrypt 形态，用于迁移幂等。
func IsPasswordHashed(s string) bool {
	return len(s) == 60 && (strings.HasPrefix(s, "$2a$") || strings.HasPrefix(s, "$2b$") ||
		strings.HasPrefix(s, "$2y$"))
}

// UpgradeAppPasswords 把历史明文项目密码原地升级为 bcrypt 密文。
// 已升级过的行跳过，可重复执行。
func UpgradeAppPasswords(st *store.Store) (int, error) {
	var apps []store.AppInfo
	if err := st.DB.Find(&apps).Error; err != nil {
		return 0, err
	}
	upgraded := 0
	for i := range apps {
		a := apps[i]
		if IsPasswordHashed(a.Password) {
			continue
		}
		hash, err := HashPassword(a.Password)
		if err != nil {
			return upgraded, err
		}
		if err := st.DB.Model(&store.AppInfo{}).Where("id = ?", a.ID).
			Updates(map[string]any{"password": hash}).Error; err != nil {
			return upgraded, err
		}
		upgraded++
	}
	return upgraded, nil
}

// SeedAdmin 无任何用户时播种管理员账号，返回是否新建。
// 多副本会同时首次启动，所以播种必须是"撞唯一键就算别人播过了"，而不是查了再插：
// 前者最多多一次失败 INSERT，后者会让落败的那个副本直接 Fatalf 退出（节点起不来）。
func SeedAdmin(st *store.Store, username, password string) (bool, error) {
	var n int64
	if err := st.DB.Model(&store.UserInfo{}).Count(&n).Error; err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return false, err
	}
	user := store.UserInfo{Username: username, Password: hash, Nickname: "管理员",
		Role: "SUPER_ADMIN", Status: "ACTIVE", AppScopes: "*",
		CreateTime: store.Now(), ModifyTime: store.Now()}
	res := st.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&user)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}
