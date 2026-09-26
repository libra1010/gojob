// Package api 暴露两组 HTTP 接口：
//
//	/openapi/* 管理面（Dashboard、curl、OpenAPI 调用）
//	/worker/*  数据面（Worker 心跳 / 拉任务 / 回报结果）
//
// 编解码统一走 encoding/json/v2（Go 1.27），不依赖 gin 内建 JSON。
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	v2 "encoding/json/v2"
	"github.com/gin-gonic/gin"

	"gojob/internal/protocol"
	"gojob/server/auth"
	"gojob/server/cluster"
	"gojob/server/dispatch"
	"gojob/server/registry"
	"gojob/server/scheduler"
	"gojob/server/store"
)

type Server struct {
	st    *store.Store
	reg   *registry.Registry
	disp  *dispatch.Dispatcher
	sched *scheduler.Scheduler
	am    *auth.Manager
	wtk   *auth.WorkerTokens
	pj    *auth.OpenAPITokens
	// rcrypto 注册口令加密（一次性 challenge + AES-GCM）；nil = 未启用，注册走明文。
	rcrypto   *auth.RegisterCrypto
	leases    *cluster.Leases // 本副本的调度归属账本，供控制台展示"谁在调度哪个项目"
	dbDriver  string
	publicURL string
}

// New as 是本进程的一整套令牌体系（见 auth.Set）：控制台会话、worker 数据面、
// OpenAPI 兼容层的三种票由同一个集群密钥签发，因此多副本只要密钥一致就互认。
func New(st *store.Store, reg *registry.Registry, disp *dispatch.Dispatcher, sched *scheduler.Scheduler,
	as *auth.Set, leases *cluster.Leases, dbDriver, publicURL string) *Server {
	return &Server{st: st, reg: reg, disp: disp, sched: sched, leases: leases,
		am: as.Console, wtk: as.Worker, pj: as.OpenAPI, rcrypto: as.Register,
		dbDriver: dbDriver, publicURL: publicURL}
}

// Router 装配路由。mid 是进程级中间件（自监控采集），必须在注册任何路由**之前**挂上：
// gin 的 addRoute 会把当时的 handler 链快照进每条路由，之后再 engine.Use() 只对
// 404/405 生效——那时「节点监控」页的 poll_inflight / req_per_sec 会永远是 0。
func (s *Server) Router(mid ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(mid...)

	// 存活探针（docker healthcheck / 负载均衡）
	r.GET("/healthz", func(c *gin.Context) { writeOK(c, "ok") })

	// 登录接口公开，其余 /openapi 需 token
	r.POST("/openapi/user/login", s.login)

	admin := r.Group("/openapi", s.requireLogin, s.opAudit(), s.authorize())
	{
		admin.POST("/user/logout", s.logout)
		admin.GET("/user/me", s.me)
		admin.POST("/user/changePassword", s.changePassword)

		// 账号管理：仅 SUPER_ADMIN
		u := admin.Group("/user", s.requireRole("SUPER_ADMIN"))
		u.GET("/list", s.listUsers)
		u.POST("/create", s.createUser)
		u.POST("/modify", s.modifyUser)
		u.POST("/resetPassword", s.resetPassword)
		u.POST("/status", s.setUserStatus)
		u.POST("/delete", s.deleteUser)

		// 审计日志：ADMIN 及以上
		au := admin.Group("/audit", s.requireRole("ADMIN", "SUPER_ADMIN"))
		au.GET("/loginLogs", s.loginLogs)
		au.GET("/operationLogs", s.operationLogs)

		// 节点监控（调度中心各副本 + worker runtime 指标）：ADMIN 及以上
		sys := admin.Group("/system", s.requireRole("ADMIN", "SUPER_ADMIN"))
		sys.GET("/nodes", s.systemNodes)

		admin.POST("/app/create", s.createApp)
		admin.POST("/app/modify", s.modifyApp)
		admin.POST("/app/delete", s.deleteApp)
		admin.GET("/app/list", s.listApps)
		admin.GET("/app/workers", s.listWorkers)

		admin.POST("/job/create", s.createJob)
		admin.POST("/job/modify", s.modifyJob)
		admin.POST("/job/delete", s.deleteJob)
		admin.GET("/job/get", s.getJob)
		admin.GET("/job/list", s.listJobs)
		admin.GET("/job/export", s.exportJobs)
		admin.POST("/job/operate", s.operateJob)
		admin.POST("/job/import", s.importJobs)
		admin.GET("/job/previewTime", s.previewTime)

		admin.GET("/instance/list", s.listInstances)
		admin.GET("/instance/get", s.getInstance)
		admin.GET("/instance/tasks", s.instanceTasks)
		admin.POST("/instance/detailPlus", s.detailPlus) // 实例详情弹窗：Task 统计 + 子任务查询
		admin.GET("/instance/logs", s.instanceLogs)
		admin.POST("/instance/stop", s.stopInstance)

		admin.GET("/stats/overview", s.statsOverview)
	}

	w := r.Group("/worker")
	{
		w.POST("/register", s.workerRegister)   // 唯一需要项目口令的数据面入口
		w.POST("/challenge", s.workerChallenge) // 加密注册握手第一步：领一次性 challenge
		w.POST("/heartbeat", s.heartbeat)
		w.POST("/fetchTask", s.fetchTask)
		w.POST("/report", s.report)
		w.POST("/mapTask", s.mapTask)         // 处理器运行期派生子任务（MAP / MAP_REDUCE）
		w.POST("/taskResults", s.taskResults) // reduce 任务分页取子任务结果
		w.POST("/reportLogs", s.reportLogs)
	}

	// OpenAPI 兼容层（/openApi，注意大小写与 /openapi 不冲突）
	s.registerOpenAPICompat(r)
	return r
}

