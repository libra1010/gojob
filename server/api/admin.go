package api

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gojob/internal/protocol"
	"gojob/server/auth"
	"gojob/server/cluster"
	"gojob/server/scheduler"
	"gojob/server/store"
)

// ---------- App（项目）管理 ----------

// appView 项目行 + 当前调度归属副本，对齐 PowerJob 应用列表里的 currentServer 列。
// current_server 为空 = 此刻没有活着的副本持有该项目的租约（新项目待认领，或持有者刚掉线）。
type appView struct {
	store.AppInfo
	CurrentServer string `json:"current_server"`
}

func (s *Server) listApps(c *gin.Context) {
	var apps []store.AppInfo
	if err := s.st.DB.Order("id asc").Limit(500).Find(&apps).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	holders := make(map[string]string)
	ls, err := cluster.Summarize(s.st.DB)
	if err != nil {
		// 归属只是列表上的一列，读不到就让相应列为空，不因此拒绝整个列表。
		log.Printf("app list: read lease holders: %v", err)
	}
	for _, h := range ls {
		if h.Alive && h.Holder != "" {
			holders[h.AppId] = h.Holder
		}
	}
	out := make([]appView, 0, len(apps))
	for _, a := range apps {
		out = append(out, appView{AppInfo: a, CurrentServer: holders[a.AppId]})
	}
	writeOK(c, out)
}

func (s *Server) modifyApp(c *gin.Context) {
	req, err := bind[protocol.ModifyAppReq](c)
	if err != nil || req.AppId == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	app, err := s.appOf(req.AppId)
	if err != nil {
		writeErr(c, protocol.CodeNotFound, err.Error())
		return
	}
	fields := map[string]any{"modify_time": store.Now()}
	if req.AppName != nil {
		fields["app_name"] = *req.AppName
	}
	rotated := false
	if req.Password != nil && *req.Password != "" {
		hash, e := auth.HashPassword(*req.Password)
		if e != nil {
			writeErr(c, protocol.CodeErr, e.Error())
			return
		}
		fields["password"] = hash
		rotated = true
	}
	if err := s.st.Updates[store.AppInfo](app.ID, fields); err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	if rotated {
		// 换口令即吊销该项目全部 worker token：老节点必须用新口令重新注册，
		// "改口令踢掉旧凭证"才是真的成立（否则泄露的 token 还能活一个 TTL）
		if err := s.wtk.RevokeApp(req.AppId); err != nil {
			log.Printf("app %s: revoke worker tokens after password rotation: %v", req.AppId, err)
		} else {
			log.Printf("app %s password rotated: all previously issued worker tokens are now invalid", req.AppId)
		}
	}
	writeOK(c, struct{}{})
}

func (s *Server) deleteApp(c *gin.Context) {
	req, err := bind[protocol.DeleteAppReq](c)
	if err != nil || req.AppId == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	app, err := s.appOf(req.AppId)
	if err != nil {
		writeErr(c, protocol.CodeNotFound, err.Error())
		return
	}
	var n int64
	if err := s.st.DB.Model(&store.JobInfo{}).Where("app_id = ?", req.AppId).Count(&n).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	if n > 0 {
		writeErr(c, protocol.CodeConflict, fmt.Sprintf("app still has %d job(s), delete them first", n))
		return
	}
	if err := s.st.DB.Delete(&store.AppInfo{}, app.ID).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	// 项目已不存在，其 worker token 也不能再领任务（行删了 → 基线读不到 → 校验一律失败）
	if err := s.wtk.RevokeApp(req.AppId); err != nil {
		log.Printf("app %s: revoke worker tokens on delete: %v", req.AppId, err)
	}
	writeOK(c, struct{}{})
}

// listWorkers 查询 app 当前在线 worker 节点（心跳注册表）。
func (s *Server) listWorkers(c *gin.Context) {
	appId := c.Query("app_id")
	if appId == "" {
		writeErr(c, protocol.CodeBadReq, "app_id required")
		return
	}
	writeOK(c, s.reg.Workers(appId))
}

