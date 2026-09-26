// Package cluster 让多个 gojob-server 副本安全地跑同一套库。
//
// 归属模型与兼容协议一致：**按项目（app）划分调度所有权**，同一时刻只有一个副本调度某个 app
// 的全部任务（兼容协议用 app_info.current_server 表达，见其 ServerElectionService 与
// "select id from AppInfoDO where currentServer = :currentServer"）。
// 有两处刻意的偏离：
//  1. 兼容协议靠 worker 心跳反向 PING 旧主判活（它有 server→worker RPC），我们是 pull 模型，
//     只能用**租约 TTL**判活，所以接管时延是显式可算的（LeaseTTL + 一跳）。
//  2. 兼容协议明确没解决"网络分区后旧主仍在写"（其选举是先到先得、无 fencing token），
//     这里用 epoch + 条件 UPDATE 把它堵死：旧主哪怕还活着，写库也会被拒。
package cluster

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"gojob/server/store"
)

const (
	// LeaseTTL 租约有效期。持有者必须在一半时间内续上，超过 TTL 才可被别的副本接管。
	LeaseTTL = 10 * time.Second
	// RenewInterval 续约周期。TTL/3 留了两次丢跳的余量（一次 GC 停顿或一次慢查询不至于丢主）。
	RenewInterval = 3 * time.Second
)

// Leases 本副本的项目调度归属账本：内存里缓存"我现在持有 which apps + 什么 epoch"，
// 真身在 DB 的 app_leases 表里（所以崩溃恢复无需任何本地状态）。
type Leases struct {
	db  *gorm.DB
	me  string
	ttl time.Duration

	mu    sync.RWMutex
	owned map[string]int64 // appId -> 我持有它时的 epoch；fencing 写入要用
	// onLost 归属被夺走时的回调（调度器用来立刻撤掉该 app 的挂轮，不等下一轮粗扫）。
	onLost func(appId string)

	now func() time.Time // 测试注入
}

func NewLeases(db *gorm.DB, me string, ttl time.Duration) *Leases {
	if ttl <= 0 {
		ttl = LeaseTTL
	}
	return &Leases{db: db, me: me, ttl: ttl, owned: make(map[string]int64), now: time.Now}
}

// OnLost 注册归属丢失回调。必须在 Run 之前设置。
func (l *Leases) OnLost(fn func(appId string)) { l.onLost = fn }

// SetClock 注入时间源，供测试在不等待 TTL 的情况下推进租约到期。
func (l *Leases) SetClock(f func() time.Time) { l.now = f }

// Me 本副本的集群身份（node_id）。
func (l *Leases) Me() string { return l.me }

// TTL 租约有效期，供接管时延的文档/页面口径引用。
func (l *Leases) TTL() time.Duration { return l.ttl }

// Owned 当前持有的 appId（排序后返回，便于日志与断言稳定）。
func (l *Leases) Owned() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.owned))
	for a := range l.owned {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Owns 是否持有该项目（调度器装载与触发前的准入判断）。
func (l *Leases) Owns(appId string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.owned[appId]
	return ok
}

// Epoch 返回本副本持有该项目时的 fencing epoch。
func (l *Leases) Epoch(appId string) (int64, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	ep, ok := l.owned[appId]
	return ep, ok
}

// Fence 在**写库事务内**确认"我仍是该项目的持有者且 epoch 未变"。
// 返回 false 表示归属已易主（本副本是僵尸主），调用方必须回滚本次写入。
//
// 为什么用 renew_count 而不是直接改 lease_expire：mysql 的 affected rows 只算"真的改了值"，
// 同一毫秒内的幂等改写会返回 0，从而把正常主误判成僵尸。自增列永远在变，RowsAffected 恒为 1。
func (l *Leases) Fence(tx *gorm.DB, appId string) bool {
	l.mu.RLock()
	ep, ok := l.owned[appId]
	l.mu.RUnlock()
	if !ok {
		return false
	}
	res := tx.Model(&store.AppLease{}).
		Where("app_id = ? AND holder = ? AND epoch = ?", appId, l.me, ep).
		Updates(map[string]any{
			"renew_count": gorm.Expr("renew_count + 1"),
			"modify_time": l.now(),
		})
	if res.Error != nil {
		// 查不出归属时一律按"不是我的"处理：宁可少跑一次触发，也不能双跑。
		log.Printf("lease fence check failed for app %s: %v", appId, res.Error)
		return false
	}
	if res.RowsAffected == 1 {
		return true
	}
	l.drop(appId, "fenced out")
	return false
}

// Run 续约循环，直到 ctx 结束。启动即跳一次，避免首跳等待一个周期。
func (l *Leases) Run(ctx context.Context) {
	l.Sync()
	t := time.NewTicker(RenewInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.releaseAll()
			return
		case <-t.C:
			l.Sync()
		}
	}
}

