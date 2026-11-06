//go:build dashboard

package webui

import (
	"embed"
	"io/fs"
)

// dist 由 Dockerfile.all 在前端构建完成后拷进本目录；本地构建需要先 `pnpm build`
// 并手动拷贝，否则带 dashboard tag 编译会失败。
//
//go:embed dist
var embedded embed.FS

// FS 返回嵌入的控制台静态文件。
func FS() (fs.FS, bool) {
	sub, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, false
	}
	return sub, true
}
