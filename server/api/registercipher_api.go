// 注册口令加密的接口层。worker 数据面（/worker/*）与 OpenAPI 兼容层（/openApi/*）
// 共用同一套握手，差别只在 principal：worker 绑 instance_id，authApp 绑 appName。
//
// 握手三步（服务端配了 key 时）：
//
//  1. POST /worker/challenge   {app_id, instance_id}      → {challenge, expires_in, schemes}
//  2. 本地 AES-256-GCM 加密 (app_id, instance_id, challenge, password)
//  3. POST /worker/register    {..., auth_scheme:"aes-gcm", kid, iv, challenge, ciphertext}
//
// 没配 key 时第 1 步直接报"未启用"，第 3 步走原来的明文口令路径，行为与引入本功能前一致。
// 配了 key 但没开强制时两条路都通（便于分批升级 worker）；开了强制则明文回 428 并附上
// 一张现领的 challenge，客户端省一次往返。
package api

import (
	"github.com/gin-gonic/gin"

	v2 "encoding/json/v2"

	"gojob/internal/protocol"
	"gojob/server/auth"
)

// writeCodeData 带 data 的响应：428 这类"不是失败，是要你换个送法"的码需要把 challenge 一起给。
func writeCodeData[T any](c *gin.Context, code int, msg string, data T) {
	bs, err := v2.Marshal(protocol.Resp[T]{Code: code, Message: msg, Data: data})
	if err != nil {
		c.AbortWithStatus(500)
		return
	}
	c.Data(200, jsonCT, bs)
}

// registerSchemes 服务端当前接受的注册方式，随每个注册的响应一起回，
// 让"两侧只配了一半"这类错配在客户端就能看出来。
func (s *Server) registerSchemes() []string {
	rc := s.rcrypto
	if rc == nil {
		rc = &auth.RegisterCrypto{} // nil 接收者的 Schemes() 就是 ["plain"]
	}
	return rc.Schemes()
}

// clearRegister 把一次注册的"口令送法"还原成明文口令。
// principal 参与 challenge 绑定与密文校验，所以冒用别人的 instance_id 在这一步就断掉。
func (s *Server) clearRegister(appId, principal string, in auth.Clear) auth.ClearOutcome {
	if s.rcrypto == nil {
		// 功能未开：只认明文。客户端配了 key 而服务端没配 ⇒ 说清楚是谁没配，别报成口令错。
		if in.Scheme == auth.RegisterSchemeAesGcm {
			return auth.ClearOutcome{Err: auth.ErrServerNoKey}
		}
		return auth.ClearOutcome{OK: true, Password: in.Password}
	}
	return s.rcrypto.Clear(appId, principal, in)
}

// writeClearReject 还原失败的统一出口。Challenge 非空 = "请改用加密，这是新凭证"（428），
// 其余是明确拒绝（401）——两者的文案互不相同，且都不含"wrong password"字样。
func (s *Server) writeClearReject(c *gin.Context, out auth.ClearOutcome) {
	resp := protocol.ChallengeResp{ExpiresIn: int(auth.ChallengeTTL.Seconds()), Schemes: s.registerSchemes()}
	if out.Challenge != "" {
		resp.Challenge = out.Challenge
		writeCodeData(c, protocol.CodeChallengeRequired, out.Err.Error(), resp)
		return
	}
	resp.ExpiresIn = 0
	writeCodeData(c, protocol.CodeAuthErr, "register rejected: "+out.Err.Error(), resp)
}

// issueChallengeJson 握手第一步的公共部分：校验入参、按项目+身份签一张一次性凭证。
// principal 是"这张 challenge 只许这个身份消费"，worker 侧传 instance_id，
// OpenAPI 侧传 ""（调用方没有实例身份）。
func (s *Server) issueChallengeJson(c *gin.Context, appId, principal string) {
	if appId == "" {
		writeErr(c, protocol.CodeBadReq, "bad request: app_id required")
		return
	}
	if s.rcrypto == nil {
		writeErr(c, protocol.CodeBadReq, "本服务端未启用注册加密（GOJOB_WORKER_AES_KEYS 未配置），请直接明文注册")
		return
	}
	writeOK(c, protocol.ChallengeResp{
		Challenge: s.rcrypto.Chal.Issue(appId, principal),
		ExpiresIn: int(auth.ChallengeTTL.Seconds()),
		Schemes:   s.rcrypto.Schemes(),
	})
}

// workerChallenge 领一张一次性 challenge。
//
// 故意**不校验项目是否存在**：这一步对任何人开放，若"项目不存在"回不同响应，
// 这里就成了一台 app_id 枚举机；项目存不存在只在真正注册那一刻才看得出来。
// 领了不用也没有代价——challenge 不落库，只有被消费时才写一行（且随自身过期被清理）。
func (s *Server) workerChallenge(c *gin.Context) {
	req, err := bind[protocol.ChallengeReq](c)
	if err != nil || req.InstanceId == "" {
		writeErr(c, protocol.CodeBadReq, "bad request: app_id and instance_id required")
		return
	}
	s.issueChallengeJson(c, req.AppId, req.InstanceId)
}

// pjChallenge 兼容协议侧的同一步（principal 为空：OpenAPI 调用方只有项目身份）。
func (s *Server) pjChallenge(c *gin.Context) {
	req, err := bind[struct {
		AppName string `json:"appName"`
	}](c)
	if err != nil {
		pjFail(c, "bad challenge request")
		return
	}
	if req.AppName == "" {
		pjFail(c, "bad challenge request: appName required")
		return
	}
	if s.rcrypto == nil {
		// 兼容信封里说清楚是谁没配：这时 authApp 只需明文，客户端不该白跑一趟握手。
		pjOK(c, pjChallengeData{Schemes: s.registerSchemes()})
		return
	}
	pjOK(c, pjChallengeData{
		Challenge: s.rcrypto.Chal.Issue(req.AppName, ""),
		ExpiresIn: int(auth.ChallengeTTL.Seconds()),
		Schemes:   s.rcrypto.Schemes(),
	})
}
