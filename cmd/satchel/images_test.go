package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
)

// testPNG 是一段以 PNG 签名开头的内容；主控按开头认定类型，后面的字节不解析。
func testPNG(tag string) string { return "\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR" + tag }

// master-settings「上传图片」的端到端（双库）：CLI 上传后用 settings set 设成 logo，/public/ 能取到、首屏用它；
// REST 以文件本身作请求体；普通用户 forbidden；MCP 上不可用；不收的类型 bad_request。
func TestImageUploadEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		withWebFiles(t, fstest.MapFS{"index.html": {Data: []byte(fakeIndex)}})
		h := start(t, bdb)
		base := h.tcpURL
		admin := setupAdmin(t, base)
		uploads := filepath.Join(h.dataDir, filepath.FromSlash(db.UploadsDir))

		// 「上传后用作 logo」：经 CLI（本机 socket）。
		logo := filepath.Join(t.TempDir(), "logo.png")
		if err := os.WriteFile(logo, []byte(testPNG("cli")), 0o600); err != nil {
			t.Fatal(err)
		}
		stdout, stderr, exit := h.cli("settings", "image", "upload", "--file", logo, "--json")
		if exit != 0 {
			t.Fatalf("settings image upload：%d %s", exit, stderr)
		}
		var img struct {
			URL         string `json:"url"`
			Size        int64  `json:"size"`
			ContentType string `json:"content_type"`
		}
		if err := json.Unmarshal([]byte(stdout), &img); err != nil || !strings.HasPrefix(img.URL, "/public/uploads/") || !strings.HasSuffix(img.URL, ".png") ||
			img.ContentType != "image/png" || img.Size != int64(len(testPNG("cli"))) {
			t.Fatalf("输出不对：%v %s", err, stdout)
		}
		cur, e, _ := h.settingsCLI("settings", "show")
		if e != nil {
			t.Fatal(e)
		}
		if _, stderr, exit := h.cli("settings", "set", "--set", "branding_logo_url="+img.URL,
			"--resource-version", strconv.FormatInt(cur.Metadata.ResourceVersion, 10), "--json"); exit != 0 {
			t.Fatalf("settings set：%s", stderr)
		}
		if r := send(t, nil, "GET", base+img.URL, nil, ""); r.status != 200 || r.body != testPNG("cli") {
			t.Fatalf("GET %s：%d %q", img.URL, r.status, r.body)
		}
		if r := send(t, nil, "GET", base+"/", nil, ""); !strings.Contains(r.body, `"logo_url":"`+img.URL+`"`) {
			t.Fatalf("首屏的 logo 地址应当是上传的那张：%s", r.body)
		}

		// REST：请求体是文件本身。
		bin := map[string]string{"Content-Type": "application/octet-stream"}
		r := send(t, admin.c, "POST", base+"/api/v1/settings/image/upload", bin, testPNG("rest"))
		if r.status != 200 || !strings.HasPrefix(str(r.fields["url"]), "/public/uploads/") || str(r.fields["content_type"]) != "image/png" {
			t.Fatalf("REST 上传：%d %s", r.status, r.body)
		}
		if r := send(t, admin.c, "POST", base+"/api/v1/settings/image/upload", bin, `<svg xmlns="http://www.w3.org/2000/svg"/>`); r.status != 400 || r.code() != "bad_request" {
			t.Fatalf("SVG 应当 bad_request：%d %s", r.status, r.body)
		}
		before := len(entries(t, uploads))

		// 「普通用户做不了」。
		seedUser(t, bdb, "bob", "secret12")
		bob := newBrowser(t)
		if status, _ := bob.login(base, "bob", "secret12", ""); status != 200 {
			t.Fatalf("bob 登录：%d", status)
		}
		if r := send(t, bob.c, "POST", base+"/api/v1/settings/image/upload", bin, testPNG("bob")); r.status != 403 || r.code() != "forbidden" {
			t.Fatalf("普通用户应当 forbidden：%d %s", r.status, r.body)
		}
		if after := len(entries(t, uploads)); after != before {
			t.Fatalf("被拒的上传不该留下文件：%d → %d", before, after)
		}

		// MCP 上不可用（上传类）。
		if text, isErr := h.mcpRun("settings", "image", "upload"); !isErr || !strings.Contains(text, "bad_request") {
			t.Fatalf("MCP 上应当被拒：%v %s", isErr, text)
		}
	})
}

func entries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return list
}
