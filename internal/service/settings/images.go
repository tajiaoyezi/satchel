package settings

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// maxImage 是上传一张图片的上限（5 MiB；变量只为测试能调小）。keepImages 是 public/uploads/ 里最多留的文件数。
var maxImage int64 = 5 << 20

const keepImages = 20

// imageTypes 是收的图片类型（http.DetectContentType 按文件开头认定）与存盘的扩展名。不收 SVG：它能带脚本，
// 而 /public/ 与网页同源、不加安全头（master-settings「上传图片」）。
var imageTypes = map[string]string{
	"image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp", "image/x-icon": "ico",
}

// imageFields 是设置里放图片地址的字段：清理时，当前设置与写前快照里这几个字段用着的文件不删。
var imageFields = []string{"branding_logo_url", "login_wallpaper", "probe_disguise_logo"}

// Image 是 settings image upload 的输出。
type Image struct {
	URL         string `json:"url"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// SetUploads 给 settings image upload 指定数据目录，以及「此刻有备份、恢复、迁移或升级在进行」的查询
// （service/backup 的 Busy；在进行时不清理，免得生成备份的过程中文件被删）。装配根在启动时设一次。
func (s *Service) SetUploads(dataDir string, busy func() bool, logger *slog.Logger) {
	s.uploads = filepath.Join(dataDir, filepath.FromSlash(db.UploadsDir))
	s.busy = busy
	s.logger = logger
}

func (s *Service) imageUpload(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if inv.Body == nil {
		return nil, v1.New(v1.CodeBadRequest, "没有收到图片文件")
	}
	body := bufio.NewReader(io.LimitReader(inv.Body, maxImage+1))
	head, err := body.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, v1.Wrap(v1.CodeBadRequest, "接收图片失败", err)
	}
	contentType := http.DetectContentType(head)
	ext, ok := imageTypes[contentType]
	if !ok {
		return nil, v1.Newf(v1.CodeBadRequest, "只收 PNG、JPEG、GIF、WebP 与 ICO 图片（按文件内容判断，不收 SVG），这个文件是 %s", contentType)
	}
	if err := os.MkdirAll(s.uploads, 0o700); err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "建图片目录失败", err)
	}
	tmp, err := os.CreateTemp(s.uploads, ".upload-*")
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "建上传的临时文件失败", err)
	}
	defer os.Remove(tmp.Name())
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), body)
	if err == nil {
		err = tmp.Chmod(0o600)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeBadRequest, "接收图片失败", err)
	}
	if n > maxImage {
		return nil, v1.Newf(v1.CodeBadRequest, "图片超过 %d 字节的上限（5 MiB）", maxImage)
	}
	name := hex.EncodeToString(sum.Sum(nil))[:32] + "." + ext
	path := filepath.Join(s.uploads, name)
	if _, err := os.Stat(path); err == nil {
		// 同样的内容已经在：不重写，只更新修改时间，清理时它算新的。
		now := time.Now()
		if err := os.Chtimes(path, now, now); err != nil {
			return nil, v1.Wrap(v1.CodeInternal, "保存图片失败", err)
		}
	} else {
		if err := os.Rename(tmp.Name(), path); err != nil {
			return nil, v1.Wrap(v1.CodeInternal, "保存图片失败", err)
		}
		if err := syncDir(s.uploads); err != nil {
			return nil, v1.Wrap(v1.CodeInternal, "保存图片失败", err)
		}
	}
	s.pruneImages(ctx)
	return &Image{URL: imageURL(name), Size: n, ContentType: contentType}, nil
}

// imageURL 是 public/uploads/ 里一个文件经 /public/ 对外的地址。
func imageURL(name string) string { return "/" + db.UploadsDir + "/" + name }

// pruneImages：public/uploads/ 里的文件超过 keepImages 个时，按修改时间从旧到新删，删到 keepImages 个或没有可删的为止；
// 当前设置或任何一份写前快照里图片字段指向它的不删（usedName）。有备份、恢复、迁移或升级在进行时这一次不清理。
// 清理失败只记日志：图片已经存好了。
//
// 清理期间占着 images，set 与 rollback 要等它做完。快照从不删，每次写都存下写前的值，所以库里出现过的图片地址一直算「用着」：
// 刚过了 busy 检查就开始的备份，它拷下的库里的地址清理也不会删。
func (s *Service) pruneImages(ctx context.Context) {
	if s.busy != nil && s.busy() {
		return
	}
	s.images.Lock()
	defer s.images.Unlock()
	entries, err := os.ReadDir(s.uploads)
	if err != nil {
		s.warn("清理上传的图片失败", err)
		return
	}
	type file struct {
		name string
		mod  time.Time
	}
	var files []file
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, file{e.Name(), fi.ModTime()})
	}
	if len(files) <= keepImages {
		return
	}
	used, err := s.usedImages(ctx)
	if err != nil {
		s.warn("清理上传的图片失败", err)
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	left := len(files)
	for _, f := range files {
		if left <= keepImages {
			break
		}
		if used[f.name] {
			continue
		}
		if err := os.Remove(filepath.Join(s.uploads, f.name)); err != nil && !os.IsNotExist(err) {
			s.warn("清理上传的图片失败", err)
			continue
		}
		left--
	}
}

// usedImages 是当前设置与全部写前快照里图片字段指向的文件名（usedName）。
func (s *Service) usedImages(ctx context.Context) (map[string]bool, error) {
	used := map[string]bool{}
	st, err := s.repo.Load(ctx)
	if err != nil {
		return nil, err
	}
	for _, f := range imageFields {
		if v, ok := st.Values[f].(string); ok {
			used[usedName(v)] = true
		}
	}
	contents, err := s.repo.SnapshotContents(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range contents {
		var values map[string]any
		if err := json.Unmarshal(c, &values); err != nil {
			return nil, v1.Wrap(v1.CodeInternal, "快照内容不是 JSON 对象", err)
		}
		for _, f := range imageFields {
			if v, ok := values[f].(string); ok {
				used[usedName(v)] = true
			}
		}
	}
	delete(used, "")
	return used, nil
}

// usedName 是图片字段的值指向的 public/uploads/ 里的文件名，指向别处时为空。按清理过的路径比，不看主机名与查询串：
// 浏览器里复制的图片地址带着主机名，/public/uploads/./x.png 这种写法 /public/ 也照样取到同一个文件。
func usedName(v string) string {
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	dir, name := path.Split(path.Clean(u.Path))
	if dir != "/"+db.UploadsDir+"/" {
		return ""
	}
	return name
}

func (s *Service) warn(msg string, err error) {
	if s.logger != nil {
		s.logger.Warn(msg, "error", err)
	}
}

// syncDir 把目录项刷到磁盘（改名之后调，断电也不会丢）。
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
