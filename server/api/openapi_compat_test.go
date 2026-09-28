// authApp 的口令校验：gojob 刻意只接受明文 password（库里没有 md5 等价物），
// 兼容协议官方 SDK 的 encryptedPassword(md5) 形态必须被明确拒绝。
package api

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"gojob/server/auth"
	"gojob/server/store"
)

const pjAuthPlain = "demo123"

func newPjServer(t *testing.T) *Server {
	t.Helper()
	s := newDepServer(t)
	now := store.Now()
	hash, err := auth.HashPassword(pjAuthPlain)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.st.DB.Create(&store.AppInfo{AppId: "demo_app", AppName: "demo_app",
		Password: hash, CreateTime: now, ModifyTime: now}).Error; err != nil {
		t.Fatal(err)
	}
	return s // s.pj 由 newDepServer 装的 auth.Set 提供
}

// pjAuthCall 直接打一次 authApp，返回解开的响应信封。
func pjAuthCall(t *testing.T, s *Server, body string) (bool, string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/openApi/authApp", s.pjAuthApp)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/openApi/authApp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Token string `json:"token"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad envelope %q: %v", w.Body.String(), err)
	}
	return resp.Success, resp.Data.Token, resp.Message
}

func TestPjAuthAppAcceptsPlaintext(t *testing.T) {
	s := newPjServer(t)
	ok, token, msg := pjAuthCall(t, s, `{"appName":"demo_app","password":"demo123","encryptType":0}`)
	if !ok || token == "" {
		t.Fatalf("明文口令应鉴权通过，got success=%v token=%q msg=%q", ok, token, msg)
	}
	if appId, found := s.pj.Lookup(token); !found || appId != "demo_app" {
		t.Fatal("签发的 token 应可用")
	}
}

func TestPjAuthAppRejects(t *testing.T) {
	s := newPjServer(t)
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"md5-only（官方 SDK encryptType=1）",
			`{"appName":"demo_app","encryptedPassword":"0d4e1b6f0a6a8e5b9f9e0e2f9a3e2e6f","encryptType":1}`,
			"rejected by design"},
		{"把 md5 当明文提交",
			`{"appName":"demo_app","password":"0d4e1b6f0a6a8e5b9f9e0e2f9a3e2e6f"}`,
			"password mismatch"},
		{"明文口令错误", `{"appName":"demo_app","password":"wrong"}`, "password mismatch"},
		{"项目不存在", `{"appName":"nope","password":"demo123"}`, "app not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, token, msg := pjAuthCall(t, s, tc.body)
			if ok || token != "" {
				t.Fatalf("应被拒绝，got success=%v token=%q", ok, token)
			}
			if !strings.Contains(msg, tc.wantMsg) {
				t.Fatalf("msg = %q, want 含 %q", msg, tc.wantMsg)
			}
		})
	}
}

// 项目口令不再有任何 md5 形态的存储，防止以后有人把这一列加回来。
func TestAppModelHasNoMD5Column(t *testing.T) {
	for _, f := range reflect.VisibleFields(reflect.TypeOf(store.AppInfo{})) {
		if strings.Contains(strings.ToLower(string(f.Tag)), "md5") {
			t.Fatalf("AppInfo.%s 带 md5 相关 tag：%s", f.Name, f.Tag)
		}
	}
}
