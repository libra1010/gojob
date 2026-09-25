// PowerJob OpenAPI 兼容层：路径、鉴权、DTO、响应信封对齐 PowerJob 4.x 客户端
// （tech.powerjob.client.PowerJobClient 直接可用）。
//
//	鉴权：POST /openApi/authApp {appName, password: 明文}
//	      → data {appId, token}；后续请求携带 Header:
//	      X-POWERJOB-APP-ID / X-POWERJOB-ACCESS-TOKEN；
//	      响应头 X-POWERJOB-AUTH-PASSED: true|false（客户端以此判断是否需要刷新鉴权）。
//	响应：ResultDTO 形态 {"success":bool,"data":...,"message":...}。
//
// 差异（README 中同步声明）：authApp 只接受明文 password（库里无 md5 等价物），
// encryptType=1 的 encryptedPassword 请求被明确拒绝；executeType 仅 STANDALONE 真实生效；
// 工作流接口返回不支持；runJob 的 delayMS>0 时异步触发、返回 data=0；
// 额外支持 gojob 扩展字段 skipWhenRunning。
package api

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/server/auth"
	"gojob/server/scheduler"
	"gojob/server/store"
)

const (
	pjHeaderAppId    = "X-POWERJOB-APP-ID"
	pjHeaderToken    = "X-POWERJOB-ACCESS-TOKEN"
	pjHeaderAuthPass = "X-POWERJOB-AUTH-PASSED"
)

// ---------- 响应信封 ----------

type pjResult struct {
	Success bool   `json:"success"`
	Data    any    `json:"data"`
	Message string `json:"message"`
}

func pjWrite(c *gin.Context, authPassed bool, r pjResult) {
	c.Header(pjHeaderAuthPass, strconv.FormatBool(authPassed))
	bs, err := v2.Marshal(r)
	if err != nil {
		c.Status(500)
		return
	}
	c.Data(200, jsonCT, bs)
}

func pjOK(c *gin.Context, data any) { pjWrite(c, true, pjResult{Success: true, Data: data}) }

func pjFail(c *gin.Context, msg string) {
	pjWrite(c, true, pjResult{Success: false, Message: msg})
}

// ---------- App 级 token ----------
//
// 令牌本身在 auth.OpenAPITokens 里（无状态签名 + 项目吊销基线），这里只保留
// "从请求头里取票"这一段 HTTP 侧的活。

// ---------- 路由注册 ----------

func (s *Server) registerPowerJobOpenAPI(r *gin.Engine) {
	g := r.Group("/openApi")
	g.POST("/authApp", s.pjAuthApp)
	pj := g.Group("", s.pjRequire)
	{
		pj.POST("/saveJob", s.pjSaveJob)
		pj.POST("/fetchJob", s.pjFetchJob)
		pj.POST("/fetchAllJob", s.pjFetchAllJob)
		pj.POST("/queryJob", s.pjFetchAllJob) // 一期忽略 query 条件，返回 app 全量
		pj.POST("/exportJob", s.pjFetchJob)
		pj.POST("/copyJob", s.pjCopyJob)
		pj.POST("/disableJob", s.pjDisableJob)
		pj.POST("/enableJob", s.pjEnableJob)
		pj.POST("/deleteJob", s.pjDeleteJob)
		pj.POST("/runJob", s.pjRunJob)
		pj.POST("/runJob2", s.pjRunJob)
		pj.POST("/stopInstance", s.pjStopInstance)
		pj.POST("/retryInstance", s.pjRetryInstance)
		pj.POST("/fetchInstanceStatus", s.pjFetchInstanceStatus)
		pj.POST("/fetchInstanceInfo", s.pjFetchInstanceInfo)
		pj.POST("/queryInstance", s.pjQueryInstance)
		for _, wf := range []string{
			"saveWorkflow", "copyWorkflow", "fetchWorkflow", "disableWorkflow",
			"enableWorkflow", "deleteWorkflow", "runWorkflow", "addWorkflowNode",
			"stopWfInstance", "retryWfInstance", "fetchWfInstanceInfo", "markWorkflowNodeAsSuccess",
		} {
			name := wf
			pj.POST("/"+name, func(c *gin.Context) {
				pjFail(c, "gojob does not support workflow ("+name+") yet, on roadmap M5")
			})
		}
	}
}

// ---------- 鉴权 ----------

