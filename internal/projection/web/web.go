// Package web 是投影层的网页：前端产物的 embed 与静态服务（m1-10），以及数据目录 public/ 的对外静态文件（master-serve「无身份入口」）。
package web

import (
	"io/fs"
	"net/http"
)

// PublicPrefix 是对外静态文件的 URL 前缀。
const PublicPrefix = "/public/"

// PublicHandler 直接提供 dir（数据目录的 public/）里的文件：没有身份要求；目录（含 /public/ 本身）一律 404，不列目录；
// http.Dir 以 dir 为根，路径里的 .. 出不了根。
func PublicHandler(dir string) http.Handler {
	return http.StripPrefix(PublicPrefix, http.FileServer(noDirFS{http.Dir(dir)}))
}

// noDirFS 把目录当成不存在，http.FileServer 因此对目录回 404 而不是列表或 index.html。
type noDirFS struct {
	fs http.FileSystem
}

func (n noDirFS) Open(name string) (http.File, error) {
	f, err := n.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}
