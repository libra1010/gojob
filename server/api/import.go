// 任务导入：接受兼容协议控制台导出的 JobDTO JSON（camelCase）。
// 一期映射策略：CRON + STANDALONE + BUILT_IN/SPRING 处理器可导入，
// BROADCAST/MAP/容器任务等高级能力在 M4/M5 实现后再放开，逐条返回失败原因。
package api

import (
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"gojob/internal/protocol"
	"gojob/server/scheduler"
	"gojob/server/store"
)

type jobConverter struct {
	appId string
}

// normalizeProcessorType 把兼容协议的 ProcessorType 归一成 gojob worker 能执行的写法，
// 返回 (处理器类型, 处理器信息, 任务参数)。导入与控制台/OpenAPI 建单共用这一份判定，
// 免得两条路径接受的值集不一样。
//
// 兼容协议的枚举有 BUILT_IN(1)/SHELL(2)/PYTHON(3)/EXTERNAL(4) 四个值，但 5.x worker 侧的
// ProcessorFactory 只支持其中两个（按其 supportTypes 分派）：
//   - BUILT_IN：Class.forName(processorInfo)，gojob 的注册名与类全名都走这一档；
//   - EXTERNAL：JarContainerProcessorFactory 按"容器ID#类名"下载 JAR 热部署，gojob 没有
//     对应实现，直接拒绝——存下来只会得到一个永远跑不起来、还看不出原因的哑弹；
//   - SHELL/PYTHON：@Deprecated，且没有任何 factory 支持。旧 dump 里的 SHELL 任务把脚本
//     正文存在 processorInfo，而官方脚本处理器读的是执行参数（CommonUtils.parseParams：
//     instanceParams 非空用它，否则 jobParams），所以 SHELL 改写成
//     BUILT_IN + 官方 ShellProcessor 类名 + 脚本文本搬到任务参数。
//
// normalizeExecuteType 校验执行类型，取值与兼容协议 ExecuteType 枚举一致。
// 建单、改单、导入、OpenAPI 四条写路径共用：任何一条不设防，控制台就会存下跑不起来的类型。
func normalizeExecuteType(v string) (string, error) {
	switch s := strings.ToUpper(strings.TrimSpace(v)); s {
	case "":
		return protocol.ExecuteStandalone, nil
	case protocol.ExecuteStandalone, protocol.ExecuteBroadcast,
		protocol.ExecuteMap, protocol.ExecuteMapReduce:
		return s, nil
	case "SHARDING":
		return "", errField("executeType", "暂不支持 SHARDING（兼容协议 3.x 的分片执行，4.x 起由 MAP 承担；请把任务改成 MAP）")
	case protocol.ExecuteWorkflow:
		return "", errField("executeType", "暂不支持 WORKFLOW：gojob 没有工作流引擎，任务间前后置关系用时间类型 DEPENDENCY（依赖上游任务成功）表达")
	default:
		return "", errField("executeType", "未知执行类型 "+s)
	}
}

func normalizeProcessorType(ptype, processor, jobParams, instanceParams string) (string, string, string, error) {
	switch ptype {
	case "SPRING_CONTAINER": // 兼容协议 3.x 的历史值，5.x 由 Spring 容器处理器以 BUILT_IN 承担
		return "BUILT_IN", processor, jobParams, nil
	case "SHELL":
		script := strings.TrimSpace(processor)
		if script == "" {
			return "", "", "", errField("processorInfo", "SHELL 任务的处理器信息应填脚本文本")
		}
		if strings.TrimSpace(jobParams) != "" {
			return "", "", "", errField("jobParams", "SHELL 任务的脚本文本在处理器信息里，与已有的任务参数冲突：请人工确认哪一段是脚本后按 BUILT_IN 导入")
		}
		if strings.TrimSpace(instanceParams) != "" {
			return "", "", "", errField("instanceParams", "脚本处理器取参数时实例参数优先，这条 SHELL 任务两者都有会跑错文本：请清空实例参数后按 BUILT_IN 导入")
		}
		return "BUILT_IN", protocol.OfficialShellProcessor, script, nil
	case "PYTHON":
		return "", "", "", errField("processorType", "暂不支持 PYTHON 处理器类型：请在 worker 内实现该处理器后按 BUILT_IN 配置")
	case "EXTERNAL":
		return "", "", "", errField("processorType", "暂不支持 EXTERNAL（兼容协议的 JAR 动态容器，处理器信息形如 容器ID#类名）：请在 worker 内实现后按 BUILT_IN 配置")
	case "BUILT_IN":
		if processor == "" {
			return "", "", "", errField("processorInfo", "缺失处理器类名")
		}
		return ptype, processor, jobParams, nil
	default:
		return "", "", "", errField("processorType", "未知处理器类型 "+ptype)
	}
}

