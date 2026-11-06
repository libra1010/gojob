//go:build !dashboard

package webui

import "io/fs"

// FS 在默认构建里返回空，Server 不注册任何静态资源。
func FS() (fs.FS, bool) { return nil, false }
