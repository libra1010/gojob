package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
	// 内嵌 IANA 时区库：裸 alpine 没有 zoneinfo，缺了它 TZ=Asia/Shanghai 会被静默忽略、退回 UTC。
	_ "time/tzdata"

	"gojob/server/api"
	"gojob/server/auth"
	"gojob/server/cleanup"
	"gojob/server/cluster"
	"gojob/server/dispatch"
	"gojob/server/node"
	"gojob/server/registry"
	"gojob/server/scheduler"
	"gojob/server/store"
)

// version 由发布脚本用 -ldflags "-X main.version=$(git describe --tags)" 盖进二进制；
// 容器里 `docker logs` 第一行就能看到跑的是哪一版。本地 go run 则是 dev。
var version = "dev"

// envOverride 实现配置优先级：GOJOB_* 环境变量 > 命令行 flag > 默认值。
func envOverride(key string, target *string) {
	if v := os.Getenv(key); v != "" {
		*target = v
	}
}

// envIntOverride 同优先级，只是目标是数值。环境变量非法时如实警告并沿用默认值——
// 静默吞掉一个写错的 GOJOB_DB_MAX_OPEN_CONNS，比启动即报错更难查。
func envIntOverride(key string, target *int) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("WARNING: %s=%q 不是整数，沿用默认值", key, v)
		return
	}
	*target = n
}

// envDurOverride 同 envIntOverride，只是目标是时长（写 "30m" / "2h" 这种 Go 时长字面量）。
func envDurOverride(key string, target *time.Duration) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("WARNING: %s=%q 不是合法时长（如 30m/2h），沿用默认值", key, v)
		return
	}
	*target = d
}

// envBoolOverride 同 envIntOverride，目标是开关（"true"/"1"/"false"/"0" 都认）。
func envBoolOverride(key string, target *bool) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("WARNING: %s=%q 不是布尔值（true/false），沿用默认值", key, v)
		return
	}
	*target = b
}

