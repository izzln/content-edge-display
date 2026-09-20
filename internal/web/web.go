// Package web 内嵌管理后台静态页面。
package web

import _ "embed"

//go:embed admin/index.html
var AdminHTML []byte
