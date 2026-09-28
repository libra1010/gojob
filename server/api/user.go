package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"gojob/internal/protocol"
	"gojob/server/auth"
	"gojob/server/store"
)

// requireLogin 保护管理面接口：Authorization: Bearer <token> 或 X-GoJob-Token 头。
// 载入用户记录并校验状态，账号被禁用则立即失效。
func (s *Server) requireLogin(c *gin.Context) {
	tk := bearerToken(c)
	claims, ok := s.am.Claims(tk)
	if !ok {
		writeErr(c, protocol.CodeAuthErr, "not logged in or token expired")
		c.Abort()
		return
	}
	var u store.UserInfo
	// 这一次读用户行同时完成三件事：状态校验、角色装载、吊销基线比对——
	// 所以改密/禁用/删除在**所有**副本上都是即刻生效，不依赖任何进程内缓存。
	if err := s.st.DB.Where("username = ?", claims.Subject).First(&u).Error; err != nil || u.Status != "ACTIVE" {
		s.am.Revoke(tk)
		writeErr(c, protocol.CodeAuthErr, "account disabled or removed")
		c.Abort()
		return
	}
	if s.am.Stale(claims, u.TokenEpoch) {
		writeErr(c, protocol.CodeAuthErr, "session revoked, please log in again")
		c.Abort()
		return
	}
	if fresh := s.am.RenewIfStale(claims, u.TokenEpoch); fresh != "" {
		// 活动中的会话经响应头续期，前端拦截器负责换掉本地存的旧票
		c.Header(auth.RenewedHeader, fresh)
	}
	c.Set("user", u.Username)
	c.Set("userObj", &u)
	c.Set("token", tk)
	c.Next()
}

func bearerToken(c *gin.Context) string {
	tk := c.GetHeader("X-GoJob-Token")
	if tk == "" {
		tk = strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	}
	return tk
}

// clientIP 取来源 IP（优先反向代理头）。
func clientIP(c *gin.Context) string {
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if x := c.GetHeader("X-Real-IP"); x != "" {
		return x
	}
	return c.ClientIP()
}

func (s *Server) login(c *gin.Context) {
	req, err := bind[protocol.LoginReq](c)
	if err != nil || req.Username == "" {
		writeErr(c, protocol.CodeBadReq, "bad request")
		return
	}
	ip, ua := clientIP(c), c.Request.UserAgent()
	reason := s.doLogin(req.Username, req.Password, ip, ua)
	if reason != "" {
		writeErr(c, protocol.CodeAuthErr, "wrong username or password")
		return
	}
	var user store.UserInfo
	if err := s.st.DB.Where("username = ?", req.Username).First(&user).Error; err != nil {
		writeErr(c, protocol.CodeErr, "issue session failed: "+err.Error())
		return
	}
	// 口令已验过，这里再失败说明用户行刚被删掉或库出故障：如实报错，
	// 别回一张"看着像登录成功、下一次请求就 401"的票。
	tk, err := s.am.Issue(user.Username)
	if err != nil {
		writeErr(c, protocol.CodeErr, "issue session failed: "+err.Error())
		return
	}
	writeOK(c, protocol.LoginResp{
		Token:    tk,
		Username: user.Username,
		Nickname: user.Nickname,
		Role:     user.Role,
	})
}

// doLogin 校验口令并写登录日志；返回空串表示成功，否则为失败原因。
func (s *Server) doLogin(username, password, ip, ua string) string {
	var user store.UserInfo
	reason := ""
	ok := false
	switch err := s.st.DB.Where("username = ?", username).First(&user).Error; {
	case err != nil:
		reason = "user not found"
	case user.Status != "ACTIVE":
		reason = "account disabled"
	case !auth.CheckPassword(user.Password, password):
		reason = "wrong password"
	default:
		ok = true
	}
	s.st.Create(&store.LoginLog{Username: username, IP: ip, UserAgent: truncUA(ua),
		Success: ok, Reason: reason, LoginTime: store.Now()})
	if !ok {
		return reason
	}
	return ""
}

func truncUA(ua string) string {
	if len(ua) > 256 {
		return ua[:256]
	}
	return ua
}

func (s *Server) logout(c *gin.Context) {
	s.am.Revoke(bearerToken(c))
	writeOK(c, struct{}{})
}

func (s *Server) me(c *gin.Context) {
	u, _ := c.Get("userObj")
	user := u.(*store.UserInfo)
	writeOK(c, struct {
		Username  string   `json:"username"`
		Nickname  string   `json:"nickname"`
		Role      string   `json:"role"`
		Status    string   `json:"status"`
		AppScopes []string `json:"app_scopes"`
	}{user.Username, user.Nickname, user.Role, user.Status, scopeList(user.AppScopes)})
}