func main() {
	addr := flag.String("addr", ":7800", "HTTP listen address")
	driver := flag.String("driver", "sqlite", "db driver: sqlite | mysql | postgres")
	dsn := flag.String("dsn", "gojob.db", "sqlite file, mysql DSN (user:pwd@tcp(host:port)/db?params) or postgres DSN (postgres://...)")
	adminUser := flag.String("admin-user", "admin", "seeded admin username (first boot only)")
	adminPass := flag.String("admin-password", "admin123", "seeded admin password (first boot only)")
	publicURL := flag.String("public-url", "", "对外访问地址（统计页展示），如 http://schedule.example.com；留空按监听地址推导")
	clusterSecret := flag.String("cluster-secret", "", "令牌签名密钥；多副本部署必须各节点配同一个值，留空则本进程随机生成（仅单副本可用）")
	dbMaxOpen := flag.Int("db-max-open-conns", 0, "数据库最大连接数；0=按驱动取默认（mysql/postgres 20，sqlite 10）。多副本按「副本数×本值 ≤ 数据库 max_connections − 预留」算")
	dbMaxIdle := flag.Int("db-max-idle-conns", 0, "数据库空闲连接保留数；0=跟随 --db-max-open-conns")
	dbConnLife := flag.Duration("db-conn-max-lifetime", 0, "单条连接的最长存活时间，0=30m。数据库侧主备切换后要调小")
	retentionDays := flag.Int("retention-days", 0, "历史实例保留天数：终态实例超过它就连同子任务、任务日志一起删；0=只认任务上配的 life_cycle；两边都不配则永不清理")
	workerAESKeys := flag.String("worker-aes-keys", "", "注册口令加密的 AES-256 key，64 位 hex，逗号分隔多把只用于轮换期并存；留空=口令明文送上来（现状）")
	workerAESRequired := flag.Bool("worker-aes-required", false, "配了 --worker-aes-keys 后进一步要求所有 worker/OpenAPI 注册必须走加密，明文注册直接拒绝")
	flag.Parse()

	envOverride("GOJOB_ADDR", addr)
	envOverride("GOJOB_DB_DRIVER", driver)
	envOverride("GOJOB_DB_DSN", dsn)
	envOverride("GOJOB_ADMIN_USER", adminUser)
	envOverride("GOJOB_ADMIN_PASSWORD", adminPass)
	envOverride("GOJOB_PUBLIC_URL", publicURL)
	envOverride("GOJOB_CLUSTER_SECRET", clusterSecret)
	envIntOverride("GOJOB_DB_MAX_OPEN_CONNS", dbMaxOpen)
	envIntOverride("GOJOB_DB_MAX_IDLE_CONNS", dbMaxIdle)
	envDurOverride("GOJOB_DB_CONN_MAX_LIFETIME", dbConnLife)
	envIntOverride("GOJOB_RETENTION_DAYS", retentionDays)
	envOverride("GOJOB_WORKER_AES_KEYS", workerAESKeys)
	envBoolOverride("GOJOB_WORKER_AES_REQUIRED", workerAESRequired)
	// 时区自检：TZ 解析失败（镜像缺 tzdata、名字拼错、CST-8 这类 POSIX 写法）时 Go 静默退回
	// UTC，日志与 cron 全偏 8 小时——启动第一屏把解析结果喊出来，部署后一眼对账。
	log.Printf("timezone: local=%s TZ=%q", time.Local, os.Getenv("TZ"))
	if tz := os.Getenv("TZ"); tz != "" && tz != "UTC" && time.Local.String() == "UTC" {
		log.Printf("WARNING: TZ=%q 未生效，仍按 UTC 运行（检查镜像 tzdata 与时区名拼写）", tz)
	}
	if *publicURL == "" {
		host, port, err := net.SplitHostPort(*addr)
		if err == nil {
			if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
				host = localIP()
			}
			*publicURL = "http://" + net.JoinHostPort(host, port)
		} else {
			*publicURL = *addr
		}
	}

	st, err := store.Open(*driver, *dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	// 连接池必须显式设：database/sql 的默认是 MaxOpenConns=无限、MaxIdleConns=2，
	// 于是上千个 worker 惊群拉单时会一路新建 TCP 连接（顶到 MySQL 默认 max_connections=151
	// 就直接报错），同时把握手成本摊进请求延迟，并在数据库侧留下大量空闲连接占内存。
	pool := store.Pool{MaxConnLifetime: 30 * time.Minute}
	pool.MaxOpenConns = 20
	if *driver == "sqlite" {
		pool.MaxOpenConns = 10 // 单写者，连接给多了也只是排队
	}
	if *dbMaxOpen > 0 {
		pool.MaxOpenConns = *dbMaxOpen
	}
	pool.MaxIdleConns = pool.MaxOpenConns // 连接是复用的热点，别让池反复销毁重建
	if *dbMaxIdle > 0 {
		pool.MaxIdleConns = *dbMaxIdle
	}
	if *dbConnLife > 0 {
		pool.MaxConnLifetime = *dbConnLife
	}
	if err := st.SetPool(pool); err != nil {
		log.Fatalf("configure db pool: %v", err)
	}
	log.Printf("db pool: max_open=%d max_idle=%d conn_max_lifetime=%s",
		pool.MaxOpenConns, pool.MaxIdleConns, pool.MaxConnLifetime)
	if err := st.Migrate(); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if created, err := auth.SeedAdmin(st, *adminUser, *adminPass); err != nil {
		log.Fatalf("seed admin: %v", err)
	} else if created {
		log.Printf("seeded admin user %q — change the password ASAP", *adminUser)
	}
	// 历史明文项目密码原地升级成密文（幂等），老部署直接滚上来即可
	if n, err := auth.UpgradeAppPasswords(st); err != nil {
		log.Fatalf("upgrade app passwords: %v", err)
	} else if n > 0 {
		log.Printf("upgraded %d app record(s) from plaintext to hashed password", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 集群身份与调度归属：node.NodeID() 含启动纳秒，所以重启即新持有者，旧租约靠 TTL 过期。
	self := node.NodeID()
	leases := cluster.NewLeases(st.DB, self, cluster.LeaseTTL)
	sig := cluster.NewSignal(st.DB)

	reg := registry.New(st.DB, 15*time.Second)
	disp := dispatch.New(st, reg, sig)
	sched := scheduler.New(st, leases, disp.Notify, disp.PickTarget, disp.BroadcastTargets, reg.HasWorkers)
	// 失去归属 → 立刻撤掉该项目的时间轮，不再去撞 fencing。
	leases.OnLost(sched.DropApp)
	// 依赖触发：实例终态（事务已提交）回调调度器，成功则拉起下游实例。
	disp.OnInstanceFinished(func(ev dispatch.FinishEvent) {
		sched.TriggerDependency(ev.JobId, ev.InstanceId, ev.Status)
	})
	authSet := auth.NewSet(st.DB, *clusterSecret, auth.ConsoleTokenTTL,
		auth.WorkerTokenTTL, auth.OpenAPITokenTTL)
	if authSet.Signer.Ephemeral {
		log.Printf("WARNING: GOJOB_CLUSTER_SECRET 未设置，本进程用随机密钥签发令牌 —— " +
			"单副本没问题，多副本部署会让令牌跨节点互不认（登录随机掉线、worker 随机 401）。" +
			"多副本务必给所有节点配同一个值。")
	}
	// 注册口令加密：只影响"口令怎么送上来"这一步，链路其余部分照旧明文。
	if err := authSet.EnableRegisterEncryption(*workerAESKeys, *workerAESRequired); err != nil {
		log.Fatalf("worker register encryption: %v", err)
	}
	if authSet.Register.Enabled() {
		log.Printf("worker register encryption: AES-256-GCM kid=%v encrypted-only=%v —— "+
			"它只保证项目口令不出现在链路上，challenge/iv/密文照旧是明文传输的内容，"+
			"任务参数与执行日志也不受它保护（那些要靠内网或前置 TLS）",
			authSet.Register.Kids(), authSet.Register.Enforce())
	}

	// 本进程自监控：连接数挂 ConnState、请求数挂中间件，每 5s 采样并 upsert 进 server_nodes，
	// 多副本部署时控制台的「节点监控」页据此看到每个副本各自的 CPU/内存/GC/连接。
	boot := time.Now()
	col := node.New()
	pub := node.NewPublisher(st.DB, self, node.Hostname(), *addr, *publicURL, boot)

	// 后台维护循环：清理失联节点、回收超时任务
	go maintain(ctx, reg, disp, authSet.Revoked)
	// 历史实例回收：只删本机持有归属的项目，多副本不会互相抢同一批行做无用功。
	cleaner := cleanup.New(st, leases.Owns, *retentionDays)
	if cleaner.Enabled() {
		log.Printf("cleanup: 终态实例保留 %d 天（任务上配了 life_cycle 的按各自天数），每 %s 一批批回收",
			*retentionDays, cleanup.Interval)
	} else {
		log.Printf("cleanup: GOJOB_RETENTION_DAYS 未配置且任务未配 life_cycle ⇒ 历史实例与任务日志将无限增长。" +
			"长期运行的部署请配一个保留天数（例如 30）。")
	}
	go cleaner.Run(ctx)
	go leases.Run(ctx)        // 租约续约/接管
	go disp.WatchSignals(ctx) // 别的副本入了队，本机长轮询要立刻知道
	go sched.Run(ctx)

	router := api.New(st, reg, disp, sched, authSet, leases, *driver, *publicURL).Router(col.Middleware())
	srv := &http.Server{Addr: *addr, Handler: router, ConnState: col.ConnState}
	go col.Run(ctx, func(s node.Sample) {
		if err := pub.Publish(s, col.History()); err != nil {
			log.Printf("node metrics publish: %v", err)
		}
	})
	go func() {
		log.Printf("gojob-server %s listening on %s (%s)", version, *addr, *driver)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Println("gojob-server exited")
}

// localIP 取本机出口 IP，仅用于推导默认对外访问地址。
func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

func maintain(ctx context.Context, reg *registry.Registry, disp *dispatch.Dispatcher, revoked *auth.Revoked) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reg.Evict()
			if n, err := revoked.Purge(); err != nil {
				log.Printf("maintain: purge revoked tokens: %v", err)
			} else if n > 0 {
				log.Printf("maintain: purged %d expired revoked token(s)", n)
			}
			if err := disp.ReassignLostTasks(5 * time.Minute); err != nil {
				log.Printf("maintain: %v", err)
			}
			if err := disp.ReleaseStaleTargets(time.Minute); err != nil {
				log.Printf("maintain: %v", err)
			}
			// 排队超过 1 分钟、项目仍无在线节点的任务判失败：兜住"触发后节点立刻掉线"这类漏网。
			if err := disp.FailOrphanedWaiting(time.Minute); err != nil {
				log.Printf("maintain: %v", err)
			}
		}
	}
}
