package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
)

func newTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	Register(r, fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("<html>gojob console</html>")},
		"assets/app.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	})
	return r
}

func TestWebUI(t *testing.T) {
	r := newTestRouter()
	cases := []struct {
		name    string
		method  string
		path    string
		want    int
		body    string
		cache   string
		isIndex bool
	}{
		{name: "首页", method: http.MethodGet, path: "/", want: 200, isIndex: true, cache: "no-cache"},
		{name: "前端路由刷新", method: http.MethodGet, path: "/jobs/6/instances", want: 200, isIndex: true, cache: "no-cache"},
		{name: "静态资源", method: http.MethodGet, path: "/assets/app.js", want: 200, body: "console.log(1)", cache: "public, max-age=31536000, immutable"},
		{name: "缺失静态资源", method: http.MethodGet, path: "/assets/missing.js", want: 404},
		{name: "未知接口", method: http.MethodGet, path: "/openapi/not-exist", want: 404},
		{name: "兼容接口", method: http.MethodGet, path: "/openApi/not-exist", want: 404},
		{name: "worker 接口", method: http.MethodPost, path: "/worker/not-exist", want: 404},
		{name: "非 GET 回退", method: http.MethodPost, path: "/jobs", want: 404},
		{name: "已有路由优先", method: http.MethodGet, path: "/healthz", want: 200, body: "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d, body=%q", w.Code, tc.want, w.Body.String())
			}
			if tc.isIndex && w.Body.String() != "<html>gojob console</html>" {
				t.Fatalf("body = %q, want index.html", w.Body.String())
			}
			if tc.body != "" && w.Body.String() != tc.body {
				t.Fatalf("body = %q, want %q", w.Body.String(), tc.body)
			}
			if tc.cache != "" && w.Header().Get("Cache-Control") != tc.cache {
				t.Fatalf("Cache-Control = %q, want %q", w.Header().Get("Cache-Control"), tc.cache)
			}
		})
	}
}
