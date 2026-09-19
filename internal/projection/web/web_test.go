package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/satchel/satchel/internal/base/db"
)

// master-serve「无身份入口」：public 目录提供文件，目录与越界都是 404。
func TestPublicHandler(t *testing.T) {
	dataDir := t.TempDir()
	pub := filepath.Join(dataDir, db.PublicDir)
	if err := os.MkdirAll(filepath.Join(pub, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pub, "logo.png"), []byte("PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secret.json"), []byte(`{"driver":"sqlite"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle(PublicPrefix, PublicHandler(pub))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	get := func(path string) (int, string) {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		return resp.StatusCode, string(buf[:n])
	}
	if code, body := get("/public/logo.png"); code != 200 || body != "PNG" {
		t.Fatalf("文件应当 200：%d %q", code, body)
	}
	for _, p := range []string{"/public/", "/public/sub", "/public/sub/", "/public/nope.png"} {
		if code, _ := get(p); code != 404 {
			t.Errorf("%s 应当 404，得到 %d", p, code)
		}
	}
	// 越界：经 mux 会被规范化并重定向到 /secret.json（不在 public 下），直接打处理器时 .. 出不了根。
	if code, body := get("/public/..%2Fsecret.json"); code == 200 || body == `{"driver":"sqlite"}` {
		t.Fatalf("不该读到 public 之外的文件：%d %q", code, body)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/public/x", nil)
	req.URL.Path = "/public/../secret.json"
	PublicHandler(pub).ServeHTTP(rec, req)
	if rec.Code == 200 || rec.Body.String() == `{"driver":"sqlite"}` {
		t.Fatalf("直接打处理器也不该越界：%d %q", rec.Code, rec.Body.String())
	}
}