// ---------- Job 修改 / 删除 ----------

// dispatchStrategyOrDefault 校验分发策略取值；0 表示未指定，按 PowerJob 语义回落健康优先。
func dispatchStrategyOrDefault(v int) (int, error) {
	switch v {
	case 0:
		return protocol.DispatchHealthFirst, nil
	case protocol.DispatchHealthFirst, protocol.DispatchRandom, protocol.DispatchSpecify, protocol.DispatchRoundRobin:
		return v, nil
	}
	return 0, fmt.Errorf("unknown dispatch_strategy %d（可用 1=健康优先 2=随机 11=指定 21=轮询）", v)
}

func (s *Server) modifyJob(c *gin.Context) {
	req, err := bind[protocol.ModifyJobReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	job, err := s.st.Get[store.JobInfo](req.JobId)
	if err != nil {
		writeErr(c, protocol.CodeNotFound, err.Error())
		return
	}
	fields := map[string]any{"modify_time": store.Now()}
	setStr := func(p *string, col string) {
		if p != nil {
			fields[col] = *p
		}
	}
	setStr(req.JobName, "job_name")
	setStr(req.JobDescription, "job_description")
	setStr(req.Processor, "processor")
	setStr(req.ProcessorType, "processor_type")
	if req.ExecuteType != nil {
		v, e := normalizeExecuteType(*req.ExecuteType)
		if e != nil {
			writeErr(c, protocol.CodeBadReq, e.Error())
			return
		}
		fields["execute_type"] = v
	}
	setStr(req.JobParams, "job_params")
	setStr(req.InstanceParams, "instance_params")
	setStr(req.DesignatedWorkers, "designated_workers")
	setStr(req.DispatchStrategyConfig, "dispatch_strategy_config")
	if req.DispatchStrategy != nil {
		v, e := dispatchStrategyOrDefault(*req.DispatchStrategy)
		if e != nil {
			writeErr(c, protocol.CodeBadReq, e.Error())
			return
		}
		fields["dispatch_strategy"] = v
	}
	if req.UpstreamJobId != nil {
		upId := *req.UpstreamJobId
		if upId != job.UpstreamJobId {
			if e := s.validateUpstream(job.ID, job.AppId, upId); e != nil {
				writeErr(c, protocol.CodeBadReq, e.Error())
				return
			}
		}
		fields["upstream_job_id"] = upId
	}
	setStr(req.LogLevel, "log_level")
	if req.TimeExpressionType != nil {
		setStr(req.TimeExpressionType, "time_expression_type")
	}
	if req.TimeExpression != nil || req.TimeExpressionType != nil {
		teType := job.TimeExpressionType
		if req.TimeExpressionType != nil {
			teType = strings.TrimSpace(*req.TimeExpressionType)
		}
		expr := job.TimeExpression
		if req.TimeExpression != nil {
			expr = strings.TrimSpace(*req.TimeExpression)
			fields["time_expression"] = expr
		}
		next, e := scheduler.NextByType(teType, expr, store.Now(), store.Now())
		if e != nil {
			writeErr(c, protocol.CodeBadReq, e.Error())
			return
		}
		fields["next_trigger_time"] = next
	}
	// 时间类型与依赖配置按"改完之后的最终值"交叉校验：依赖触发型没有上游就永远不会执行
	effTeType := job.TimeExpressionType
	if req.TimeExpressionType != nil {
		effTeType = strings.TrimSpace(*req.TimeExpressionType)
	}
	effUpstream := job.UpstreamJobId
	if req.UpstreamJobId != nil {
		effUpstream = *req.UpstreamJobId
	}
	if e := validateDependencySchedule(effTeType, effUpstream); e != nil {
		writeErr(c, protocol.CodeBadReq, e.Error())
		return
	}
	setInt := func(p *int, col string) {
		if p != nil && *p > 0 {
			fields[col] = *p
		}
	}
	// 重试次数的 0 是配置本身（"不重试"），不能按"没传"丢掉——和 instance_num_limit 的
	// 0=不限制同一类。concurrency 的 0 没有意义，仍走 >0。
	setNonNeg := func(p *int, col string) {
		if p != nil && *p >= 0 {
			fields[col] = *p
		}
	}
	setNonNeg(req.MaxRetryTimes, "max_retry_times")
	setInt(req.Concurrency, "concurrency")
	setNonNeg(req.InstanceRetryNum, "instance_retry_num")
	if req.MaxWorkerCount != nil && *req.MaxWorkerCount >= 0 {
		fields["max_worker_count"] = *req.MaxWorkerCount
	}
	if req.InstanceNumLimit != nil && *req.InstanceNumLimit >= 0 {
		fields["instance_num_limit"] = *req.InstanceNumLimit
	}
	if req.SkipWhenRunning != nil {
		fields["skip_when_running"] = *req.SkipWhenRunning
	}
	if err := s.st.Updates[store.JobInfo](job.ID, fields); err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	s.sched.Reschedule(job.ID)
	writeOK(c, struct{}{})
}

func (s *Server) deleteJob(c *gin.Context) {
	req, err := bind[protocol.DeleteJobReq](c)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	job, err := s.st.Get[store.JobInfo](req.JobId)
	if err != nil {
		writeErr(c, protocol.CodeNotFound, err.Error())
		return
	}
	var n int64
	if err := s.st.DB.Model(&store.JobInstance{}).
		Where("job_id = ? AND status IN ?", job.ID,
			[]string{protocol.InstanceWaiting, protocol.InstanceRunning}).
		Count(&n).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	if n > 0 {
		writeErr(c, protocol.CodeConflict, "job has running instance(s)")
		return
	}
	// 依赖触发：仍有任务将本任务作为上游时不允许删除，否则下游配置变成永远等不到的悬空依赖
	var dependents int64
	if err := s.st.DB.Model(&store.JobInfo{}).Where("upstream_job_id = ?", job.ID).
		Count(&dependents).Error; err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	if dependents > 0 {
		writeErr(c, protocol.CodeConflict,
			fmt.Sprintf("job is the upstream of %d downstream job(s)", dependents))
		return
	}
	// 删除 job 与其尚未派发完的任务，保留历史实例便于审计
	_, err = s.st.Tx(func(tx *gorm.DB) (struct{}, error) {
		if e := tx.Where("job_id = ? AND status IN ?", job.ID,
			[]string{protocol.TaskWaiting, protocol.TaskRunning}).
			Delete(&store.InstanceTask{}).Error; e != nil {
			return struct{}{}, e
		}
		return struct{}{}, tx.Delete(&store.JobInfo{}, job.ID).Error
	})
	if err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	s.sched.Reschedule(job.ID) // 撤下时间轮上已删除的任务
	writeOK(c, struct{}{})
}

// previewTime 预览最近 N 次计划触发时间（不入库，仅按表达式计算）。
// GET /openapi/job/previewTime?time_expression_type=CRON&time_expression=0/5 * * * * ?&count=15
func (s *Server) previewTime(c *gin.Context) {
	teType := c.Query("time_expression_type")
	expr := c.Query("time_expression")
	count := 15
	if n, err := strconv.Atoi(c.Query("count")); err == nil {
		count = n
	}
	times, err := scheduler.Preview(teType, expr, store.Now(), count)
	if err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	// 固定频率/固定延迟/每日间隔可亚秒级，预览需带上毫秒位
	layout := "2006-01-02 15:04:05"
	switch t := strings.ToUpper(strings.TrimSpace(teType)); t {
	case "FIXED_RATE", "FIXED_DELAY", "DAILY_TIME_INTERVAL":
		layout = "2006-01-02 15:04:05.000"
	}
	out := make([]string, 0, len(times))
	for _, t := range times {
		out = append(out, t.Format(layout))
	}
	writeOK(c, gin.H{"type": strings.ToUpper(strings.TrimSpace(teType)), "preview": out})
}
