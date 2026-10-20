// Package web 是投影层的网页：前端产物的 embed 与静态服务（m1-11），以及数据目录 public/ 的对外静态文件（master-serve「无身份入口」）。
package web

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/satchel/satchel/internal/base/db"
)

// PublicPrefix 是对外静态文件的 URL 前缀。
const PublicPrefix = "/public/"

// uploadsPrefix 是 public/uploads/（上传的图片）经 /public/ 对外的路径前缀。
var uploadsPrefix = "/" + db.UploadsDir + "/"

// PublicHandler 直接提供 dir（数据目录的 public/）里的文件：没有身份要求；目录（含 /public/ 本身）一律 404，不列目录；
// http.Dir 以 dir 为根，路径里的 .. 出不了根。public/uploads/ 里的文件另加 nosniff 与 CSP sandbox：被人直接打开时
// 浏览器不按内容猜类型、不以网页的源站运行脚本（从备份恢复回来的文件没经过上传时的类型检查）。public/ 里人手放的别的文件不受影响。
func PublicHandler(dir string) http.Handler {
	files := http.StripPrefix(PublicPrefix, http.FileServer(noDirFS{http.Dir(dir)}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(path.Clean(r.URL.Path), uploadsPrefix) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "sandbox")
		}
		files.ServeHTTP(w, r)
	})
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