// releaseAll 优雅下线时主动交出归属（把 lease_expire 推到当下），
// 让接管方不必等满 TTL。进程被 kill 时走不到这里，那种情况由 TTL 兜底。
func (l *Leases) releaseAll() {
	apps := l.Owned()
	if len(apps) == 0 {
		return
	}
	if err := l.db.Model(&store.AppLease{}).Where("holder = ?", l.me).
		Updates(map[string]any{"lease_expire": l.now(), "modify_time": l.now()}).Error; err != nil {
		log.Printf("lease release on shutdown: %v", err)
		return
	}
	l.mu.Lock()
	l.owned = make(map[string]int64)
	l.mu.Unlock()
	log.Printf("gojob-server released %d app lease(s) on shutdown", len(apps))
}

// Sync 一跳：续约已持有的、抢占空闲的、交出已消失的。
// 导出给测试直接驱动（不依赖定时器就能断言状态机）。
func (l *Leases) Sync() {
	apps, err := l.appIds()
	if err != nil {
		log.Printf("lease: list apps: %v", err)
		return
	}
	leases, err := l.rows()
	if err != nil {
		log.Printf("lease: list leases: %v", err)
		return
	}
	now := l.now()
	alive := make(map[string]bool, len(apps))
	for _, a := range apps {
		alive[a] = true
	}
	for _, a := range l.Owned() {
		switch {
		case !alive[a]:
			l.drop(a, "app deleted")
		case !l.renew(a, now):
			l.drop(a, "renew lost")
		}
	}
	for _, a := range apps {
		if l.Owns(a) {
			continue
		}
		// 只有"没有租约行"或"行已过期/持有者就是自己"才可抢占；
		// 别人还在有效期内就等它掉线，绝不抢活着的租约（那正是双跑的来源）。
		if r, ok := leases[a]; ok && r.LeaseExpire.After(now) {
			continue
		}
		if epoch, ok := l.acquire(a, now); ok {
			l.mu.Lock()
			l.owned[a] = epoch
			l.mu.Unlock()
			log.Printf("lease: this server owns app %s (epoch %d)", a, epoch)
		}
	}
}

// appIds 全部项目。归属以 app 为粒度，所以 app_infos 就是待认领清单。
func (l *Leases) appIds() ([]string, error) {
	var ids []string
	err := l.db.Model(&store.AppInfo{}).Order("app_id asc").Pluck("app_id", &ids).Error
	return ids, err
}

// rows 现有租约行（原样返回，判活交给调用方，避免在这里就用掉时间基准）。
func (l *Leases) rows() (map[string]store.AppLease, error) {
	var rs []store.AppLease
	if err := l.db.Find(&rs).Error; err != nil {
		return nil, err
	}
	out := make(map[string]store.AppLease, len(rs))
	for _, r := range rs {
		out[r.AppId] = r
	}
	return out, nil
}

// freeLease "没人持有"的租约到期时刻。用 1970 而不是零值/NULL：
// MySQL 严格模式拒绝 '0000-01-01'，而 NULL 时间列扫进非指针 time.Time 会直接报错。
var freeLease = time.Unix(1, 0)

// ensureRow 保证租约行存在。抢占要先于读改写，所以靠唯一索引做幂等插入。
func (l *Leases) ensureRow(appId string) error {
	now := l.now()
	row := store.AppLease{AppId: appId, Holder: "", Epoch: 0,
		LeaseExpire: freeLease, CreateTime: now, ModifyTime: now}
	return l.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error
}

