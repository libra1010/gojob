package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"gojob/internal/protocol"
	"gojob/server/auth"
	"gojob/server/store"
)

const timeLayout = "2006-01-02 15:04:05"

// scopeList 把逗号分隔的 AppScopes 解析成数组。
func scopeList(scopes string) []string {
	if strings.TrimSpace(scopes) == "" {
		return []string{}
	}
	parts := strings.Split(scopes, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func currentUser(c *gin.Context) *store.UserInfo {
	if v, ok := c.Get("userObj"); ok {
		return v.(*store.UserInfo)
	}
	return nil
}

func (s *Server) isSuperAdmin(c *gin.Context) bool {
	u := currentUser(c)
	return u != nil && u.Role == "SUPER_ADMIN"
}

// canAccessApp 判定当前用户是否可对某 app 执行写操作。
// SUPER_ADMIN 全量；ADMIN 限 AppScopes（含 "*"）；READONLY 一律拒绝。
func (s *Server) canAccessApp(c *gin.Context, appId string) bool {
	u := currentUser(c)
	if u == nil {
		return false
	}
	switch u.Role {
	case "SUPER_ADMIN":
		return true
	case "ADMIN":
		if u.AppScopes == "*" {
			return true
		}
		for _, a := range scopeList(u.AppScopes) {
			if a == appId {
				return true
			}
		}
		return false
	default: // READONLY
		return false
	}
}

// requireRole 限制路由仅指定角色可访问。
func (s *Server) requireRole(roles ...string) gin.HandlerFunc {
	allow := func(r string) bool {
		for _, x := range roles {
			if x == r {
				return true
			}
		}
		return false
	}
	return func(c *gin.Context) {
		u := currentUser(c)
		if u == nil || !allow(u.Role) {
			writeErr(c, protocol.CodeAuthErr, "permission denied")
			c.Abort()
			return
		}
		c.Next()
	}
}

// authorize 统一的管理面鉴权，接在 requireLogin、opAudit 之后（依赖其写入的 appId）。
//   - 自助改密：任何登录用户。
//   - GET：任何登录用户可读（READONLY 允许）。
//   - 写操作：READONLY 一律拒绝；
//     项目写操作需命中 AppScopes（无 app 归属的写操作仅 SUPER_ADMIN）。
func (s *Server) authorize() gin.HandlerFunc {
	superOnly := map[string]bool{
		"/openapi/app/create": true, "/openapi/app/modify": true, "/openapi/app/delete": true,
		"/openapi/job/import": true,
	}
	appScoped := map[string]bool{
		"/openapi/job/create": true, "/openapi/job/modify": true, "/openapi/job/delete": true,
		"/openapi/job/operate": true,
		// 停/启某台 worker 接单：app_id 直接来自请求体，同样是项目级授权范围
		"/openapi/worker/acceptTasks": true,
	}
	return func(c *gin.Context) {
		path := c.FullPath()
		if path == "/openapi/user/changePassword" || c.Request.Method == "GET" {
			c.Next()
			return
		}
		u := currentUser(c)
		if u == nil || u.Role == "READONLY" {
			writeErr(c, protocol.CodeAuthErr, "read-only account cannot perform write operations")
			c.Abort()
			return
		}
		if superOnly[path] {
			if u.Role != "SUPER_ADMIN" {
				writeErr(c, protocol.CodeAuthErr, "permission denied")
				c.Abort()
				return
			}
			c.Next()
			return
		}
		if appScoped[path] || strings.HasPrefix(path, "/openapi/instance") {
			appId := c.GetString("appId")
			if appId == "" {
				appId = c.Query("app_id")
			}
			if u.Role != "SUPER_ADMIN" {
				if appId == "" || !s.scopeHas(u, appId) {
					writeErr(c, protocol.CodeAuthErr, "no permission for app "+appId)
					c.Abort()
					return
				}
			}
		}
		c.Next()
	}
}

func (s *Server) scopeHas(u *store.UserInfo, appId string) bool {
	if u.AppScopes == "*" {
		return true
	}
	for _, a := range scopeList(u.AppScopes) {
		if a == appId {
			return true
		}
	}
	return false
}

// ---------- 改密 / 重置密码 ----------

func (s *Server) changePassword(c *gin.Context) {
	req, err := bind[struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}](c)
	if err != nil || req.NewPassword == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	u := currentUser(c)
	if !auth.CheckPassword(u.Password, req.OldPassword) {
		writeErr(c, protocol.CodeAuthErr, "wrong old password")
		return
	}
	s.setPassword(c, u.Username, req.NewPassword)
	writeOK(c, struct{}{})
}

func (s *Server) resetPassword(c *gin.Context) {
	req, err := bind[struct {
		Username    string `json:"username"`
		NewPassword string `json:"new_password"`
	}](c)
	if err != nil || req.Username == "" || req.NewPassword == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	s.setPassword(c, req.Username, req.NewPassword)
	writeOK(c, struct{}{})
}

// setPassword 更新口令并吊销该用户全部会话（改密后旧 token 立即失效）。
func (s *Server) setPassword(c *gin.Context, username, plain string) {
	hash, err := auth.HashPassword(plain)
	if err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	s.st.DB.Model(&store.UserInfo{}).Where("username = ?", username).
		Updates(map[string]any{"password": hash, "modify_time": store.Now()})
	s.am.RevokeUser(username)
}

// ---------- 用户 CRUD（仅 SUPER_ADMIN） ----------

type userReq struct {
	Username  string   `json:"username"`
	Password  string   `json:"password"`
	Nickname  string   `json:"nickname"`
	Role      string   `json:"role"`
	Status    string   `json:"status"`
	AppScopes []string `json:"app_scopes"`
}

func normalizeRole(r string) string {
	switch r {
	case "SUPER_ADMIN", "ADMIN", "READONLY":
		return r
	default:
		return "READONLY"
	}
}

func (s *Server) listUsers(c *gin.Context) {
	var users []store.UserInfo
	s.st.DB.Order("id asc").Find(&users)
	type row struct {
		ID         uint64   `json:"id"`
		Username   string   `json:"username"`
		Nickname   string   `json:"nickname"`
		Role       string   `json:"role"`
		Status     string   `json:"status"`
		AppScopes  []string `json:"app_scopes"`
		CreateTime string   `json:"create_time"`
	}
	out := make([]row, 0, len(users))
	for _, u := range users {
		out = append(out, row{u.ID, u.Username, u.Nickname, u.Role, u.Status,
			scopeList(u.AppScopes), u.CreateTime.Format(timeLayout)})
	}
	writeOK(c, out)
}

func (s *Server) createUser(c *gin.Context) {
	req, err := bind[userReq](c)
	if err != nil || req.Username == "" || req.Password == "" {
		writeErr(c, protocol.CodeBadReq, "username and password required")
		return
	}
	var n int64
	s.st.DB.Model(&store.UserInfo{}).Where("username = ?", req.Username).Count(&n)
	if n > 0 {
		writeErr(c, protocol.CodeConflict, "username already exists")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	u := store.UserInfo{Username: req.Username, Password: hash, Nickname: req.Nickname,
		Role: normalizeRole(req.Role), Status: "ACTIVE", AppScopes: strings.Join(req.AppScopes, ","),
		CreateTime: store.Now(), ModifyTime: store.Now()}
	if err := s.st.Create(&u); err != nil {
		writeErr(c, protocol.CodeErr, err.Error())
		return
	}
	writeOK(c, u.ID)
}

func (s *Server) modifyUser(c *gin.Context) {
	req, err := bind[userReq](c)
	if err != nil || req.Username == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	caller := currentUser(c)
	var u store.UserInfo
	if err := s.st.DB.Where("username = ?", req.Username).First(&u).Error; err != nil {
		writeErr(c, protocol.CodeNotFound, "user not found")
		return
	}
	updates := map[string]any{"modify_time": store.Now()}
	updates["nickname"] = req.Nickname
	updates["app_scopes"] = strings.Join(req.AppScopes, ",")
	newRole := normalizeRole(req.Role)
	// 防止超管把自己降级/禁用后无人可管理；至少保留一个启用的超管。
	lastSuper := s.lastActiveSuperAdmin(&u)
	if lastSuper && (newRole != "SUPER_ADMIN" || req.Status == "DISABLED") {
		writeErr(c, protocol.CodeBadReq, "cannot demote or disable the last active super admin")
		return
	}
	updates["role"] = newRole
	if req.Status == "DISABLED" {
		updates["status"] = "DISABLED"
		s.am.RevokeUser(u.Username) // 禁用即时踢下线
	} else {
		updates["status"] = "ACTIVE"
	}
	// 非超管不能授予超管（上面已限超管路由，此处冗余防御）
	if caller != nil && caller.Role != "SUPER_ADMIN" && newRole == "SUPER_ADMIN" {
		writeErr(c, protocol.CodeAuthErr, "only super admin can grant super admin")
		return
	}
	s.st.DB.Model(&u).Updates(updates)
	writeOK(c, struct{}{})
}

func (s *Server) setUserStatus(c *gin.Context) {
	req, err := bind[struct {
		Username string `json:"username"`
		Status   string `json:"status"`
	}](c)
	if err != nil || req.Username == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	status := "ACTIVE"
	if req.Status == "DISABLED" {
		status = "DISABLED"
	}
	var u store.UserInfo
	if err := s.st.DB.Where("username = ?", req.Username).First(&u).Error; err != nil {
		writeErr(c, protocol.CodeNotFound, "user not found")
		return
	}
	if status == "DISABLED" && s.lastActiveSuperAdmin(&u) {
		writeErr(c, protocol.CodeBadReq, "cannot disable the last active super admin")
		return
	}
	s.st.DB.Model(&u).Updates(map[string]any{"status": status, "modify_time": store.Now()})
	if status == "DISABLED" {
		s.am.RevokeUser(u.Username)
	}
	writeOK(c, struct{}{})
}

func (s *Server) deleteUser(c *gin.Context) {
	req, err := bind[struct {
		Username string `json:"username"`
	}](c)
	if err != nil || req.Username == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	var u store.UserInfo
	if err := s.st.DB.Where("username = ?", req.Username).First(&u).Error; err != nil {
		writeOK(c, struct{}{}) // 幂等删除
		return
	}
	if s.lastActiveSuperAdmin(&u) {
		writeErr(c, protocol.CodeBadReq, "cannot delete the last active super admin")
		return
	}
	s.st.DB.Delete(&u)
	s.am.RevokeUser(u.Username)
	writeOK(c, struct{}{})
}

// lastActiveSuperAdmin 判断 u 是否为唯一仍在世的启用超管。
func (s *Server) lastActiveSuperAdmin(u *store.UserInfo) bool {
	if u.Role != "SUPER_ADMIN" || u.Status != "ACTIVE" {
		return false
	}
	var n int64
	s.st.DB.Model(&store.UserInfo{}).
		Where("role = ? AND status = ? AND username <> ?", "SUPER_ADMIN", "ACTIVE", u.Username).
		Count(&n)
	return n == 0
}
