package api

import (
	"bytes"
	"io"
	"strconv"

	v2 "encoding/json/v2"
	"github.com/gin-gonic/gin"

	"gojob/internal/protocol"
	"gojob/server/store"
)

// respCapture 包住 gin.ResponseWriter，抓下响应体以解析业务 code。
type respCapture struct {
	gin.ResponseWriter
	body *bytes.Buffer
}

func (w *respCapture) Write(b []byte) (int, error) {
	// 仅保留前 512 字节足够解析 code，避免缓冲大响应。
	if w.body.Len() < 512 {
		w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// opAudit 记录管理面写操作（POST /openapi/*）。登录/登出由其专用逻辑处理，跳过。
func (s *Server) opAudit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != "POST" {
			c.Next()
			return
		}
		path := c.FullPath()
		if path == "/openapi/user/login" || path == "/openapi/user/logout" {
			c.Next()
			return
		}

		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, _ = io.ReadAll(io.LimitReader(c.Request.Body, 4096))
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes)) // 交还给下游 handler
		}

		rec := &respCapture{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
		c.Writer = rec
		// appId 必须在 c.Next() **之前**落到 context 上：下游的 authorize 中间件要靠它判
		// "这个 ADMIN 有没有该项目的写权限"。放在 Next 之后取到的是空串，项目授权会整体失效
		// （实测 job/create 带着 app_id 也被拒成 "no permission for app "）。
		appId := s.auditAppID(bodyBytes, c)
		c.Set("appId", appId)
		// 原始请求体也交给下游：authorize 要靠 job_id/instance_id 反查真实归属项目
		// （只信客户端自报的 app_id 就等于让人自己声明要动哪个项目）。
		c.Set("reqBody", bodyBytes)
		c.Next()

		action := stringsTrimPrefix(path, "/openapi/")
		code := parseCode(rec.body.Bytes())
		u := currentUser(c)
		role := ""
		user := "-"
		if u != nil {
			role, user = u.Role, u.Username
		}
		s.st.Create(&store.OperationLog{
			Username: user, Role: role, Action: action, AppId: appId,
			Method: c.Request.Method, Path: path, Params: sanitizeParams(bodyBytes),
			ResultCode: code, Success: code == protocol.CodeOK,
			IP: clientIP(c), CreateTime: store.Now(),
		})
	}
}

func stringsTrimPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

// auditAppID 判定这次写操作**实际**作用于哪个项目，顺序是"能查到实物的就以实物为准"：
//  1. 请求体里有 job_id / instance_id —— 反查库里那条记录归属的 app，客户端乱报也没用；
//  2. 否则取请求体里的 app_id；
//  3. 再回退到 query 上的 app_id。
//
// 第 1 步不只是"体贴前端"：项目级授权（ADMIN 的 AppScopes）判的就是这个 app_id，
// 如果只信客户端自报，被授权 A 项目的账号只要声明 app_id=A 就能去停 B 项目的任务。
func (s *Server) auditAppID(body []byte, c *gin.Context) string {
	var m map[string]any
	if len(body) > 0 {
		if err := v2.Unmarshal(body, &m); err == nil {
			if appId, ok := s.appOfTarget(m); ok {
				return appId
			}
			if s, ok := m["app_id"].(string); ok && s != "" {
				return s
			}
		}
	}
	return c.Query("app_id")
}

// appOfTarget 按请求里的 job_id / instance_id 反查真实归属项目。
// 查不到（记录不存在或 id 类型不对）时返回 ok=false，交给下游 handler 报 404，
// 这里不猜项目：授权宁可拒得太严，不能拒错对象。
func (s *Server) appOfTarget(m map[string]any) (string, bool) {
	if id := jsonUint(m, "job_id"); id != 0 {
		var job store.JobInfo
		if err := s.st.DB.Select("app_id").First(&job, id).Error; err == nil {
			return job.AppId, true
		}
	}
	if id := jsonUint(m, "instance_id"); id != 0 {
		var inst store.JobInstance
		if err := s.st.DB.Select("app_id").First(&inst, id).Error; err == nil {
			return inst.AppId, true
		}
	}
	return "", false
}

func jsonUint(m map[string]any, key string) uint64 {
	switch v := m[key].(type) {
	case float64:
		return uint64(v)
	case string:
		n, _ := strconv.ParseUint(v, 10, 64)
		return n
	}
	return 0
}

// sanitizeParams 存请求体摘要，剔除口令字段。
func sanitizeParams(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var m map[string]any
	if err := v2.Unmarshal(body, &m); err != nil {
		return trunc(string(body), 1024)
	}
	for k := range m {
		if k == "password" || k == "old_password" || k == "new_password" {
			m[k] = "***"
		}
	}
	out, err := v2.Marshal(m)
	if err != nil {
		return ""
	}
	return trunc(string(out), 1024)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "...(truncated)"
	}
	return s
}

func parseCode(body []byte) int {
	var r struct {
		Code int `json:"code"`
	}
	if err := v2.Unmarshal(body, &r); err != nil {
		return -1
	}
	return r.Code
}

// ---------- 审计查询（ADMIN 及以上） ----------

func pageArgs(c *gin.Context) (limit, offset int) {
	limit = 20
	if n, err := strconv.Atoi(c.Query("size")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	if n, err := strconv.Atoi(c.Query("page")); err == nil && n > 0 {
		offset = (n - 1) * limit
	}
	return
}

func (s *Server) loginLogs(c *gin.Context) {
	limit, offset := pageArgs(c)
	q := s.st.DB.Model(&store.LoginLog{})
	if u := c.Query("username"); u != "" {
		q = q.Where("username = ?", u)
	}
	var total int64
	q.Count(&total)
	var rows []store.LoginLog
	q.Order("id desc").Limit(limit).Offset(offset).Find(&rows)
	writeOK(c, gin.H{"total": total, "rows": rows})
}

func (s *Server) operationLogs(c *gin.Context) {
	limit, offset := pageArgs(c)
	q := s.st.DB.Model(&store.OperationLog{})
	if u := c.Query("username"); u != "" {
		q = q.Where("username = ?", u)
	}
	if a := c.Query("action"); a != "" {
		q = q.Where("action LIKE ?", a+"%")
	}
	if app := c.Query("app_id"); app != "" {
		q = q.Where("app_id = ?", app)
	}
	var total int64
	q.Count(&total)
	var rows []store.OperationLog
	q.Order("id desc").Limit(limit).Offset(offset).Find(&rows)
	writeOK(c, gin.H{"total": total, "rows": rows})
}