func (s *Server) pjAuthApp(c *gin.Context) {
	req, err := bind[struct {
		AppName           string `json:"appName"`
		Password          string `json:"password"`
		EncryptedPassword string `json:"encryptedPassword"`
		EncryptType       int    `json:"encryptType"`
	}](c)
	if err != nil || req.AppName == "" {
		pjFail(c, "bad authApp request")
		return
	}
	app, err := s.appOf(req.AppName)
	if err != nil {
		pjFail(c, "AUTH_FAILED: app not found")
		return
	}
	// 声明的偏离：gojob 库里只有 bcrypt 摘要，没有任何明文口令等价物，所以 authApp 必须提交
	// 明文 password。官方 PowerJob 客户端在 encryptType=1 下只提交 encryptedPassword=md5(明文)，
	// 这类请求无法校验（要支持就得入库一份"知道即可冒用"的摘要），一律明确拒绝。
	if req.Password == "" {
		pjFail(c, "AUTH_FAILED: gojob requires plaintext \"password\" in authApp; "+
			"encryptedPassword(md5) is rejected by design, see README \"与 PowerJob 的差异\"")
		return
	}
	if !auth.CheckPassword(app.Password, req.Password) {
		pjFail(c, "AUTH_FAILED: password mismatch")
		return
	}
	tk, err := s.pj.Issue(app.AppId)
	if err != nil {
		pjFail(c, "AUTH_FAILED: issue token: "+err.Error())
		return
	}
	pjOK(c, struct {
		AppId uint64 `json:"appId"`
		Token string `json:"token"`
	}{app.ID, tk})
}

func (s *Server) pjRequire(c *gin.Context) {
	tk := firstNonEmpty(c.GetHeader(pjHeaderToken), c.GetHeader(strings.ToLower(pjHeaderToken)))
	if tk == "" {
		pjWrite(c, false, pjResult{Success: false, Message: "AUTH_MISSING: " + pjHeaderToken})
		c.Abort()
		return
	}
	appId, ok := s.pj.Lookup(tk)
	if !ok {
		pjWrite(c, false, pjResult{Success: false, Message: "AUTH_EXPIRED, retry authApp"})
		c.Abort()
		return
	}
	if c0, ok := s.pj.ClaimsOf(tk); ok {
		if fresh := s.pj.RenewIfStale(c0); fresh != "" {
			c.Header(auth.RenewedHeader, fresh)
		}
	}
	c.Set("pjApp", appId)
	c.Next()
}

func (s *Server) pjApp(c *gin.Context) string { v, _ := c.Get("pjApp"); a, _ := v.(string); return a }

// ---------- Job DTO 与映射 ----------

type pjSaveJobReq struct {
	Id                     *uint64 `json:"id"`
	JobName                string  `json:"jobName"`
	JobDescription         string  `json:"jobDescription"`
	AppId                  *uint64 `json:"appId"`
	JobParams              any     `json:"jobParams"`
	TimeExpressionType     *string `json:"timeExpressionType"`
	TimeExpression         *string `json:"timeExpression"`
	ExecuteType            *string `json:"executeType"`
	ProcessorType          *string `json:"processorType"`
	ProcessorInfo          *string `json:"processorInfo"`
	MaxInstanceNum         *int    `json:"maxInstanceNum"`
	Concurrency            *int    `json:"concurrency"`
	InstanceTimeLimit      *int64  `json:"instanceTimeLimit"`
	InstanceRetryNum       *int    `json:"instanceRetryNum"`
	TaskRetryNum           *int    `json:"taskRetryNum"`
	DesignatedWorkers      *string `json:"designatedWorkers"`
	MaxWorkerCount         *int    `json:"maxWorkerCount"`
	DispatchStrategy       any     `json:"dispatchStrategy"` // 枚举名或整数值，PowerJob 客户端两种形态都发得出
	DispatchStrategyConfig *string `json:"dispatchStrategyConfig"`
	Enable                 *bool   `json:"enable"`
	// gojob 扩展字段（PowerJob 客户端会忽略未知字段，向后兼容）
	SkipWhenRunning *bool `json:"skipWhenRunning"`
}

func paramsToString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		bs, _ := v2.Marshal(t)
		return string(bs)
	}
}

