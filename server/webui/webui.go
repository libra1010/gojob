// Package webui 把控制台静态文件挂到 Server 的同一端口上。
//
// 默认构建（不带 dashboard tag）不包含前端，行为与引入本包前完全一致；
// 单镜像构建时带上 -tags dashboard 并把 dist 放到本目录，见 Dockerfile.all。
package webui

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/gin-gonic/gin"
)

// apiPrefixes 是后端自己的路径前缀。这些前缀下未匹配到路由时必须回 404，
// 不能落进 SPA 的 history 回退，否则前端会把一个打错的接口当成页面。
var apiPrefixes = []string{"/openapi", "/openApi", "/worker", "/healthz"}

func isAPI(p string) bool {
	for _, prefix := range apiPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// Register 挂载静态资源与 SPA history 回退。
// 前端路由（/jobs/6/instances 之类）刷新时由 index.html 接管；
// 带扩展名但文件不存在的请求仍返回 404，避免把缺失的 js/css 悄悄换成 HTML。
func Register(r *gin.Engine, files fs.FS) {
	fileServer := http.FileServer(http.FS(files))
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if isAPI(p) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}

		name := strings.TrimPrefix(p, "/")
		if name != "" {
			if f, err := files.Open(name); err == nil {
				st, statErr := f.Stat()
				f.Close()
				if statErr == nil && !st.IsDir() {
					if strings.HasPrefix(name, "assets/") {
						c.Header("Cache-Control", "public, max-age=31536000, immutable")
					} else {
						c.Header("Cache-Control", "no-cache")
					}
					fileServer.ServeHTTP(c.Writer, c.Request)
					return
				}
			}
			if strings.Contains(path.Base(name), ".") {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
		}

		c.Header("Cache-Control", "no-cache")
		c.Request.URL.Path = "/"
		fileServer.ServeHTTP(c.Writer, c.Request)
	})
}
