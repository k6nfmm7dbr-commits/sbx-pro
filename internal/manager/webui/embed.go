// Package webui 内嵌 Manager 前端。前端资源随二进制一起发布/升级。
package webui

import (
	"embed"
	"io/fs"
)

//go:embed static/index.html static/login.html static/app.js static/login.js static/style.css static/extra.css
var embedded embed.FS

// FS 返回以 "static" 为根的前端文件系统。
func FS() fs.FS {
	sub, err := fs.Sub(embedded, "static")
	if err != nil {
		return embedded
	}
	return sub
}