func (s *Server) pjApplyJob(req *pjSaveJobReq, job *store.JobInfo) error {
	job.JobName = req.JobName
	job.JobDescription = req.JobDescription
	job.Processor = derefStr(req.ProcessorInfo, "")
	job.ProcessorType = strings.ToUpper(derefStr(req.ProcessorType, "BUILT_IN"))
	// OpenAPI 与控制台/导入共用同一份执行类型白名单：兼容层不设防等于给绕过校验留后门
	execType, e := normalizeExecuteType(derefStr(req.ExecuteType, "STANDALONE"))
	if e != nil {
		return e
	}
	job.ExecuteType = execType
	job.DesignatedWorkers = derefStr(req.DesignatedWorkers, "")
	job.MaxWorkerCount = deref(req.MaxWorkerCount, 0)
	disp, err := pjStrategy(req.DispatchStrategy)
	if err != nil {
		return err
	}
	job.DispatchStrategy = disp
	job.DispatchStrategyConfig = derefStr(req.DispatchStrategyConfig, "")
	job.TimeExpression = derefStr(req.TimeExpression, "")
	teType := strings.ToUpper(derefStr(req.TimeExpressionType, "CRON"))
	switch teType {
	case "CRON", "API", "FIXED_RATE", "FIXED_DELAY", "DAILY_TIME_INTERVAL":
	default:
		return fmt.Errorf("timeExpressionType %q unsupported, use CRON / API / FIXED_RATE / FIXED_DELAY / DAILY_TIME_INTERVAL", teType)
	}
	// 兼容旧语义：无类型但表达式为空 → API 手动型
	if req.TimeExpressionType == nil && strings.TrimSpace(job.TimeExpression) == "" {
		teType = "API"
	}
	job.TimeExpressionType = teType
	if req.JobParams != nil {
		job.JobParams = paramsToString(req.JobParams)
	} else {
		job.JobParams = ""
	}
	// 处理器类型判定与导入共用一份：放在 JobParams 之后，因为 SHELL 会把脚本正文搬进任务参数。
	ptype, processor, jobParams, perr := normalizeProcessorType(
		job.ProcessorType, job.Processor, job.JobParams, job.InstanceParams)
	if perr != nil {
		return perr
	}
	job.ProcessorType, job.Processor, job.JobParams = ptype, processor, jobParams
	job.MaxRetryTimes = deref(req.TaskRetryNum, deref(req.InstanceRetryNum, job.MaxRetryTimes))
	if job.MaxRetryTimes <= 0 {
		job.MaxRetryTimes = 3
	}
	job.Concurrency = deref(req.Concurrency, 5)
	if req.MaxInstanceNum != nil {
		job.InstanceNumLimit = *req.MaxInstanceNum // 0=不限制
		if *req.MaxInstanceNum == 1 && req.SkipWhenRunning == nil {
			job.SkipWhenRunning = true // 语义等价转换
		}
	}
	if req.SkipWhenRunning != nil {
		job.SkipWhenRunning = *req.SkipWhenRunning
	}
	if req.Enable != nil {
		job.Enable = *req.Enable
	}
	next, err := scheduler.NextByType(job.TimeExpressionType, job.TimeExpression, store.Now(), store.Now())
	if err != nil {
		return err
	}
	job.NextTriggerTime = next
	return nil
}

func deref[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}

func derefStr(p *string, def string) string {
	if p != nil {
		return *p
	}
	return def
}

// pjStrategy 归一化 PowerJob 客户端的 dispatchStrategy：Jackson 默认发枚举名，
// 控制台/导出 JSON 发整数值。缺省回落 HEALTH_FIRST（同 PowerJob DispatchStrategy.of(null)）。
func pjStrategy(v any) (int, error) {
	switch t := v.(type) {
	case nil:
		return protocol.DispatchHealthFirst, nil
	case float64:
		return dispatchStrategyOrDefault(int(t))
	case int:
		return dispatchStrategyOrDefault(t)
	case string:
		name := strings.ToUpper(strings.TrimSpace(t))
		switch name {
		case "":
			return protocol.DispatchHealthFirst, nil
		case "HEALTH_FIRST":
			return protocol.DispatchHealthFirst, nil
		case "RANDOM":
			return protocol.DispatchRandom, nil
		case "SPECIFY":
			return protocol.DispatchSpecify, nil
		case "ROUND_ROBIN":
			return protocol.DispatchRoundRobin, nil
		}
		if n, e := strconv.Atoi(name); e == nil {
			return dispatchStrategyOrDefault(n)
		}
	}
	return 0, fmt.Errorf("unknown dispatchStrategy %v, use HEALTH_FIRST / RANDOM / SPECIFY(11) or 1 / 2 / 11", v)
}