// convert 把一条任务包里的任务定义转换为 gojob JobInfo；返回 error 表示不可导入的原因。
func (jc *jobConverter) convert(p *protocol.JobDTO) (*store.JobInfo, error) {
	deref := func(s *string, def string) string {
		if s != nil {
			return *s
		}
		return def
	}

	jobName := strings.TrimSpace(deref(p.JobName, ""))
	if jobName == "" {
		return nil, errField("jobName", "缺失任务名")
	}
	switch strings.ToUpper(deref(p.ExecuteType, "STANDALONE")) {
	case "STANDALONE", "BROADCAST", "MAP", "MAP_REDUCE":
	case "SHARDING":
		// 兼容协议 3.x 的分片执行，4.x 起被 MAP 取代；老导出包里偶尔还见得到
	default:
		return nil, errField("executeType", "暂不支持执行类型 "+*p.ExecuteType+"（WORKFLOW 属工作流，gojob 用依赖触发表达前后置关系）")
	}
	teType := strings.ToUpper(deref(p.TimeExpressionType, "CRON"))
	switch teType {
	case "CRON", "FIXED_RATE", "FIXED_DELAY", "DAILY_TIME_INTERVAL", "API":
	default:
		return nil, errField("timeExpressionType", "暂不支持时间类型 "+teType)
	}
	// 处理器类型按兼容协议的 ProcessorType 分流（见 normalizeProcessorType）。
	pType, processor, jobParams, perr := normalizeProcessorType(
		strings.ToUpper(deref(p.ProcessorType, "BUILT_IN")),
		strings.TrimSpace(deref(p.ProcessorInfo, "")),
		deref(p.JobParams, ""), deref(p.InstanceParams, ""))
	if perr != nil {
		return nil, perr
	}
	expr := strings.TrimSpace(deref(p.TimeExpression, ""))
	next, err := scheduler.NextByType(teType, expr, store.Now(), store.Now())
	if err != nil {
		return nil, errField("timeExpression", err.Error())
	}

	retry := 3
	if p.TaskRetryNum != nil && *p.TaskRetryNum > 0 {
		retry = *p.TaskRetryNum
	} else if p.InstanceRetryNum != nil && *p.InstanceRetryNum > 0 {
		retry = *p.InstanceRetryNum
	}
	concurrency := 5
	if p.Concurrency != nil && *p.Concurrency > 0 {
		concurrency = *p.Concurrency
	}
	limit := 3
	if p.MaxInstanceNum != nil && *p.MaxInstanceNum >= 0 {
		limit = *p.MaxInstanceNum // 兼容协议用 0 表示不限制，与 gojob 语义一致
	}
	skipWhenRunning := limit == 1 // 原任务"同时仅一个实例"等价于执行中跳过
	if p.SkipWhenRunning != nil {
		skipWhenRunning = *p.SkipWhenRunning // gojob 导出带回时以显式值为准
	}
	dispStrategy, derr := dispatchStrategyOrDefault(derefInt(p.DispatchStrategy, 0))
	if derr != nil {
		return nil, errField("dispatchStrategy", derr.Error())
	}
	logLevel := strings.ToUpper(deref(p.LogLevel, "INFO"))
	switch logLevel {
	case "DEBUG", "INFO", "WARN", "ERROR", "OFF":
	default:
		return nil, errField("logLevel", "未知日志级别 "+logLevel)
	}

	job := &store.JobInfo{
		AppId: jc.appId, JobName: jobName,
		JobDescription: deref(p.JobDescription, ""),
		Processor:      processor, TimeExpression: expr,
		TimeExpressionType: teType,
		ExecuteType:        strings.ToUpper(deref(p.ExecuteType, "STANDALONE")),
		ProcessorType:      pType,
		DesignatedWorkers:  deref(p.DesignatedWorkers, ""),
		MaxWorkerCount:     derefInt(p.MaxWorkerCount, 0),
		DispatchStrategy:   dispStrategy, DispatchStrategyConfig: deref(p.DispatchStrategyConfig, ""),
		JobParams:      jobParams,
		InstanceParams: deref(p.InstanceParams, ""),
		MaxRetryTimes:  retry, Concurrency: concurrency,
		InstanceRetryNum: derefInt(p.InstanceRetryNum, 3),
		InstanceNumLimit: limit, SkipWhenRunning: skipWhenRunning,
		Enable:          p.Enable == nil || *p.Enable,
		LogLevel:        logLevel,
		NextTriggerTime: next,
		CreateTime:      store.Now(), ModifyTime: store.Now(),
	}
	return job, nil
}