// ---------- encoding/json/v2 编解码助手（泛型函数） ----------

const jsonCT = "application/json; charset=utf-8"

func writeOK[T any](c *gin.Context, data T) {
	bs, err := v2.Marshal(protocol.Resp[T]{Code: protocol.CodeOK, Data: data})
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Data(http.StatusOK, jsonCT, bs)
}

func writeErr(c *gin.Context, code int, msg string) {
	bs, _ := v2.Marshal(protocol.Resp[struct{}]{Code: code, Message: msg})
	c.Data(http.StatusOK, jsonCT, bs)
}

func bind[T any](c *gin.Context) (*T, error) {
	body, err := c.GetRawData()
	if err != nil {
		return nil, err
	}
	var v T
	if err := v2.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func parseID(q string) (uint64, error) {
	id, err := strconv.ParseUint(q, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("invalid id: " + q)
	}
	return id, nil
}

// ---------- 鉴权 ----------

func (s *Server) appOf(appId string) (*store.AppInfo, error) {
	var app store.AppInfo
	if err := s.st.DB.Where("app_id = ?", appId).First(&app).Error; err != nil {
		return nil, errors.New("app not found: " + appId)
	}
	return &app, nil
}

func (s *Server) auth(appId, password string) error {
	app, err := s.appOf(appId)
	if err != nil {
		return err
	}
	// 库里存的是 bcrypt 摘要，不再保存明文
	if !auth.CheckPassword(app.Password, password) {
		return errors.New("wrong password")
	}
	return nil
}

// ---------- 管理面 ----------

func (s *Server) createApp(c *gin.Context) {
	req, err := bind[protocol.CreateAppReq](c)
	if err != nil || req.AppId == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	app := store.AppInfo{AppId: req.AppId, AppName: req.AppName, CreateTime: store.Now(), ModifyTime: store.Now()}
	app.Password, err = auth.HashPassword(req.Password)
	if err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	if err := s.st.Create(&app); err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, app.ID)
}

