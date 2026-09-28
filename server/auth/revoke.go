// 吊销基线与单票黑名单：无状态令牌的两条撤销通道。
package auth

import (
	"errors"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gojob/server/store"
)

// RevocationCacheTTL 吊销基线的进程内缓存时长。
// 没有这层缓存，每个请求都要为验签多读一次 app/user 行；有了它，
// 本机发起的吊销是即时的（改值时顺手清掉自己的缓存），跨副本收敛 ≤ 这个时长。
const RevocationCacheTTL = 5 * time.Second

// Revocations 读写 user_infos.token_epoch / app_infos.token_epoch。
type Revocations struct {
	db  *gorm.DB
	ttl time.Duration

	mu   sync.Mutex
	user map[string]revVal
	app  map[string]revVal
}

type revVal struct {
	epoch   int64
	readAt  time.Time
	missing bool // 查过、行不存在（删掉的账号/项目）：也要能被缓存住，否则每次都要打库
}

func NewRevocations(db *gorm.DB) *Revocations {
	return NewRevocationsTTL(db, RevocationCacheTTL)
}

func NewRevocationsTTL(db *gorm.DB, ttl time.Duration) *Revocations {
	if ttl <= 0 {
		ttl = RevocationCacheTTL
	}
	return &Revocations{db: db, ttl: ttl, user: map[string]revVal{}, app: map[string]revVal{}}
}

// ErrPrincipalGone 账号或项目已不存在，其令牌一律不再承认。
var ErrPrincipalGone = errors.New("account or app no longer exists")

// User 当前用户名下的会话代号。
func (r *Revocations) User(username string) (int64, error) {
	return r.read("user", username)
}

// App 某个项目的令牌代号。
func (r *Revocations) App(appId string) (int64, error) {
	return r.read("app", appId)
}

func (r *Revocations) read(kind, key string) (int64, error) {
	if v, ok := r.cached(kind, key); ok {
		if v.missing {
			return 0, ErrPrincipalGone
		}
		return v.epoch, nil
	}
	// 一条查询同时拿到代号和"主体还在不在"：Take 对零行返回 ErrRecordNotFound，
	// 而 Pluck 会静默给出零值，分不清"代号为 0"和"账号已被删"。
	var row struct{ TokenEpoch int64 }
	q := r.db.Model(&store.UserInfo{})
	if kind == "app" {
		q = r.db.Model(&store.AppInfo{})
	} else if kind != "user" {
		return 0, errors.New("revocations: unknown kind " + kind)
	}
	col, val := "username", key
	if kind == "app" {
		col = "app_id"
	}
	err := q.Select("token_epoch").Where(col+" = ?", val).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		r.put(kind, key, revVal{readAt: time.Now(), missing: true})
		return 0, ErrPrincipalGone
	}
	if err != nil {
		return 0, err
	}
	r.put(kind, key, revVal{epoch: row.TokenEpoch, readAt: time.Now()})
	return row.TokenEpoch, nil
}

func (r *Revocations) cached(kind, key string) (revVal, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.user
	if kind == "app" {
		m = r.app
	}
	v, ok := m[key]
	if !ok || time.Since(v.readAt) > r.ttl {
		return revVal{}, false
	}
	return v, true
}

func (r *Revocations) put(kind, key string, v revVal) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if kind == "app" {
		r.app[key] = v
	} else {
		r.user[key] = v
	}
}

// forget 本机改完代号后立刻丢缓存，让本机第一时间的下一次校验就落到新基线上。
func (r *Revocations) forget(kind, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if kind == "app" {
		delete(r.app, key)
	} else {
		delete(r.user, key)
	}
}

// BumpUser 吊销该用户的全部控制台会话（改密/重置密码/禁用/删除账号）。
func (r *Revocations) BumpUser(username string) error {
	return r.bump("user", username)
}

// BumpApp 吊销该项目的 worker token 与 OpenAPI token（改口令/删项目）。
func (r *Revocations) BumpApp(appId string) error {
	return r.bump("app", appId)
}

func (r *Revocations) bump(kind, key string) error {
	var res *gorm.DB
	switch kind {
	case "user":
		res = r.db.Model(&store.UserInfo{}).Where("username = ?", key).
			Updates(map[string]any{"token_epoch": gorm.Expr("token_epoch + 1"),
				"modify_time": store.Now()})
	case "app":
		res = r.db.Model(&store.AppInfo{}).Where("app_id = ?", key).
			Update("token_epoch", gorm.Expr("token_epoch + 1"))
	default:
		return errors.New("revocations: unknown kind " + kind)
	}
	r.forget(kind, key)
	return res.Error
}

// Revoked 单票黑名单（退出登录用）。行本身带过期时刻，过期后由维护循环删除，
// 所以这张表的大小只跟"近期登出数"有关，不会无界增长。
type Revoked struct {
	db *gorm.DB
}

func NewRevoked(db *gorm.DB) *Revoked { return &Revoked{db: db} }

// Add 拉黑一张票直到它自然过期。
func (rv *Revoked) Add(c Claims) error {
	return rv.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.TokenRevoked{
		Jti: c.Jti, Purpose: c.Purpose, Subject: c.Subject,
		ExpireAt: time.Unix(c.Expire, 0),
	}).Error
}

// Spend 把一张票的 jti 当场消费掉：返回 true 表示"这次是第一次"。
// 一次 INSERT + 主键冲突判定 = 跨副本原子的"一次性"凭证，所以一次性注册 challenge
// 也走这张表（它的真实语义是"已消费的 jti"，登出与 challenge 共用；两者都随自身过期被 Purge 掉）。
// 查库失败按"没消费成功"返回错误：宁可不放行，也不能让同一张 challenge 用两次。
func (rv *Revoked) Spend(c Claims) (bool, error) {
	if c.Jti == "" {
		return false, nil
	}
	res := rv.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&store.TokenRevoked{
		Jti: c.Jti, Purpose: c.Purpose, Subject: c.Subject,
		ExpireAt: time.Unix(c.Expire, 0),
	})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// Has 这张票是否被登出过。按用途限定：这张表现在同时容纳"登出票"和"已消费的一次性
// challenge"（见 Spend），不区分就可能让一次随机 jti 冲撞把某个正常会话误判成已登出。
// 查库失败时按"没被登出"处理并记日志：
// 拉黑只影响主动退出这一条路径，不该因为一次查询抖动把正常会话踢下线。
func (rv *Revoked) Has(jti, purpose string) bool {
	if jti == "" {
		return false
	}
	var n int64
	err := rv.db.Model(&store.TokenRevoked{}).Where("jti = ? AND purpose = ?", jti, purpose).Count(&n).Error
	if err != nil {
		log.Printf("revoked token lookup: %v", err)
		return false
	}
	return n > 0
}

// Purge 删掉已自然过期的黑名单行，返回清理条数。
func (rv *Revoked) Purge() (int64, error) {
	res := rv.db.Where("expire_at < ?", store.Now()).Delete(&store.TokenRevoked{})
	return res.RowsAffected, res.Error
}