func derefInt(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

// toJobDTO 反向映射：把 gojob 任务导出成通用任务包的 JobDTO（camelCase）。
// 导出的是"兼容协议能看懂的那部分 + gojob 扩展字段"，因此这份 JSON 既可以直接粘回
// 本控制台的导入框（round-trip），也可以喂给兼容协议（它会忽略不认识的键）。
func toJobDTO(j *store.JobInfo) protocol.JobDTO {
	disp := j.DispatchStrategy
	if disp == 0 {
		disp = protocol.DispatchHealthFirst
	}
	out := protocol.JobDTO{
		JobName: strPtr(j.JobName), JobDescription: strPtr(j.JobDescription),
		TimeExpressionType:     strPtr(orDefaultStr(j.TimeExpressionType, "CRON")),
		TimeExpression:         strPtr(j.TimeExpression),
		ExecuteType:            strPtr(orDefaultStr(j.ExecuteType, "STANDALONE")),
		ProcessorType:          strPtr(orDefaultStr(j.ProcessorType, "BUILT_IN")),
		ProcessorInfo:          strPtr(j.Processor),
		JobParams:              strPtr(j.JobParams),
		MaxInstanceNum:         &j.InstanceNumLimit,
		Concurrency:            &j.Concurrency,
		InstanceRetryNum:       &j.InstanceRetryNum,
		TaskRetryNum:           &j.MaxRetryTimes,
		DesignatedWorkers:      strPtr(j.DesignatedWorkers),
		MaxWorkerCount:         &j.MaxWorkerCount,
		DispatchStrategy:       &disp,
		DispatchStrategyConfig: strPtr(j.DispatchStrategyConfig),
		Enable:                 &j.Enable,
		InstanceParams:         strPtr(j.InstanceParams),
		SkipWhenRunning:        &j.SkipWhenRunning,
		LogLevel:               strPtr(orDefaultStr(j.LogLevel, "INFO")),
	}
	return out
}

func errField(field, reason string) error {
	return &importError{field: field, reason: reason}
}

type importError struct {
	field  string
	reason string
}

func (e *importError) Error() string { return e.field + ": " + e.reason }

func (s *Server) importJobs(c *gin.Context) {
	req, err := bind[protocol.ImportJobsReq](c)
	if err != nil || req.AppId == "" {
		writeErr(c, protocol.CodeBadReq, "bad request: 需要 {app_id, jobs:[...]}")
		return
	}
	if _, err := s.appOf(req.AppId); err != nil {
		writeErr(c, protocol.CodeBadReq, err.Error())
		return
	}
	jc := &jobConverter{appId: req.AppId}
	resp := protocol.ImportJobsResp{Total: len(req.Jobs), Items: make([]protocol.ImportJobItem, 0, len(req.Jobs))}
	for i := range req.Jobs {
		pj := &req.Jobs[i]
		item := protocol.ImportJobItem{JobName: strOf(pj.JobName)}
		job, cerr := jc.convert(pj)
		if cerr != nil {
			item.Reason = cerr.Error()
		} else if err := s.st.Create(job); err != nil {
			item.Reason = "create: " + err.Error()
		} else {
			item.Success, item.JobId = true, job.ID
			s.sched.Reschedule(job.ID)
		}
		if item.Success {
			resp.Succeeded++
		} else {
			resp.Failed++
		}
		resp.Items = append(resp.Items, item)
	}
	writeOK(c, resp)
}

func strOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// exportJobs 导出任务包，三种选法，data 的形状跟着变：
//
//	?job_id=7         → 单个对象（保持与既有调用方/导入框 round-trip 一致）
//	?job_ids=7,8,9    → 数组，控制台多选后的批量导出
//	?app_id=demo_app  → 该项目全部任务的数组（整项目备份 / 迁到另一套环境）
//
// 读侧的 GET 一律不进 authorize 中间件，所以**批量导出必须在这里自己判项目授权**：
// 一次就能拉走整个项目的任务配置，等于把"未授权项目可读"从一条放大成一批。
// 判定按写级授权（canAccessApp）执行——导出通常由项目管理员发起。
func (s *Server) exportJobs(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, protocol.CodeAuthErr, "not logged in")
		return
	}
	deny := func(appId string) {
		writeErr(c, protocol.CodeAuthErr, "permission denied for app "+appId)
	}
	ids := splitIDs(c.Query("job_ids"))
	// 形状按"怎么问的"决定，不按"结果几条"决定：job_id= 一定是对象（既有调用方与导入框
	// 靠它），job_ids= / app_id= 一定是数组——否则只勾选一条任务的批量导出会突然换了形状。
	single := false
	if id, err := parseID(c.Query("job_id")); err == nil && id > 0 {
		ids = append(ids, id)
		single = len(ids) == 1
	}
	var jobs []*store.JobInfo
	switch {
	case len(ids) > 0:
		for _, id := range ids {
			job, err := s.st.Get[store.JobInfo](id)
			if err != nil {
				writeErr(c, protocol.CodeNotFound, fmt.Sprintf("job %d: %v", id, err))
				return
			}
			if u.Role != "SUPER_ADMIN" && !s.canAccessApp(c, job.AppId) {
				deny(job.AppId)
				return
			}
			jobs = append(jobs, job)
		}
	case c.Query("app_id") != "":
		appId := c.Query("app_id")
		if u.Role != "SUPER_ADMIN" && !s.canAccessApp(c, appId) {
			deny(appId)
			return
		}
		all, err := s.st.List[store.JobInfo](-1, 0, "id asc", map[string]any{"app_id": appId})
		if err != nil {
			writeErr(c, protocol.CodeErr, err.Error())
			return
		}
		for i := range all {
			jobs = append(jobs, &all[i])
		}
	default:
		writeErr(c, protocol.CodeBadReq, "需要 job_id / job_ids / app_id 三者之一")
		return
	}

	out := make([]protocol.JobDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobDTO(j))
	}
	// 单任务导出保持"对象"而非"数组"：与既有导入框、外部脚本的期望一致
	if single {
		writeOK(c, out[0])
		return
	}
	writeOK(c, out)
}

// splitIDs 解析 "1,2,3" 形式的 id 清单；非法片段直接忽略（前端多选可能带空串）。
func splitIDs(s string) []uint64 {
	ids := make([]uint64, 0, 4)
	for _, part := range strings.Split(s, ",") {
		if id, err := parseID(strings.TrimSpace(part)); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}
