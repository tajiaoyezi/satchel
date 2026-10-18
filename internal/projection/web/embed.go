package web

import (
	"embed"
	"io/fs"
)

// dist 是编进二进制的网页产物（tools/web/build.sh 放进来；没跑过脚本时只有 .gitkeep，这样的二进制是开发构建，
// 网页路径回占位页）。all: 让只有 .gitkeep 的目录也能编过。
//
//go:embed all:dist
var dist embed.FS

// Dist 是编进二进制的网页产物的根（dist/ 里面那一层）。
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist 是编译期就在的目录
	}
	return sub
}