func (s *Server) pjToDTO(job *store.JobInfo) pjSaveJobReq {
	d := pjSaveJobReq{
		Id: &job.ID, JobName: job.JobName, JobDescription: job.JobDescription, AppId: nil,
		JobParams:      job.JobParams,
		ProcessorInfo:  strPtr(job.Processor),
		ProcessorType:  strPtr(orDefaultStr(job.ProcessorType, "BUILT_IN")),
		ExecuteType:    strPtr(orDefaultStr(job.ExecuteType, "STANDALONE")),
		TimeExpression: strPtr(job.TimeExpression),
		Enable:         &job.Enable,
	}
	d.TimeExpressionType = strPtr(orDefaultStr(job.TimeExpressionType, "CRON"))
	disp := job.DispatchStrategy
	if disp == 0 {
		disp = protocol.DispatchHealthFirst
	}
	d.DispatchStrategy = disp
	if job.DispatchStrategyConfig != "" {
		d.DispatchStrategyConfig = strPtr(job.DispatchStrategyConfig)
	}
	if job.DesignatedWorkers != "" {
		d.DesignatedWorkers = strPtr(job.DesignatedWorkers)
	}
	m, c, r := job.InstanceNumLimit, job.Concurrency, job.MaxRetryTimes
	sk := job.SkipWhenRunning
	d.MaxInstanceNum, d.Concurrency, d.TaskRetryNum, d.InstanceRetryNum, d.SkipWhenRunning = &m, &c, &r, &r, &sk
	w := job.MaxWorkerCount
	d.MaxWorkerCount = &w
	return d
}

func strPtr(s string) *string { return &s }

// ---------- 请求字段提取 ----------
// PowerJob 各版本客户端有的接口发 form 参数、有的发 JSON body，这里统一提取；
// 结果缓存在 gin context 上（body 只能读一次）。
func pjFields(c *gin.Context) map[string]string {
	if v, ok := c.Get("pjFields"); ok {
		return v.(map[string]string)
	}
	out := map[string]string{}
	_ = c.Request.ParseForm()
	for k, v := range c.Request.PostForm {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	if len(out) == 0 {
		if body, err := c.GetRawData(); err == nil && strings.TrimSpace(string(body)) != "" {
			var m map[string]any
			if v2.Unmarshal(body, &m) == nil {
				for k, v := range m {
					switch t := v.(type) {
					case string:
						out[k] = t
					default:
						if v != nil {
							out[k] = fmt.Sprintf("%v", t)
						}
					}
				}
			}
		}
	}
	c.Set("pjFields", out)
	return out
}

// ---------- Job 接口 ----------

func (s *Server) pjSaveJob(c *gin.Context) {
	req, err := bind[pjSaveJobReq](c)
	if err != nil || req.JobName == "" {
		pjFail(c, "bad saveJob request")
		return
	}
	appId := s.pjApp(c)
	if req.Id != nil && *req.Id > 0 { // 更新
		job, err := s.st.Get[store.JobInfo](*req.Id)
		if err != nil || job.AppId != appId {
			pjFail(c, "job not found in this app")
			return
		}
		if err := s.pjApplyJob(req, job); err != nil {
			pjFail(c, err.Error())
			return
		}
		job.ModifyTime = store.Now()
		if err := s.st.DB.Save(job).Error; err != nil {
			pjFail(c, err.Error())
			return
		}
		s.sched.Reschedule(job.ID)
		pjOK(c, job.ID)
		return
	}
	job := &store.JobInfo{AppId: appId, Enable: true, MaxRetryTimes: 3,
		Concurrency: 5, InstanceNumLimit: 0,
		CreateTime: store.Now(), ModifyTime: store.Now()}
	if err := s.pjApplyJob(req, job); err != nil {
		pjFail(c, err.Error())
		return
	}
	if err := s.st.Create(job); err != nil {
		pjFail(c, err.Error())
		return
	}
	s.sched.Reschedule(job.ID)
	pjOK(c, job.ID)
}

// pjJobOf 按 jobId（form 或 JSON body）取本 app 的 job
func (s *Server) pjJobOf(c *gin.Context) (*store.JobInfo, bool) {
	id, err := parseID(pjFields(c)["jobId"])
	if err != nil {
		pjFail(c, "jobId required")
		return nil, false
	}
	job, err := s.st.Get[store.JobInfo](id)
	if err != nil || job.AppId != s.pjApp(c) {
		pjFail(c, "job not found in this app")
		return nil, false
	}
	return job, true
}

func (s *Server) pjFetchJob(c *gin.Context) {
	job, ok := s.pjJobOf(c)
	if ok {
		pjOK(c, s.pjToDTO(job))
	}
}

func (s *Server) pjFetchAllJob(c *gin.Context) {
	var jobs []store.JobInfo
	if err := s.st.DB.Where("app_id = ?", s.pjApp(c)).Limit(1000).Find(&jobs).Error; err != nil {
		pjFail(c, err.Error())
		return
	}
	out := make([]pjSaveJobReq, 0, len(jobs))
	for i := range jobs {
		out = append(out, s.pjToDTO(&jobs[i]))
	}
	pjOK(c, out)
}

func (s *Server) pjCopyJob(c *gin.Context) {
	job, ok := s.pjJobOf(c)
	if !ok {
		return
	}
	cp := *job
	cp.ID = 0
	cp.JobName = job.JobName + "_copy"
	cp.CreateTime, cp.ModifyTime = store.Now(), store.Now()
	if err := s.st.Create(&cp); err != nil {
		pjFail(c, err.Error())
		return
	}
	s.sched.Reschedule(cp.ID)
	pjOK(c, cp.ID)
}

func (s *Server) pjDisableJob(c *gin.Context) { s.pjSetEnable(c, false) }
func (s *Server) pjEnableJob(c *gin.Context)  { s.pjSetEnable(c, true) }

func (s *Server) pjSetEnable(c *gin.Context, enable bool) {
	job, ok := s.pjJobOf(c)
	if !ok {
		return
	}
	fields := map[string]any{"enable": enable, "modify_time": store.Now()}
	if enable {
		next, err := scheduler.NextByType(job.TimeExpressionType, job.TimeExpression, store.Now(), store.Now())
		if err != nil {
			pjFail(c, err.Error())
			return
		}
		fields["next_trigger_time"] = next
	}
	if err := s.st.Updates[store.JobInfo](job.ID, fields); err != nil {
		pjFail(c, err.Error())
		return
	}
	s.sched.Reschedule(job.ID)
	pjOK(c, nil)
}

func (s *Server) pjDeleteJob(c *gin.Context) {
	job, ok := s.pjJobOf(c)
	if !ok {
		return
	}
	var n int64
	if err := s.st.DB.Model(&store.JobInstance{}).
		Where("job_id = ? AND status IN ?", job.ID,
			[]string{protocol.InstanceWaiting, protocol.InstanceRunning}).Count(&n).Error; err != nil {
		pjFail(c, err.Error())
		return
	}
	if n > 0 {
		pjFail(c, "job has running instances")
		return
	}
	_, err := s.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		if e := tx.Where("job_id = ? AND status IN ?", job.ID,
			[]string{protocol.TaskWaiting, protocol.TaskRunning}).
			Delete(&store.InstanceTask{}).Error; e != nil {
			return struct{}{}, e
		}
		return struct{}{}, tx.Delete(&store.JobInfo{}, job.ID).Error
	})
	if err != nil {
		pjFail(c, err.Error())
		return
	}
	s.sched.Reschedule(job.ID) // 撤下已删除任务的挂轮
	pjOK(c, nil)
}

