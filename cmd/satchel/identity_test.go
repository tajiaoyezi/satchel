package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/db/dbtest"
	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/projection/cli"
	"github.com/satchel/satchel/internal/service/auth"
)

// browser 是一个带 cookie 罐的 HTTP 客户端，模拟一个浏览器（一处登录）。
type browser struct {
	t *testing.T
	c *http.Client
}

func newBrowser(t *testing.T) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{t: t, c: &http.Client{Jar: jar}}
}

// call 发一个请求：body 非空时是 JSON；返回状态码、解出的 JSON 对象与响应（看 Set-Cookie 用）。
func (b *browser) call(method, url, body string, header map[string]string) (int, map[string]json.RawMessage, *http.Response) {
	b.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		b.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		b.t.Fatalf("%s %s 的 body 不是 JSON 对象：%s", method, url, raw)
	}
	return resp.StatusCode, fields, resp
}

func str(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// code 是某个时刻的 TOTP 码；防重放让同一个码只能成功一次，相邻周期（±30 秒，都在校验的宽限内）的码是另外的码。
func code(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totp.GenerateCode(secret, at)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// login 走密码登录 + 第二步（second 非空时），返回第二步的响应字段。
func (b *browser) login(base, username, password, second string) (int, map[string]json.RawMessage) {
	b.t.Helper()
	status, fields, _ := b.call("POST", base+"/api/v1/session", `{"username":"`+username+`","password":"`+password+`"}`, nil)
	if second == "" || status != 200 {
		return status, fields
	}
	if string(fields["two_factor_required"]) != "true" {
		b.t.Fatalf("开了两步验证的账号登录第一步应当要第二步：%v", fields)
	}
	status, fields, _ = b.call("POST", base+"/api/v1/session/two-factor", `{"pending":`+string(fields["pending"])+`,"code":"`+second+`"}`, nil)
	return status, fields
}

// seedUser 直接插一个普通用户（M3 之前没有用户管理命令）。
func seedUser(t *testing.T, bdb *bun.DB, username, password string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	u := &model.User{Username: username, Role: "user", IsActive: true, PasswordHash: hash, RecoveryCodes: json.RawMessage(`[]`),
		NodeSpeedLimitOverrides: json.RawMessage(`{}`), NodeDeviceLimitOverrides: json.RawMessage(`{}`), CreatedAt: now, UpdatedAt: now, ResourceVersion: 1}
	if _, err := bdb.NewInsert().Model(u).Exec(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// lastAudit 取最新一条审计记录（经 CLI 的 audit list，它自己也记一条）。
func (h *harness) lastAudit() map[string]any {
	h.t.Helper()
	stdout, stderr, code := h.cli("audit", "list", "--json", "--limit", "1")
	if code != 0 {
		h.t.Fatalf("audit list：%s", stderr)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &page); err != nil || len(page.Items) != 1 {
		h.t.Fatalf("audit list 应当给 1 条：%s", stdout)
	}
	return page.Items[0]
}

// 身份的端到端（双库）：初始化向导 → 会话 → 两步验证与恢复码 → 当场验证 → 本机重置密码 → 普通用户的权限。
func TestIdentityEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		base := h.tcpURL
		alice := newBrowser(t)

		// 空库：setup status 不要身份，只有建管理员这一支可用。
		status, fields, _ := alice.call("GET", base+"/api/v1/setup/status", "", nil)
		var paths map[string]struct {
			Available bool `json:"available"`
		}
		_ = json.Unmarshal(fields["paths"], &paths)
		if status != 200 || string(fields["initialized"]) != "false" || !paths["create_admin"].Available || paths["restore_backup"].Available || paths["import_mmwx"].Available {
			t.Fatalf("空库的 setup status：%d %v", status, fields)
		}
		// 密码不合规、用户名不合规都是 400，不建账号。
		for _, body := range []string{`{"username":"admin","password":"short"}`, `{"username":"Admin","password":"secret12"}`, `{"username":"admin"}`} {
			if status, fields, _ := alice.call("POST", base+"/api/v1/setup/init", body, nil); status != 400 || str(fields["code"]) != "bad_request" {
				t.Fatalf("%s 应当 400 bad_request：%d %v", body, status, fields)
			}
		}
		// setup init：TCP 上无身份也能做；成功顺手发会话 cookie。
		status, fields, resp := alice.call("POST", base+"/api/v1/setup/init", `{"username":"admin","password":"secret12","email":"a@example.com"}`, nil)
		if status != 200 || str(fields["username"]) != "admin" || str(fields["role"]) != "admin" {
			t.Fatalf("setup init：%d %v", status, fields)
		}
		var cookie *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == "satchel_session" {
				cookie = c
			}
		}
		if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Secure || len(cookie.Value) < 40 {
			t.Fatalf("setup init 的 cookie：%+v", cookie)
		}
		// 第二次 init 是 conflict；status 变成已初始化、三支都不可用。
		if status, fields, _ := alice.call("POST", base+"/api/v1/setup/init", `{"username":"other","password":"secret12"}`, nil); status != 409 || str(fields["code"]) != "conflict" {
			t.Fatalf("第二次 setup init 应当 409 conflict：%d %v", status, fields)
		}
		status, fields, _ = alice.call("GET", base+"/api/v1/setup/status", "", nil)
		_ = json.Unmarshal(fields["paths"], &paths)
		if status != 200 || string(fields["initialized"]) != "true" || paths["create_admin"].Available {
			t.Fatalf("初始化后的 setup status：%v", fields)
		}
		// 带 cookie 的 whoami 是 user / admin。
		status, fields, _ = alice.call("GET", base+"/api/v1/whoami", "", nil)
		if status != 200 || str(fields["actor_kind"]) != "user" || str(fields["actor"]) != "admin" || str(fields["role"]) != "admin" {
			t.Fatalf("会话身份：%d %v", status, fields)
		}
		// 审计：setup status ×2、setup init ×5（三次 400、一次 200、一次 409）、whoami ×1；密码打码。
		// 不要身份的命令进了链就记，包括参数不合规被业务拒掉的那几次（不经 authz 的「anonymous 不记」）。
		if n := h.auditCount(); n != 2+5+1 {
			t.Fatalf("向导阶段审计应当 8 条，得到 %d", n)
		}
		stdout, _, _ := h.cli("audit", "list", "--json", "--limit", "20")
		if strings.Contains(stdout, "secret12") || !strings.Contains(stdout, `\"password\":\"***\"`) {
			t.Fatalf("审计摘要里密码应当打码：%s", stdout)
		}
		// 本机管理员经 socket 调 setup status 也行；account show 对本机管理员是 bad_request 指引。
		if stdout, stderr, code := h.cli("setup", "status", "--json"); code != 0 || !strings.Contains(stdout, `"initialized":true`) {
			t.Fatalf("CLI setup status：%d %s %s", code, stdout, stderr)
		}
		if _, stderr, code := h.cli("account", "show", "--json"); code != 1 || !strings.Contains(stderr, "bad_request") {
			t.Fatalf("本机管理员 account show 应当 bad_request：%d %s", code, stderr)
		}
		// MCP 上向导命令不可用。
		if text, isErr := h.mcpRun("setup", "status"); !isErr || !strings.Contains(text, "bad_request") {
			t.Fatalf("MCP setup status 应当 bad_request：%v %s", isErr, text)
		}

		// account show → 开两步验证（当场验证只要密码）→ 拿恢复码。
		status, fields, _ = alice.call("GET", base+"/api/v1/account/show", "", nil)
		if status != 200 || string(fields["totp_enabled"]) != "false" || string(fields["sessions"]) != "1" {
			t.Fatalf("account show：%d %v", status, fields)
		}
		status, fields, _ = alice.call("POST", base+"/api/v1/account/totp/setup", `{"verify-password":"wrong"}`, nil)
		if status != 403 || str(fields["code"]) != "human_required" {
			t.Fatalf("密码错的当场验证应当 403 human_required：%d %v", status, fields)
		}
		if e := h.lastAudit(); e["command"] != "account totp setup" || e["result"] != "human_required" || e["actor"] != "admin" {
			t.Fatalf("人类专属失败应当记审计：%v", e)
		}
		status, fields, _ = alice.call("POST", base+"/api/v1/account/totp/setup", `{"verify-password":"secret12"}`, nil)
		secret := str(fields["secret"])
		if status != 200 || secret == "" || !strings.HasPrefix(str(fields["otpauth_url"]), "otpauth://totp/Satchel") {
			t.Fatalf("totp setup：%d %v", status, fields)
		}
		now := time.Now()
		status, fields, _ = alice.call("POST", base+"/api/v1/account/totp/confirm", `{"code":"`+code(t, secret, now)+`","verify-password":"secret12"}`, nil)
		var codes []string
		_ = json.Unmarshal(fields["recovery_codes"], &codes)
		if status != 200 || len(codes) != 8 {
			t.Fatalf("totp confirm：%d %v", status, fields)
		}
		// 登出：cookie 失效。会话入口不记审计。
		before := h.auditCount()
		if status, _, _ := alice.call("DELETE", base+"/api/v1/session", "", nil); status != 200 {
			t.Fatalf("登出：%d", status)
		}
		if status, _, _ := alice.call("GET", base+"/api/v1/whoami", "", nil); status != 401 {
			t.Fatalf("登出后应当 401：%d", status)
		}
		// 密码错：401 同一条；停用之外的两步登录：TOTP（下一周期的码，避开 confirm 用掉的那一个）。
		if status, fields := alice.login(base, "admin", "nope", ""); status != 401 || str(fields["code"]) != "unauthenticated" {
			t.Fatalf("密码错：%d %v", status, fields)
		}
		if status, fields := alice.login(base, "admin", "secret12", code(t, secret, now.Add(30*time.Second))); status != 200 || string(fields["recovery_codes_remaining"]) != "8" {
			t.Fatalf("TOTP 登录：%d %v", status, fields)
		}
		if status, fields, _ := alice.call("GET", base+"/api/v1/whoami", "", nil); status != 200 || str(fields["actor"]) != "admin" {
			t.Fatalf("TOTP 登录后的身份：%d %v", status, fields)
		}
		if n := h.auditCount(); n != before+1 { // 只有 whoami 那一条
			t.Fatalf("登录 / 登出入口不该记审计：%d → %d", before, n)
		}
		// 恢复码：一次成功、同一枚第二次失败。
		alice.call("DELETE", base+"/api/v1/session", "", nil)
		if status, fields := alice.login(base, "admin", "secret12", codes[0]); status != 200 || string(fields["recovery_codes_remaining"]) != "7" {
			t.Fatalf("恢复码登录：%d %v", status, fields)
		}
		alice.call("DELETE", base+"/api/v1/session", "", nil)
		if status, fields := alice.login(base, "admin", "secret12", codes[0]); status != 401 || str(fields["code"]) != "unauthenticated" {
			t.Fatalf("用过的恢复码应当 401：%d %v", status, fields)
		}
		if status, _ := alice.login(base, "admin", "secret12", codes[1]); status != 200 {
			t.Fatalf("另一枚恢复码：%d", status)
		}
		// 第二处登录（TOTP 上一周期的码）；account show 看到两个会话。
		bob := newBrowser(t)
		if status, _ := bob.login(base, "admin", "secret12", code(t, secret, now.Add(-30*time.Second))); status != 200 {
			t.Fatalf("第二处登录：%d", status)
		}
		if _, fields, _ := alice.call("GET", base+"/api/v1/account/show", "", nil); string(fields["sessions"]) != "2" || string(fields["totp_enabled"]) != "true" || string(fields["recovery_codes_remaining"]) != "6" {
			t.Fatalf("两处登录后的 account show：%v", fields)
		}
		// 改密码（开了两步验证：当场验证要第二因素）：作废另一处，保留当前。
		status, fields, _ = alice.call("POST", base+"/api/v1/account/set-password", `{"new-password":"n3wsecret","verify-password":"secret12"}`, nil)
		if status != 403 || !strings.Contains(str(fields["reason"]), "第二因素") {
			t.Fatalf("缺第二因素应当 403：%d %v", status, fields)
		}
		status, fields, _ = alice.call("POST", base+"/api/v1/account/set-password", `{"new-password":"n3wsecret","verify-password":"secret12","verify-code":"`+codes[2]+`"}`, nil)
		if status != 200 || string(fields["sessions_revoked"]) != "1" {
			t.Fatalf("set-password：%d %v", status, fields)
		}
		if status, _, _ := bob.call("GET", base+"/api/v1/whoami", "", nil); status != 401 {
			t.Fatalf("另一处应当被作废：%d", status)
		}
		if status, _, _ := alice.call("GET", base+"/api/v1/whoami", "", nil); status != 200 {
			t.Fatalf("当前会话应当保留：%d", status)
		}
		// 跨站的登出被拒，会话仍在。
		if status, fields, _ := alice.call("DELETE", base+"/api/v1/session", "", map[string]string{"Origin": "http://evil.example"}); status != 403 || str(fields["code"]) != "forbidden" {
			t.Fatalf("跨站登出应当 403：%d %v", status, fields)
		}
		if status, _, _ := alice.call("GET", base+"/api/v1/whoami", "", nil); status != 200 {
			t.Fatalf("跨站登出不该作废会话：%d", status)
		}
		// 同源检查也管 /mcp（带会话 cookie 的浏览器请求）与没有身份的登录入口（登录 CSRF）。
		if status, fields, _ := alice.call("POST", base+"/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{"Origin": "http://evil.example", "Accept": "application/json, text/event-stream"}); status != 403 || str(fields["code"]) != "forbidden" {
			t.Fatalf("跨站打 /mcp 应当 403：%d %v", status, fields)
		}
		if status, fields, _ := newBrowser(t).call("POST", base+"/api/v1/session", `{"username":"admin","password":"secret12"}`, map[string]string{"Origin": "http://evil.example"}); status != 403 || str(fields["code"]) != "forbidden" {
			t.Fatalf("跨站登录应当 403：%d %v", status, fields)
		}
		// 本机重置管理员密码：作废全部会话；两步验证不动；新密码能登录。
		revoked, err := cli.ResetAdminPassword(context.Background(), bdb, "admin", "resetpass1")
		if err != nil || revoked != 1 {
			t.Fatalf("admin reset-password：%d %v", revoked, err)
		}
		if status, _, _ := alice.call("GET", base+"/api/v1/whoami", "", nil); status != 401 {
			t.Fatalf("重置后旧会话应当失效：%d", status)
		}
		if status, _ := alice.login(base, "admin", "n3wsecret", codes[3]); status != 401 {
			t.Fatalf("旧密码应当失效：%d", status)
		}
		if status, fields := alice.login(base, "admin", "resetpass1", codes[3]); status != 200 || string(fields["recovery_codes_remaining"]) != "4" {
			t.Fatalf("新密码登录：%d %v", status, fields)
		}
		// 普通用户：能登录、能看自己、不能看审计。
		seedUser(t, bdb, "carol", "carolpass")
		carol := newBrowser(t)
		if status, fields := carol.login(base, "carol", "carolpass", ""); status != 200 || str(fields["role"]) != "user" {
			t.Fatalf("普通用户登录：%d %v", status, fields)
		}
		if status, fields, _ := carol.call("GET", base+"/api/v1/whoami", "", nil); status != 200 || str(fields["role"]) != "user" || str(fields["actor_kind"]) != "user" {
			t.Fatalf("普通用户身份：%d %v", status, fields)
		}
		if status, fields, _ := carol.call("GET", base+"/api/v1/audit", "", nil); status != 403 || str(fields["code"]) != "forbidden" {
			t.Fatalf("普通用户看审计应当 403：%d %v", status, fields)
		}
		if status, fields, _ := carol.call("GET", base+"/api/v1/account/show", "", nil); status != 200 || str(fields["username"]) != "carol" {
			t.Fatalf("普通用户 account show：%d %v", status, fields)
		}
	})
}
