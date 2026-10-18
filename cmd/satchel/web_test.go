package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/projection/web"
)

// withWebFiles 换上一份假的网页产物，测试结束时还原成编进二进制的那份。
func withWebFiles(t *testing.T, files fs.FS) {
	t.Helper()
	orig := webFiles
	webFiles = func() fs.FS { return files }
	t.Cleanup(func() { webFiles = orig })
}

const fakeIndex = `<!doctype html><html><head><title>__SATCHEL_SITE_TITLE__</title>` +
	`<script id="satchel-boot" type="application/json">__SATCHEL_BOOT__</script></head><body></body></html>`

// master-web 的装配（双库）：根路径与前端路由回网页，首屏注入随设置写更新、不用重启；网页回应带安全头，
// /api/、/mcp、/public/ 不带；别的方法仍是四字段 not_found；关闭公网访问时远程的网页请求 307，静默模式锁定时网页也藏起来。
func TestWebEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		withWebFiles(t, fstest.MapFS{"index.html": {Data: []byte(fakeIndex)}, "assets/app-1.js": {Data: []byte("1")}})
		h := start(t, bdb)
		clk := newClock()
		h.setClock(clk)
		base := h.tcpURL
		port := strings.TrimPrefix(base, "http://127.0.0.1")
		setupAdmin(t, base)
		if err := os.WriteFile(filepath.Join(h.dataDir, db.PublicDir, "logo.png"), []byte("PNG"), 0o600); err != nil {
			t.Fatal(err)
		}

		for _, p := range []string{"/", "/tokens", "/settings/gates"} {
			r := send(t, nil, "GET", base+p, nil, "")
			if r.status != 200 || !strings.Contains(r.body, `id="satchel-boot"`) || r.header.Get("Content-Security-Policy") != web.ContentSecurityPolicy {
				t.Fatalf("%s 应当回网页并带安全头：%d %v %s", p, r.status, r.header, r.body)
			}
		}
		if r := send(t, nil, "GET", base+"/assets/missing-0.js", nil, ""); r.status != 404 {
			t.Fatalf("缺的资源 404：%d", r.status)
		}
		if r := send(t, nil, "POST", base+"/", nil, ""); r.status != 404 || r.code() != "not_found" || len(r.fields) != 4 {
			t.Fatalf("别的方法仍是四字段 not_found：%d %s", r.status, r.body)
		}
		for _, c := range []struct{ method, path, body string }{{"GET", "/api/v1/healthz", ""}, {"POST", "/mcp", `{}`}, {"GET", "/public/logo.png", ""}} {
			if r := send(t, nil, c.method, base+c.path, nil, c.body); r.header.Get("Content-Security-Policy") != "" {
				t.Fatalf("%s 不该带网页的安全头：%v", c.path, r.header)
			}
		}

		// master-web「改了品牌，首屏跟着变」：settings set 写成功后下一个请求就是新值。
		cur, e, _ := h.settingsCLI("settings", "show")
		if e != nil {
			t.Fatal(e)
		}
		if _, stderr, code := h.cli("settings", "set", "--set", "branding_site_title=甲", "--set", "branding_brand_title=乙", "--set", "default_theme=flat",
			"--resource-version", strconv.FormatInt(cur.Metadata.ResourceVersion, 10), "--json"); code != 0 {
			t.Fatalf("settings set：%s", stderr)
		}
		if r := send(t, nil, "GET", base+"/", nil, ""); !strings.Contains(r.body, "<title>甲</title>") || !strings.Contains(r.body, `"brand_title":"乙"`) || !strings.Contains(r.body, `"default_theme":"flat"`) {
			t.Fatalf("首屏应当用新值：%s", r.body)
		}

		// 关闭公网访问：远程的网页请求 307 到主控地址。
		if _, stderr, code := h.human("settings", "master-url", "set", "--url", "https://panel.example.com", "--resource-version", strconv.FormatInt(cur.Metadata.ResourceVersion+1, 10)); code != 0 {
			t.Fatalf("master-url set：%s", stderr)
		}
		h.gates("master_local_only=true", loopbackProxy)
		other := map[string]string{"Host": "203.0.113.5" + port}
		if r := send(t, nil, "GET", base+"/tokens", other, ""); r.status != 307 || r.header.Get("Location") != "https://panel.example.com/tokens" {
			t.Fatalf("远程的网页请求应当 307：%d %s", r.status, r.header.Get("Location"))
		}
		h.gates("master_local_only=false")

		// master-web「静默模式下网页也藏起来」。
		h.gates("silent_mode=true", "silent_mode_timeout=15")
		clk.advance(16 * time.Minute)
		nosuch := send(t, nil, "GET", base+"/no/such/page", nil, "")
		if nosuch.status != 404 {
			t.Fatalf("锁定期内不存在的路径是 404：%d", nosuch.status)
		}
		for _, p := range []string{"/", "/tokens"} {
			if r := send(t, nil, "GET", base+p, nil, ""); r.status != 404 || r.body != strings.ReplaceAll(nosuch.body, "/no/such/page", p) || strings.Contains(r.body, "satchel-boot") {
				t.Fatalf("锁定期内 %s 应当与不存在的路径一样：%d %s / %s", p, r.status, r.body, nosuch.body)
			}
		}
	})
}
