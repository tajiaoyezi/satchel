package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/captcha"
	"github.com/satchel/satchel/internal/base/db/dbtest"
)

// master-login-protection「测试配置」「测试配置的前提与权限」的端到端（双库）：经 CLI 与 REST，审计里 response 打码，
// 不改设置、不写安全事件。
func TestTurnstileTestEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		isolateHome(t)
		h := start(t, bdb)
		base := h.tcpURL
		admin := setupAdmin(t, base)
		siteverify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			if r.PostForm.Get("secret") == "0x4AAAAAAAsecretforsatchel" && r.PostForm.Get("response") == "good" {
				_, _ = w.Write([]byte(`{"success":true,"hostname":"panel.example.com"}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
		}))
		defer siteverify.Close()
		h.app.identity.SetCaptcha(&captcha.Client{URL: siteverify.URL, HTTP: &http.Client{Timeout: captcha.Timeout}})

		// 普通用户先登录（存了 key 之后登录要过验证码）。
		seedUser(t, bdb, "carol", "carolpass")
		carol := newBrowser(t)
		if status, fields := carol.login(base, "carol", "carolpass", ""); status != 200 {
			t.Fatalf("普通用户登录：%d %v", status, fields)
		}

		// 两个 key 没都填：conflict。
		if _, stderr, code := h.cli("settings", "turnstile", "test", "--response", "good", "--json"); code != 1 || !strings.Contains(stderr, `"conflict"`) {
			t.Fatalf("没填 key 应当 conflict：%d %s", code, stderr)
		}
		h.gates("turnstile_site_key=0x4AAAAAAAsitekeyforsatchel", "turnstile_secret_key=0x4AAAAAAAsecretforsatchel")
		settingsBefore, _, _ := h.cli("settings", "show", "--json")
		events := h.securityEventCount()

		stdout, stderr, code := h.cli("settings", "turnstile", "test", "--response", "good", "--json")
		if code != 0 || !strings.Contains(stdout, `"success":true`) || !strings.Contains(stdout, `"hostname":"panel.example.com"`) || !strings.Contains(stdout, `"error_codes":[]`) {
			t.Fatalf("CLI 通过时：%d %s %s", code, stdout, stderr)
		}
		if rec := h.lastAudit(); rec["command"] != "settings turnstile test" || strings.Contains(rec["args_digest"].(string), "good") || !strings.Contains(rec["args_digest"].(string), `"response":"***"`) {
			t.Fatalf("审计里 response 应当打码：%v", rec)
		}
		status, fields, _ := admin.call("POST", base+"/api/v1/settings/turnstile/test", `{"response":"bad"}`, nil)
		if status != 200 || string(fields["success"]) != "false" || !strings.Contains(string(fields["error_codes"]), "invalid-input-response") {
			t.Fatalf("REST 不通过时照样 200：%d %v", status, fields)
		}
		if settingsAfter, _, _ := h.cli("settings", "show", "--json"); settingsAfter != settingsBefore {
			t.Fatal("测试配置不该改设置")
		}
		if n := h.securityEventCount(); n != events {
			t.Fatalf("测试配置不写安全事件：%d → %d", events, n)
		}

		// 普通用户 forbidden；连不上 unavailable。
		if status, fields, _ := carol.call("POST", base+"/api/v1/settings/turnstile/test", `{"response":"good"}`, nil); status != 403 || str(fields["code"]) != "forbidden" {
			t.Fatalf("普通用户应当 forbidden：%d %v", status, fields)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		h.app.identity.SetCaptcha(&captcha.Client{URL: "http://" + ln.Addr().String(), HTTP: &http.Client{Timeout: captcha.Timeout}})
		ln.Close()
		if _, stderr, code := h.cli("settings", "turnstile", "test", "--response", "good", "--json"); code != 1 || !strings.Contains(stderr, `"unavailable"`) {
			t.Fatalf("连不上应当 unavailable：%d %s", code, stderr)
		}
	})
}

// securityEventCount 是安全事件表的行数。
func (h *harness) securityEventCount() int {
	h.t.Helper()
	n, err := h.db.NewSelect().TableExpr("security_events").Count(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}
