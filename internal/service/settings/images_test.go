package settings

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// png 是一段以 PNG 签名开头的内容；tag 让每张的内容（也就是文件名）不同。
func png(tag string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), []byte(tag)...)
}

// uploads 建一个临时数据目录给 settings image upload 用，busy 是 service/backup 的 Busy 的替身。
func (f *fixture) uploads(busy *bool) string {
	f.t.Helper()
	dataDir := f.t.TempDir()
	f.svc.SetUploads(dataDir, func() bool { return busy != nil && *busy }, nil)
	return filepath.Join(dataDir, filepath.FromSlash(db.UploadsDir))
}

func (f *fixture) upload(ctx context.Context, body io.Reader) (*Image, error) {
	f.t.Helper()
	res, err := f.svc.Bindings()["settings image upload"](ctx, &command.Invocation{Path: []string{"settings", "image", "upload"}, Flags: map[string]any{}, Body: body})
	if err != nil {
		return nil, err
	}
	return res.(*Image), nil
}

func (f *fixture) mustUpload(content []byte) *Image {
	f.t.Helper()
	img, err := f.upload(f.admin, bytes.NewReader(content))
	if err != nil {
		f.t.Fatalf("上传失败：%v", err)
	}
	return img
}

// names 列出目录里的文件名（含 . 开头的临时文件），目录不存在时为空。
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// master-settings「上传图片」：存成内容哈希命名的文件，返回地址；目录 0700、文件 0600；五种类型的扩展名。
func TestImageUpload(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		dir := f.uploads(nil)
		content := png("logo")
		img := f.mustUpload(content)
		if !strings.HasPrefix(img.URL, "/public/uploads/") || !strings.HasSuffix(img.URL, ".png") || img.ContentType != "image/png" || img.Size != int64(len(content)) {
			t.Fatalf("输出不对：%+v", img)
		}
		name := strings.TrimPrefix(img.URL, "/public/uploads/")
		if len(name) != 32+len(".png") {
			t.Errorf("文件名应当是哈希前 32 位加扩展名：%s", name)
		}
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("存下的文件不对：%v", err)
		}
		if fi, _ := os.Stat(filepath.Join(dir, name)); fi.Mode().Perm() != 0o600 {
			t.Errorf("文件权限应当是 0600：%v", fi.Mode())
		}
		if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
			t.Errorf("目录权限应当是 0700：%v", fi.Mode())
		}
		for content, want := range map[string]string{
			"\xff\xd8\xff\xe0jpeg":          ".jpg",
			"GIF89a gif":                    ".gif",
			"RIFF\x00\x00\x00\x00WEBPVP8 x": ".webp",
			"\x00\x00\x01\x00\x01\x00 ico":  ".ico",
		} {
			if img := f.mustUpload([]byte(content)); !strings.HasSuffix(img.URL, want) {
				t.Errorf("应当存成 %s：%+v", want, img)
			}
		}
	})
}

// master-settings「同样的内容只存一份」：同一张图片上传两次，地址相同、只有一个文件，修改时间更新。
func TestImageUploadSameContent(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		dir := f.uploads(nil)
		first := f.mustUpload(png("same"))
		path := filepath.Join(dir, strings.TrimPrefix(first.URL, "/public/uploads/"))
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		second := f.mustUpload(png("same"))
		if first.URL != second.URL {
			t.Fatalf("两次的地址应当相同：%s 与 %s", first.URL, second.URL)
		}
		if got := names(t, dir); len(got) != 1 {
			t.Fatalf("应当只有一个文件：%v", got)
		}
		if fi, _ := os.Stat(path); !fi.ModTime().After(old.Add(time.Minute)) {
			t.Errorf("修改时间应当更新：%v", fi.ModTime())
		}
	})
}

// master-settings「只收几种图片」：SVG、改名成 png 的文本、超过 5 MiB 的 PNG 都是 bad_request，不留下文件（含临时文件）。
func TestImageUploadRejects(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		dir := f.uploads(nil)
		big := append(png("big"), make([]byte, 6<<20)...)
		for name, content := range map[string][]byte{
			"svg":    []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
			"text":   []byte("just text in x.png"),
			"html":   []byte("<!DOCTYPE html><html><script>alert(1)</script></html>"),
			"empty":  {},
			"6 MiB":  big,
			"bitmap": []byte("BM bitmap"),
		} {
			_, err := f.upload(f.admin, bytes.NewReader(content))
			if v1.AsError(err).Code != v1.CodeBadRequest {
				t.Errorf("%s 应当是 bad_request，得到 %v", name, err)
			}
			if name == "svg" && !strings.Contains(v1.AsError(err).Reason, "PNG、JPEG、GIF、WebP 与 ICO") {
				t.Errorf("reason 应当列出收哪几种：%v", err)
			}
		}
		if got := names(t, dir); len(got) != 0 {
			t.Fatalf("不该留下文件：%v", got)
		}
		if _, err := f.upload(f.admin, nil); v1.AsError(err).Code != v1.CodeBadRequest {
			t.Errorf("没有请求体应当是 bad_request，得到 %v", err)
		}
	})
}

// master-settings「普通用户做不了」。
func TestImageUploadForbidden(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		dir := f.uploads(nil)
		if _, err := f.upload(f.user, bytes.NewReader(png("x"))); v1.AsError(err).Code != v1.CodeForbidden {
			t.Fatalf("普通用户应当是 forbidden，得到 %v", err)
		}
		if got := names(t, dir); len(got) != 0 {
			t.Fatalf("不该留下文件：%v", got)
		}
	})
}

