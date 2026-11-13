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

// validateDSN 在驱动与 DSN 明显不匹配时给出可操作的错误。典型场景：切 MySQL/PG 时
// 只改了 GOJOB_DB_DRIVER，DSN 还留着 Dockerfile / compose 的 sqlite 默认值，
// 驱动会把 /app/data 当成网络名，报 "default addr for network '/app/data' unknown"。
func validateDSN(driver, dsn string) error {
	switch driver {
	case "mysql":
		// MySQL DSN 形如 [user[:pass]@][tcp(host:port)]/dbname?...；文件路径一定不是。
		if isFileDSN(dsn) || !strings.Contains(dsn, "/") {
			return fmt.Errorf("GOJOB_DB_DRIVER=mysql 但 GOJOB_DB_DSN=%q 是 sqlite 文件路径或不是 MySQL DSN；"+
				"请同时设置 MySQL DSN，例如 user:pass@tcp(host:3306)/db?charset=utf8mb4&parseTime=True&loc=UTC", dsn)
		}
	case "postgres", "pgsql", "postgresql":
		// PG 接 URL（postgres://...）或关键字写法（host=... dbname=...）；文件路径两者都不是。
		if isFileDSN(dsn) && !strings.Contains(dsn, "=") && !strings.Contains(dsn, "://") {
			return fmt.Errorf("GOJOB_DB_DRIVER=%s 但 GOJOB_DB_DSN=%q 是 sqlite 文件路径或不是 PostgreSQL DSN；"+
				"请同时设置 PG DSN，例如 postgres://user:pass@host:5432/db?sslmode=disable", driver, dsn)
		}
	}
	return nil
}

func isFileDSN(dsn string) bool {
	if strings.HasPrefix(dsn, "/") || strings.HasPrefix(dsn, "./") || strings.HasPrefix(dsn, "../") || strings.HasPrefix(dsn, "~") {
		return true
	}
	return strings.HasSuffix(dsn, ".db") || strings.HasSuffix(dsn, ".sqlite") || strings.HasSuffix(dsn, ".sqlite3")
}

// Open driver: sqlite | mysql | postgres
func Open(driver, dsn string) (*Store, error) {
	if err := validateDSN(driver, dsn); err != nil {
		return nil, err
	}
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

// schemaModels 启动期要建、也要核对的全部表。
var schemaModels = []any{&AppInfo{}, &UserInfo{}, &JobInfo{}, &JobInstance{},
	&InstanceTask{}, &InstanceTaskLog{}, &LoginLog{}, &OperationLog{}, &ServerNode{},
	&AppLease{}, &WorkerNode{}, &ClusterSignal{}, &TokenRevoked{}}

// Migrate 建表 + 回填，需要数据库账号具备 DDL 权限（sqlite、本地开发、一键体验）。
func (s *Store) Migrate() error { return s.migrate(true) }

// PrepareSchema 不执行任何 DDL。线上账号通常只有增删改查权限，建表由 DBA 用
// scripts/ddl/*.sql 完成，进程启动只核对表与列齐不齐，缺了就在启动期报错。
func (s *Store) PrepareSchema() error { return s.migrate(false) }

func (s *Store) migrate(autoMigrate bool) error {
	if autoMigrate {
		if err := s.DB.AutoMigrate(schemaModels...); err != nil {
			return err
		}
	} else if err := s.checkSchema(); err != nil {
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

// checkSchema 不开自动建表时核对表与列，缺什么都点名。等到运行期某条 SQL 报
// "Unknown column" 才发现，现场已经离配置点很远了。
func (s *Store) checkSchema() error {
	var missing []string
	m := s.DB.Migrator()
	for _, mdl := range schemaModels {
		stmt := &gorm.Statement{DB: s.DB}
		if err := stmt.Parse(mdl); err != nil {
			return err
		}
		table := stmt.Schema.Table
		if !m.HasTable(mdl) {
			missing = append(missing, "表 "+table)
			continue
		}
		cols, err := m.ColumnTypes(mdl)
		if err != nil {
			return err
		}
		have := make(map[string]bool, len(cols))
		for _, c := range cols {
			have[strings.ToLower(c.Name())] = true
		}
		for _, f := range stmt.Schema.Fields {
			if f.DBName != "" && !have[strings.ToLower(f.DBName)] {
				missing = append(missing, table+"."+f.DBName)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	list := strings.Join(missing, ", ")
	if len(missing) > 8 {
		list = strings.Join(missing[:8], ", ") + fmt.Sprintf("……（共 %d 项）", len(missing))
	}
	return fmt.Errorf("库里缺少 %s；本进程按 auto-migrate=false 启动，不会执行任何 DDL。"+
		"请由 DBA 执行 scripts/ddl/ 下对应驱动的建表脚本（存量库再补 server/README.md「存量库升级」里的 ALTER），"+
		"或临时改回 GOJOB_DB_AUTO_MIGRATE=true", list)
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