// ---------- runJob / 实例操作 ----------

// runJob / runJob2：兼容 form(delayMS) 与 JSON RunJobRequest(delay, ms) 两种客户端形态
func (s *Server) pjRunJob(c *gin.Context) {
	job, ok := s.pjJobOf(c)
	if !ok {
		return
	}
	f := pjFields(c)
	delay, _ := strconv.ParseInt(firstNonEmpty(f["delayMS"], f["delay"], "0"), 10, 64)
	s.pjRun(c, job, f["instanceParams"], delay)
}

func (s *Server) pjRun(c *gin.Context, job *store.JobInfo, instanceParams string, delayMS int64) {
	if delayMS > 0 { // 一期：异步延迟触发，立即返回，data=0 表示实例稍后可查
		d := time.Duration(delayMS) * time.Millisecond
		go func() {
			time.Sleep(d)
			if _, err := s.sched.TriggerRun(job, instanceParams); err != nil {
				// skip_when_running 等策略导致跳过：静默，与 PowerJob 行为差异见 README
				_ = err
			}
		}()
		pjOK(c, uint64(0))
		return
	}
	instId, err := s.sched.TriggerRun(job, instanceParams)
	if err != nil {
		pjFail(c, err.Error())
		return
	}
	pjOK(c, instId)
}

func (s *Server) pjInstanceOf(c *gin.Context) (*store.JobInstance, bool) {
	id, err := parseID(pjFields(c)["instanceId"])
	if err != nil {
		pjFail(c, "instanceId required")
		return nil, false
	}
	inst, err := s.st.Get[store.JobInstance](id)
	if err != nil || inst.AppId != s.pjApp(c) {
		pjFail(c, "instance not found in this app")
		return nil, false
	}
	return inst, true
}