// acquire 抢占一个（自认为）空闲的租约。单条条件 UPDATE 决定成败，
// 两个副本同时抢只会成一个，输的那个 RowsAffected==0。
func (l *Leases) acquire(appId string, now time.Time) (int64, bool) {
	if err := l.ensureRow(appId); err != nil {
		log.Printf("lease ensure row for app %s: %v", appId, err)
		return 0, false
	}
	res := l.db.Model(&store.AppLease{}).
		// <= 与 rows()/Summarize 的"到期时刻即无人持有"保持同一口径，
		// 否则优雅下线刚写下的 lease_expire=now 会因毫秒相等而挡住接管。
		Where("app_id = ? AND (holder = ? OR lease_expire <= ?)", appId, l.me, now).
		Updates(map[string]any{
			"holder": l.me,
			// 续约不增 epoch，接管才 +1：epoch 变化就等于"换了主人"这件事得能被看出来。
			"epoch":        gorm.Expr("CASE WHEN holder = ? THEN epoch ELSE epoch + 1 END", l.me),
			"lease_expire": now.Add(l.ttl),
			"renew_count":  gorm.Expr("renew_count + 1"),
			"modify_time":  now,
		})
	if res.Error != nil {
		log.Printf("lease acquire app %s: %v", appId, res.Error)
		return 0, false
	}
	if res.RowsAffected != 1 {
		return 0, false
	}
	var row store.AppLease
	if err := l.db.Where("app_id = ?", appId).First(&row).Error; err != nil {
		log.Printf("lease read back app %s: %v", appId, err)
		return 0, false
	}
	if row.Holder != l.me {
		return 0, false // 读到之前又被抢走了，交给下一跳
	}
	return row.Epoch, true
}

// renew 续上已有租约（同一条条件 UPDATE，只是不改 epoch）。
func (l *Leases) renew(appId string, now time.Time) bool {
	res := l.db.Model(&store.AppLease{}).
		Where("app_id = ? AND holder = ?", appId, l.me).
		Updates(map[string]any{
			"lease_expire": now.Add(l.ttl),
			"renew_count":  gorm.Expr("renew_count + 1"),
			"modify_time":  now,
		})
	if res.Error != nil {
		log.Printf("lease renew app %s: %v", appId, res.Error)
		return false
	}
	return res.RowsAffected == 1
}

func (l *Leases) drop(appId, why string) {
	l.mu.Lock()
	_, had := l.owned[appId]
	delete(l.owned, appId)
	l.mu.Unlock()
	if !had {
		return
	}
	log.Printf("lease: this server lost app %s (%s)", appId, why)
	if l.onLost != nil {
		l.onLost(appId)
	}
}

// HolderSummary 供控制台展示：每个 app 当前的调度归属副本与租约剩余时间。
type HolderSummary struct {
	AppId       string    `json:"app_id"`
	Holder      string    `json:"holder"`
	Epoch       int64     `json:"epoch"`
	LeaseExpire time.Time `json:"lease_expire"`
	Alive       bool      `json:"alive"`
}

// Summarize 读全量租约视图（只读，不参与决策）。无租约行的 app 也列出，
// 好让页面能区分"没人调度"和"我看不清谁在调度"。
func Summarize(db *gorm.DB) ([]HolderSummary, error) {
	var apps []string
	if err := db.Model(&store.AppInfo{}).Order("app_id asc").Pluck("app_id", &apps).Error; err != nil {
		return nil, err
	}
	var rs []store.AppLease
	if err := db.Find(&rs).Error; err != nil {
		return nil, err
	}
	byApp := make(map[string]store.AppLease, len(rs))
	for _, r := range rs {
		byApp[r.AppId] = r
	}
	now := store.Now()
	out := make([]HolderSummary, 0, len(apps))
	for _, a := range apps {
		r, ok := byApp[a]
		if !ok {
			out = append(out, HolderSummary{AppId: a})
			continue
		}
		out = append(out, HolderSummary{AppId: a, Holder: r.Holder, Epoch: r.Epoch,
			LeaseExpire: r.LeaseExpire, Alive: r.LeaseExpire.After(now)})
	}
	return out, nil
}