// fill 往目录里放 n 张图片，修改时间从旧到新各差一分钟，返回它们的地址（最旧的在前）。
func (f *fixture) fill(dir string, n int) []string {
	f.t.Helper()
	var urls []string
	base := time.Now().Add(-time.Duration(n+1) * time.Minute)
	for i := 0; i < n; i++ {
		img := f.mustUpload(png(fmt.Sprintf("fill-%d", i)))
		at := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(filepath.Join(dir, strings.TrimPrefix(img.URL, "/public/uploads/")), at, at); err != nil {
			f.t.Fatal(err)
		}
		urls = append(urls, img.URL)
	}
	return urls
}

func exists(dir, url string) bool {
	_, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(url, "/public/uploads/")))
	return err == nil
}

// master-settings「清理时不删用着的图片」：超过 20 个时从最旧的删；当前设置用着的、只被写前快照用着的都不删，
// 带主机名与查询串的完整地址也算用着。
func TestImagePrune(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		dir := f.uploads(nil)
		urls := f.fill(dir, 20)
		// 最旧的是当前 logo；第二旧的先当壁纸、再换掉，只留在写前快照里；第三旧的以完整地址当探针伪装 logo。
		obj := f.mustSet(map[string]any{"branding_logo_url": urls[0], "login_wallpaper": urls[1], "probe_disguise_logo": "https://panel.example.com" + urls[2] + "?v=2"}, 1)
		f.mustSet(map[string]any{"login_wallpaper": ""}, int(obj.Metadata.ResourceVersion))
		latest := f.mustUpload(png("new"))
		got := names(t, dir)
		if len(got) != 20 {
			t.Fatalf("应当还剩 20 个文件，得到 %d：%v", len(got), got)
		}
		if !exists(dir, urls[0]) || !exists(dir, urls[1]) || !exists(dir, urls[2]) || !exists(dir, latest.URL) {
			t.Fatal("当前 logo、只被快照用着的壁纸、以完整地址用着的伪装 logo 与新图片都应当还在")
		}
		if exists(dir, urls[3]) {
			t.Fatal("没人用的最旧的那个应当被删掉")
		}
	})
}

// usedName 按清理过的路径取文件名，不看主机名与查询串；指向 public/uploads/ 以外的为空。
func TestUsedName(t *testing.T) {
	for v, want := range map[string]string{
		"/public/uploads/ab.png":                            "ab.png",
		"/public/uploads/./ab.png":                          "ab.png",
		"/public/uploads/x/../ab.png?v=1":                   "ab.png",
		"https://panel.example.com/public/uploads/ab.png#x": "ab.png",
		"//panel.example.com/public/uploads/ab.png":         "ab.png",
		"/public/uploads/%61b.png":                          "ab.png",
		"/public/uploads/sub/ab.png":                        "",
		"/public/uploads/../ab.png":                         "",
		"/public/uploads/":                                  "",
		"data:image/png;base64,AAAA":                        "",
		"":                                                  "",
	} {
		if got := usedName(v); got != want {
			t.Errorf("usedName(%q) = %q，应当是 %q", v, got, want)
		}
	}
}

// 清理占着 images 时，set 与 rollback 要等它做完（清理按读到的引用删，中间不能有一次写把字段改成正要删的文件）。
func TestImageFieldWritesWaitForPrune(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		f.mustSet(map[string]any{"branding_logo_url": "/public/uploads/a.png"}, 1)
		var snap int64
		if err := bdb.NewSelect().Model((*model.ConfigSnapshot)(nil)).Column("id").Order("id DESC").Limit(1).Scan(context.Background(), &snap); err != nil {
			t.Fatal(err)
		}
		for name, write := range map[string]func() error{
			"settings set": func() error {
				_, err := f.set(map[string]any{"branding_logo_url": "/public/uploads/b.png"}, int(f.show().Metadata.ResourceVersion))
				return err
			},
			"settings rollback": func() error {
				_, err := f.run(f.admin, "settings rollback", []string{strconv.FormatInt(snap, 10)}, map[string]any{"resource-version": int(f.show().Metadata.ResourceVersion)})
				return err
			},
		} {
			f.svc.images.Lock()
			done := make(chan error, 1)
			go func() { done <- write() }()
			select {
			case err := <-done:
				f.svc.images.Unlock()
				t.Fatalf("%s 应当等清理做完，却已经返回：%v", name, err)
			case <-time.After(100 * time.Millisecond):
			}
			f.svc.images.Unlock()
			if err := <-done; err != nil {
				t.Fatalf("%s：%v", name, err)
			}
		}
	})
}

// 有备份、恢复、迁移或升级在进行时这一次不清理。
func TestImagePruneSkippedWhenBusy(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		f := setup(t, bdb)
		busy := false
		dir := f.uploads(&busy)
		f.fill(dir, 20)
		busy = true
		f.mustUpload(png("new"))
		if got := names(t, dir); len(got) != 21 {
			t.Fatalf("在忙时不该清理，应当有 21 个文件，得到 %d", len(got))
		}
		busy = false
		f.mustUpload(png("newer"))
		if got := names(t, dir); len(got) != 20 {
			t.Fatalf("不忙时应当清理到 20 个，得到 %d", len(got))
		}
	})
}