func (s *Server) pjStopInstance(c *gin.Context) {
	inst, ok := s.pjInstanceOf(c)
	if !ok {
		return
	}
	if err := s.disp.StopInstance(inst.ID); err != nil {
		pjFail(c, err.Error())
		return
	}
	pjOK(c, nil)
}

func (s *Server) pjRetryInstance(c *gin.Context) {
	inst, ok := s.pjInstanceOf(c)
	if !ok {
		return
	}
	if inst.Status == protocol.InstanceWaiting || inst.Status == protocol.InstanceRunning {
		pjFail(c, "instance still alive, no need to retry")
		return
	}
	now := store.Now()
	_, err := s.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		task := store.InstanceTask{
			JobId: inst.JobId, InstanceId: inst.ID, AppId: inst.AppId,
			Processor: inst.Processor, JobParams: inst.JobParams, FixedParams: inst.FixedParams,
			Status: protocol.TaskWaiting, Attempts: 1, MaxAttempts: 4,
			DispatchTime: now, CreateTime: now, ModifyTime: now,
		}
		if e := tx.Create(&task).Error; e != nil {
			return struct{}{}, e
		}
		return struct{}{}, tx.Model(inst).Updates(map[string]any{
			"status": protocol.InstanceRunning, "running_times": inst.RunningTimes + 1,
			"total_task_num": inst.TotalTaskNum + 1, "modify_time": now,
		}).Error
	})
	if err != nil {
		pjFail(c, err.Error())
		return
	}
	s.disp.Notify()
	pjOK(c, nil)
}

func (s *Server) pjFetchInstanceStatus(c *gin.Context) {
	inst, ok := s.pjInstanceOf(c)
	if ok {
		pjOK(c, inst.Status)
	}
}

type pjInstanceDTO struct {
	InstanceId      uint64 `json:"instanceId"`
	JobId           uint64 `json:"jobId"`
	JobName         string `json:"jobName"`
	AppId           string `json:"appId"`
	Status          string `json:"status"`
	Result          string `json:"result"`
	AppParams       string `json:"appParams"`
	InstanceParams  string `json:"instanceParams"`
	WorkflowInfo    string `json:"workflowInfo"`
	ScheduleTime    int64  `json:"scheduleTime"`
	ActualTrigger   int64  `json:"actualTriggerTime"`
	FinishedTime    int64  `json:"finishedTime"`
	RunningDuration int64  `json:"runningTime"`
}

func (s *Server) pjToInstanceDTO(inst *store.JobInstance, result string) pjInstanceDTO {
	return pjInstanceDTO{
		InstanceId: inst.ID, JobId: inst.JobId, JobName: inst.JobName, AppId: inst.AppId,
		Status: inst.Status, Result: result,
		AppParams: inst.JobParams, InstanceParams: inst.FixedParams,
		ScheduleTime: inst.CreateTime.UnixMilli(), ActualTrigger: inst.CreateTime.UnixMilli(),
		FinishedTime: inst.ModifyTime.UnixMilli(),
	}
}

func (s *Server) pjTaskResult(instanceId uint64) string {
	var t []store.InstanceTask
	_ = s.st.DB.Where("instance_id = ?", instanceId).Order("id desc").Limit(5).Find(&t).Error
	msgs := make([]string, 0, len(t))
	for _, x := range t {
		if x.Result != "" {
			msgs = append(msgs, x.Result)
		}
	}
	return strings.Join(msgs, "\n")
}

func (s *Server) pjFetchInstanceInfo(c *gin.Context) {
	inst, ok := s.pjInstanceOf(c)
	if ok {
		pjOK(c, s.pjToInstanceDTO(inst, s.pjTaskResult(inst.ID)))
	}
}

func (s *Server) pjQueryInstance(c *gin.Context) {
	var insts []store.JobInstance
	q := s.st.DB.Where("app_id = ?", s.pjApp(c)).Order("id desc").Limit(200)
	if j := pjFields(c)["jobId"]; j != "" {
		q = q.Where("job_id = ?", j)
	}
	if err := q.Find(&insts).Error; err != nil {
		pjFail(c, err.Error())
		return
	}
	out := make([]pjInstanceDTO, 0, len(insts))
	for i := range insts {
		out = append(out, s.pjToInstanceDTO(&insts[i], ""))
	}
	pjOK(c, out)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
