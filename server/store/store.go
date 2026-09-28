package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	glogger "gorm.io/gorm/logger"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	DB *gorm.DB
}

// Open driver: sqlite | mysql | postgres
func Open(driver, dsn string) (*Store, error) {
	var dialector gorm.Dialector
	switch driver {
	case "sqlite", "":
		// WAL + busy_timeout + 立即写锁事务：调度器/长轮询/日志上报并发写时，
		// deferred 事务升级为写锁的冲突不会被 busy_timeout 重试（immediate SQLITE_BUSY），
		// _txlock=immediate 让 BEGIN 阶段就排队等锁，冲突可被 busy_timeout 消化。
		if !strings.Contains(dsn, "_pragma") {
			sep := "?"
			if strings.Contains(dsn, "?") {
				sep = "&"
			}
			dsn += sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
		}
		dialector = sqlite.Open(dsn)
	case "mysql":
		dialector = mysql.Open(dsn)
	case "postgres", "pgsql", "postgresql":
		dialector = postgres.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported driver %q", driver)
	}
	db, err := gorm.Open(dialector, &gorm.Config{Logger: glogger.Default.LogMode(glogger.Warn)})
	if err != nil {
		return nil, err
	}
	return &Store{DB: db}, nil
}

// Pool 是数据库连接池参数。全部字段留零即不改（沿用 database/sql 的默认：MaxIdleConns=2、
// MaxOpenConns 不限），因为"不限连接"在 worker 惊群拉单时会瞬间把连接数顶到 MySQL 默认的
// max_connections=151 以上，并把新建 TCP 的握手成本摊到请求延迟上——所以 main.go 会显式给一组
// 保守默认值，见 cmd/gojob-server/main.go 的 DefaultPool。
type Pool struct {
	MaxOpenConns    int
	MaxIdleConns    int
	MaxConnLifetime time.Duration
}

func (s *Store) SetPool(p Pool) error {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return err
	}
	if p.MaxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(p.MaxOpenConns)
	}
	if p.MaxIdleConns > 0 {
		sqlDB.SetMaxIdleConns(p.MaxIdleConns)
	}
	if p.MaxConnLifetime > 0 {
		sqlDB.SetConnMaxLifetime(p.MaxConnLifetime)
	}
	return nil
}

func (s *Store) Migrate() error {
	if err := s.DB.AutoMigrate(&AppInfo{}, &UserInfo{}, &JobInfo{}, &JobInstance{},
		&InstanceTask{}, &InstanceTaskLog{}, &LoginLog{}, &OperationLog{}, &ServerNode{},
		&AppLease{}, &WorkerNode{}, &ClusterSignal{}, &TokenRevoked{}); err != nil {
		return err
	}
	// 为升级前的存量用户回填默认值（AutoMigrate 加列不会给旧行填默认）。
	if err := s.DB.Model(&UserInfo{}).Where("role = ?", "").Update("role", "SUPER_ADMIN").Error; err != nil {
		return err
	}
	if err := s.DB.Model(&UserInfo{}).Where("status = ?", "").Update("status", "ACTIVE").Error; err != nil {
		return err
	}
	// 吊销基线同理回填：存量行的 token_epoch 是 NULL，而 +1 运算在 NULL 上仍是 NULL，
	// 会让"改口令即踢人"静默失效，所以必须补成 0。
	if err := s.DB.Model(&UserInfo{}).Where("token_epoch IS NULL").Update("token_epoch", 0).Error; err != nil {
		return err
	}
	if err := s.DB.Model(&AppInfo{}).Where("token_epoch IS NULL").Update("token_epoch", 0).Error; err != nil {
		return err
	}
	// 跨副本唤醒信号的 singleton 行：bump 是 UPDATE，行不存在就什么都不会发生。
	now := Now()
	return s.DB.Where("id = ?", SignalRowID).
		FirstOrCreate(&ClusterSignal{ID: SignalRowID, CreateTime: now, ModifyTime: now}).Error
}

// ---------- Go 1.27 泛型方法：通用仓储操作 ----------

func (s *Store) Create[T any](entity *T) error {
	return s.DB.Create(entity).Error
}

func (s *Store) Get[T any](id uint64) (*T, error) {
	var e T
	if err := s.DB.First(&e, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &e, nil
}

func (s *Store) Updates[T any](id uint64, fields map[string]any) error {
	return s.DB.Model(new(T)).Where("id = ?", id).Updates(fields).Error
}

func (s *Store) List[T any](limit, offset int, order string, wheres ...map[string]any) ([]T, error) {
	var es []T
	q := s.DB.Model(new(T))
	for _, w := range wheres {
		for k, v := range w {
			q = q.Where(k+" = ?", v)
		}
	}
	err := q.Order(order).Limit(limit).Offset(offset).Find(&es).Error
	return es, err
}

// Tx 在事务中执行 fn，fn 返回的类型同样由泛型参数决定。
func (s *Store) Tx[R any](fn func(tx *gorm.DB) (R, error)) (R, error) {
	var zero R
	var out R
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var e error
		out, e = fn(tx)
		return e
	})
	if err != nil {
		return zero, err
	}
	return out, nil
}

func Now() time.Time { return time.Now() }