func (s *Server) createJob(c *gin.Context) {
	req, err := bind[protocol.CreateJobReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	if _, err := s.appOf(req.AppId); err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	teType := orDefaultStr(req.TimeExpressionType, "CRON")
	next, err := scheduler.NextByType(teType, req.TimeExpression, store.Now(), store.Now())
	if err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	dispStrategy, err := dispatchStrategyOrDefault(req.DispatchStrategy)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	if err := s.validateUpstream(0, req.AppId, req.UpstreamJobId); err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	if err := validateDependencySchedule(teType, req.UpstreamJobId); err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	// 执行类型必须是已实现的四种之一：WORKFLOW 等未实现类型在这里就拒掉，
	// 别让控制台存下一个"看着能配、永远跑不起来"的任务。
	execType, execErr := normalizeExecuteType(req.ExecuteType)
	if execErr != nil {
		writeErr(c, protocol.CodeBadReq, execErr.Error())
		return
	}
	job := store.JobInfo{
		AppId: req.AppId, JobName: req.JobName, JobDescription: req.JobDescription,
		Processor: req.Processor, TimeExpression: strings.TrimSpace(req.TimeExpression),
		TimeExpressionType: teType,
		JobParams:          req.JobParams, InstanceParams: req.InstanceParams,
		ProcessorType:     orDefaultStr(req.ProcessorType, "BUILT_IN"),
		ExecuteType:       execType,
		MaxRetryTimes:     orUnset(req.MaxRetryTimes, 3, 0),
		InstanceRetryNum:  orUnset(req.InstanceRetryNum, 3, 0),
		Concurrency:       orUnset(req.Concurrency, 5, 1),
		InstanceNumLimit:  orUnset(req.InstanceNumLimit, 3, 0),
		SkipWhenRunning:   req.SkipWhenRunning,
		DesignatedWorkers: req.DesignatedWorkers,
		MaxWorkerCount:    req.MaxWorkerCount,
		DispatchStrategy:  dispStrategy, DispatchStrategyConfig: strings.TrimSpace(req.DispatchStrategyConfig),
		UpstreamJobId: req.UpstreamJobId,
		LogLevel:      orDefaultStr(req.LogLevel, "INFO"),
		Enable:        true, NextTriggerTime: next,
		CreateTime: store.Now(), ModifyTime: store.Now(),
	}
	if err := s.st.Create(&job); err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	s.sched.Reschedule(job.ID)
	writeOK(c, job.ID)
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

// orUnset 只在这个字段没出现在请求里时填默认值；显式传 0 是合法配置
// （max_retry_times=0 即"不重试"，instance_num_limit=0 即"不限制实例数"），
// 而 min 以下（负数）按 min 处理。
func orUnset(v *int, def, floor int) int {
	if v == nil {
		return def
	}
	return max(*v, floor)
}

func orDefaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

func (s *Server) getJob(c *gin.Context) {
	id, err := parseID(c.Query("job_id"))
	if err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	job, err := s.st.Get[store.JobInfo](id)
	if err != nil {
		writeErr(c, protocol.CodeNotFound, err.Error())
		return
	}
	writeOK(c, job)
}

// listJobs 任务列表：语义沿用兼容协议的会话接口 `/job/list`（index 从 0 起、jobId 精确、
// keyword 模糊匹配任务名/描述、返回 PageResult）。
// page_size<=0 表示不分页（返回全量、total_pages=1）——控制台建单抽屉里的「上游任务」下拉
// 要的是全项目候选，不能被分页切成几页。
func (s *Server) listJobs(c *gin.Context) {
	q := s.st.DB.Model(&store.JobInfo{})
	if appId := c.Query("app_id"); appId != "" {
		q = q.Where("app_id = ?", appId)
	}
	if jobId := c.Query("job_id"); jobId != "" {
		q = q.Where("id = ?", jobId)
	}
	if kw := strings.TrimSpace(c.Query("keyword")); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("job_name LIKE ? OR job_description LIKE ?", like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	index, pageSize := intQuery(c, "index", 0), intQuery(c, "page_size", 10)
	if index < 0 {
		index = 0
	}
	if pageSize > 0 {
		q = q.Offset(index * pageSize).Limit(pageSize)
	}
	var jobs []store.JobInfo
	if err := q.Order("id desc").Find(&jobs).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	if pageSize <= 0 { // 不分页：一次给全
		pageSize = int(total)
	}
	writeOK(c, protocol.PageResult[store.JobInfo]{Index: index, PageSize: pageSize,
		TotalItems: total, TotalPages: ceilDiv(int(total), pageSize), Data: jobs})
}

// intQuery 读一个整数 query 参数，缺省或非法取默认值。
func intQuery(c *gin.Context, name string, def int) int {
	v := strings.TrimSpace(c.Query(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func (s *Server) operateJob(c *gin.Context) {
	req, err := bind[protocol.OperateJobReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	job, err := s.st.Get[store.JobInfo](req.JobId)
	if err != nil {
		writeErr(c, protocol.CodeNotFound, err.Error())
		return
	}
	switch req.Action {
	case "pause":
		err = s.st.Updates[store.JobInfo](job.ID, map[string]any{"enable": false})
		s.sched.Reschedule(job.ID)
	case "resume":
		next, e := scheduler.NextByType(job.TimeExpressionType, job.TimeExpression, store.Now(), store.Now())
		if e != nil {
			writeErr(c, protocol.CodeBadReq, e.Error())
			return
		}
		err = s.st.Updates[store.JobInfo](job.ID, map[string]any{"enable": true, "next_trigger_time": next})
		s.sched.Reschedule(job.ID)
	case "trigger":
		// 带 instance_params 就是兼容协议的 runJob 语义（控制台「参数运行」），
		// 只影响这一次运行，不改任务里配的默认实例参数。
		if req.InstanceParams != "" {
			_, err = s.sched.TriggerRun(job, req.InstanceParams)
		} else {
			err = s.sched.TriggerManually(job)
		}
	default:
		writeErr(c, protocol.CodeBadReq, "unknown action: "+req.Action)
		return
	}
	if err != nil {
		if errors.Is(err, scheduler.ErrJobRunning) {
			writeErr(c, protocol.CodeConflict, err.Error())
			return
		}
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, struct{}{})
}

func (s *Server) listInstances(c *gin.Context) {
	var insts []store.JobInstance
	q := s.st.DB.Order("id desc").Limit(200)
	if jobId := c.Query("job_id"); jobId != "" {
		q = q.Where("job_id = ?", jobId)
	}
	if err := q.Find(&insts).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, insts)
}

func (s *Server) getInstance(c *gin.Context) {
	id, err := parseID(c.Query("instance_id"))
	if err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	inst, err := s.st.Get[store.JobInstance](id)
	if err != nil {
		writeErr(c, protocol.CodeNotFound, err.Error())
		return
	}
	writeOK(c, inst)
}

// stopInstance 停止实例：停止意图写入 DB，靠长轮询/心跳响应夹带下发给 worker。
// POST /openapi/instance/stop {instance_id, app_id}
func (s *Server) stopInstance(c *gin.Context) {
	req, err := bind[protocol.StopInstanceReq](c)
	if err != nil || req.InstanceId == 0 {
		writeErr(c, protocol.CodeBadReq, "bad request: instance_id required")
		return
	}
	if err := s.disp.StopInstance(req.InstanceId); err != nil {
		writeErr(c, protocol.CodeNotFound, err.Error())
		return
	}
	writeOK(c, struct{}{})
}

// ---------- 数据面 ----------
//
// 除 /worker/register 外，全部端点只认 token：auth.ForWorker 校验 token 有效性，
// 并强制 token 绑定身份（app_id + instance_id）与请求者自述一致，
// 因此泄露的 token 既不能冒充别的节点领任务，也不能改判别人的任务结果。

func (s *Server) workerRegister(c *gin.Context) {
	req, err := bind[protocol.RegisterReq](c)
	if err != nil || req.AppId == "" || req.InstanceId == "" {
		writeErr(c, protocol.CodeBadReq, "bad request: app_id and instance_id required")
		return
	}
	// 口令可能以两种方式上来（明文 / challenge+AES-GCM），先还原再校验，
	// 后面的 bcrypt 比对与日志与引入加密前完全一样。
	out := s.clearRegister(req.AppId, req.InstanceId, auth.Clear{
		Scheme: req.AuthScheme, Kid: req.Kid, Iv: req.Iv,
		Ciphertext: req.Ciphertext, Challenge: req.Challenge, Password: req.Password,
	})
	if out.Err != nil || !out.OK {
		s.writeClearReject(c, out)
		return
	}
	if err := s.auth(req.AppId, out.Password); err != nil {
		writeErr(c, protocol.CodeAuthErr, "register rejected: "+err.Error())
		return
	}
	tk, err := s.wtk.Issue(req.AppId, req.InstanceId)
	if err != nil {
		writeErr(c, protocol.CodeErr, "register failed: "+err.Error())
		return
	}
	log.Printf("worker registered: app=%s instance=%s addr=%s sdk=%s ver=%s",
		req.AppId, req.InstanceId, req.Address, req.SdkType, req.Version)
	writeOK(c, protocol.RegisterResp{Token: tk, TtlSeconds: int(s.wtk.TTL() / time.Second)})
}

// workerAuthorized 数据面鉴权；返回的错误原文回给 worker，SDK 靠 401 触发重注册。
func (s *Server) workerAuthorized(appId, instanceId, token string) error {
	err := s.wtk.Validate(token, appId, instanceId)
	if errors.Is(err, auth.ErrTokenImpersonated) {
		log.Printf("worker token rejected: %s (app=%s, self-reported instance=%s)",
			err, appId, instanceId)
	}
	return err
}

func (s *Server) heartbeat(c *gin.Context) {
	req, err := bind[protocol.HeartbeatReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	if err := s.workerAuthorized(req.AppId, req.InstanceId, req.Token); err != nil {
		writeErr(c, protocol.CodeAuthErr, err.Error())
		return
	}
	if err := s.reg.Alive(req.WorkerInfo); err != nil {
		// 在线表没落库就等于"这个节点在集群里不存在"：分发策略看不见它、任务也不会派给它，
		// 所以如实回错，让 SDK 下个心跳周期重试，而不是假装登记成功。
		writeErr(c, protocol.CodeErr, "heartbeat not recorded: "+err.Error())
		return
	}
	// 心跳是停止指令的兜底通道（长轮询已断或 worker 满载时仍能收到），
	// 同时是集群路由提示的下发通道：worker 每 5s 一次心跳，搭车下发就等于零额外 QPS，
	// 不需要兼容协议那种每 10s 一次的 /server/acquire 专用端点。
	writeOK(c, protocol.HeartbeatResp{
		CancelTasks: protocol.CancelTasks{CancelTaskIds: s.disp.PendingCancels(req.InstanceId)},
		Cluster:     s.clusterHint(req.AppId),
	})
}

// clusterHint 组装本 app 的集群路由提示。取不到就返回空提示——路由提示不承重，
// 任何一步失败都不能让心跳失败（那会让 worker 以为节点没登记上）。
func (s *Server) clusterHint(appId string) protocol.ClusterHint {
	var h protocol.ClusterHint
	nodeId, addr, err := cluster.Owner(s.st.DB, appId)
	if err != nil {
		log.Printf("cluster hint: owner of app %s: %v", appId, err)
		return h
	}
	h.OwnerNode, h.Owner = nodeId, addr
	servers, err := cluster.AliveServers(s.st.DB)
	if err != nil {
		log.Printf("cluster hint: alive servers: %v", err)
		return h
	}
	h.Servers = servers
	return h
}

func (s *Server) fetchTask(c *gin.Context) {
	req, err := bind[protocol.FetchTaskReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	if err := s.workerAuthorized(req.AppId, req.InstanceId, req.Token); err != nil {
		writeErr(c, protocol.CodeAuthErr, err.Error())
		return
	}
	req.WaitSeconds = min(max(req.WaitSeconds, 1), 120)
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Duration(req.WaitSeconds)*time.Second+5*time.Second)
	defer cancel()
	tasks, cancels, err := s.disp.Fetch(ctx, req)
	if err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, protocol.FetchTaskResp{Tasks: tasks, CancelTasks: protocol.CancelTasks{CancelTaskIds: cancels}})
}

func (s *Server) report(c *gin.Context) {
	req, err := bind[protocol.ReportTaskReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	if err := s.workerAuthorized(req.AppId, req.InstanceId, req.Token); err != nil {
		writeErr(c, protocol.CodeAuthErr, err.Error())
		return
	}
	if err := s.disp.Report(c.Request.Context(), req); err != nil {
		log.Printf("report task %d: %v", req.TaskId, err)
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, struct{}{})
}

// mapTask 处理器在运行期派生子任务。与 report 同一套 token 鉴权，归属校验在 Dispatcher 里
// （只有正在执行该任务的那台 worker 能替它派生），所以一个被窃取的 token 只能给
// 自己领到的任务加子任务，不能往别人的实例里塞。
func (s *Server) mapTask(c *gin.Context) {
	req, err := bind[protocol.MapTaskReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	if err := s.workerAuthorized(req.AppId, req.InstanceId, req.Token); err != nil {
		writeErr(c, protocol.CodeAuthErr, err.Error())
		return
	}
	resp, err := s.disp.MapTasks(req)
	if err != nil {
		log.Printf("map task %d: %v", req.TaskId, err)
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, resp)
}

// taskResults reduce 任务取子任务结果清单（分页）。
func (s *Server) taskResults(c *gin.Context) {
	req, err := bind[protocol.TaskResultsReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	if err := s.workerAuthorized(req.AppId, req.InstanceId, req.Token); err != nil {
		writeErr(c, protocol.CodeAuthErr, err.Error())
		return
	}
	resp, err := s.disp.TaskResults(req)
	if err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, resp)
}
