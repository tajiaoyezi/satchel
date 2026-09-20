package rest

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/middleware/authn"
	"github.com/satchel/satchel/internal/service/auth"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// fakeSessions 是会话入口的假业务：一个账号 admin / pw，两步验证按开关。
type fakeSessions struct {
	twoFactor  bool
	loggedOut  []string
	issuedFor  []string
	rememberMe bool
}

var expires = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

func (f *fakeSessions) Login(_ context.Context, username, password string, rememberMe bool) (*auth.LoginResult, error) {
	if username != "admin" || password != "pw" {
		return nil, v1.New(v1.CodeUnauthenticated, "用户名或密码不对")
	}
	f.rememberMe = rememberMe
	if f.twoFactor {
		return &auth.LoginResult{TwoFactorRequired: true, Pending: "pending-1"}, nil
	}
	return &auth.LoginResult{Username: "admin", Role: v1.RoleAdmin, ExpiresAt: expires, Token: "tok-login"}, nil
}

func (f *fakeSessions) CompleteTwoFactor(_ context.Context, pending, code string) (*auth.LoginResult, error) {
	if pending != "pending-1" || code != "123456" {
		return nil, v1.New(v1.CodeUnauthenticated, "验证码不对")
	}
	n := 7
	return &auth.LoginResult{Username: "admin", Role: v1.RoleAdmin, ExpiresAt: expires, Token: "tok-2fa", RecoveryCodesRemaining: &n}, nil
}

func (f *fakeSessions) Logout(_ context.Context, token string) error {
	f.loggedOut = append(f.loggedOut, token)
	return nil
}

func (f *fakeSessions) IssueSession(_ context.Context, username string, _ bool) (string, time.Time, error) {
	f.issuedFor = append(f.issuedFor, username)
	return "tok-setup", expires, nil
}

// raw 发一个请求并把 recorder 交回来（要看 Set-Cookie 头）。
func raw(t *testing.T, h http.Handler, method, path, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == authn.SessionCookie {
			return c
		}
	}
	return nil
}

func fields(t *testing.T, rec *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var f map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatalf("body 不是 JSON 对象：%s", rec.Body.String())
	}
	return f
}

// master-web-session「登录」「登出」与 master-two-factor「登录第二步」：三个入口的形状、错误码与 cookie 属性。
func TestSessionEndpoints(t *testing.T) {
	fs := &fakeSessions{}
	h := NewHandler(testTable(t), &echo{}, fs)

	rec := raw(t, h, "POST", SessionPath, `{"username":"admin","password":"pw","remember_me":true}`, nil)
	if rec.Code != 200 || !fs.rememberMe {
		t.Fatalf("登录应当 200 且带上记住我：%d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "tok-login") {
		t.Fatalf("令牌只进 cookie，不进 JSON：%s", rec.Body.String())
	}
	f := fields(t, rec)
	if string(f["username"]) != `"admin"` || string(f["role"]) != `"admin"` || !strings.Contains(string(f["expires_at"]), "2030-01-02") {
		t.Fatalf("登录响应形状：%v", f)
	}
	c := sessionCookie(t, rec)
	if c == nil || c.Value != "tok-login" || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Secure || !c.Expires.Equal(expires) {
		t.Fatalf("cookie 属性：%+v", c)
	}
	// TLS 到达时带 Secure。
	rec = raw(t, h, "POST", SessionPath, `{"username":"admin","password":"pw"}`, func(r *http.Request) { r.TLS = &tls.ConnectionState{} })
	if c := sessionCookie(t, rec); c == nil || !c.Secure {
		t.Fatalf("TLS 下 cookie 应当 Secure：%+v", c)
	}
	// 密码错：401 四字段，无 cookie。
	rec = raw(t, h, "POST", SessionPath, `{"username":"admin","password":"nope"}`, nil)
	if rec.Code != 401 || errorOf(t, fields(t, rec)).Code != v1.CodeUnauthenticated || sessionCookie(t, rec) != nil {
		t.Fatalf("密码错应当 401 且无 cookie：%d %s", rec.Code, rec.Body.String())
	}
	// 未知字段与非 JSON 是 400。
	for _, body := range []string{`{"username":"admin","password":"pw","extra":1}`, `not json`, `[]`} {
		rec = raw(t, h, "POST", SessionPath, body, nil)
		if rec.Code != 400 || errorOf(t, fields(t, rec)).Code != v1.CodeBadRequest {
			t.Errorf("%s 应当 400：%d %s", body, rec.Code, rec.Body.String())
		}
	}
	// 开了两步验证：不发 cookie，给 pending。
	fs.twoFactor = true
	rec = raw(t, h, "POST", SessionPath, `{"username":"admin","password":"pw"}`, nil)
	f = fields(t, rec)
	if rec.Code != 200 || string(f["two_factor_required"]) != "true" || string(f["pending"]) != `"pending-1"` || sessionCookie(t, rec) != nil {
		t.Fatalf("两步验证第一步：%d %s %+v", rec.Code, rec.Body.String(), sessionCookie(t, rec))
	}
	rec = raw(t, h, "POST", SessionTwoFactorPath, `{"pending":"pending-1","code":"123456"}`, nil)
	f = fields(t, rec)
	if rec.Code != 200 || string(f["recovery_codes_remaining"]) != "7" {
		t.Fatalf("第二步成功：%d %s", rec.Code, rec.Body.String())
	}
	if c := sessionCookie(t, rec); c == nil || c.Value != "tok-2fa" || !c.HttpOnly {
		t.Fatalf("第二步应当发 cookie：%+v", c)
	}
	rec = raw(t, h, "POST", SessionTwoFactorPath, `{"pending":"pending-1","code":"000000"}`, nil)
	if rec.Code != 401 || sessionCookie(t, rec) != nil {
		t.Fatalf("第二步失败应当 401 无 cookie：%d", rec.Code)
	}
	// 登出：带 cookie 就作废它，响应清 cookie。
	rec = raw(t, h, "DELETE", SessionPath, "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: authn.SessionCookie, Value: "tok-login"}) })
	if rec.Code != 200 || len(fs.loggedOut) != 1 || fs.loggedOut[0] != "tok-login" {
		t.Fatalf("登出：%d %v", rec.Code, fs.loggedOut)
	}
	if c := sessionCookie(t, rec); c == nil || c.Value != "" || c.MaxAge != -1 || !c.HttpOnly {
		t.Fatalf("登出应当清 cookie（Max-Age=0）：%+v", c)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("Set-Cookie 头应当含 Max-Age=0：%s", rec.Header().Get("Set-Cookie"))
	}
	// 没 cookie 的登出也 200。
	if rec = raw(t, h, "DELETE", SessionPath, "", nil); rec.Code != 200 {
		t.Fatalf("没 cookie 的登出：%d", rec.Code)
	}
	// 其它方法与不挂会话入口时都是 404。
	for _, m := range []string{"GET", "PUT", "PATCH"} {
		if rec = raw(t, h, m, SessionPath, "", nil); rec.Code != 404 {
			t.Errorf("%s 会话入口应当 404：%d", m, rec.Code)
		}
	}
	if rec = raw(t, h, "GET", SessionTwoFactorPath, "", nil); rec.Code != 404 {
		t.Fatalf("GET 第二步入口应当 404：%d", rec.Code)
	}
	noSessions := NewHandler(testTable(t), &echo{}, nil)
	if rec = raw(t, noSessions, "POST", SessionPath, `{"username":"admin","password":"pw"}`, nil); rec.Code != 404 {
		t.Fatalf("没装会话业务时入口不存在：%d", rec.Code)
	}
}

// source 模拟 authn：给请求塞身份与身份来源。
func source(id v1.Identity, src v1.CredentialSource, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := v1.WithCredentialSource(v1.WithIdentity(r.Context(), id), src)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// master-web-session「同源检查」：只对身份来自会话 cookie 的写请求。
func TestSameOrigin(t *testing.T) {
	user := v1.Identity{Actor: "alice", ActorKind: v1.ActorUser, Role: v1.RoleAdmin}
	e := &echo{}
	viaSession := source(user, v1.SourceSession, SameOrigin(NewHandler(testTable(t), e, &fakeSessions{})))
	viaSocket := source(v1.LocalAdmin("root"), v1.SourceSocket, SameOrigin(NewHandler(testTable(t), e, &fakeSessions{})))
	anonymous := SameOrigin(NewHandler(testTable(t), e, &fakeSessions{}))
	post := func(h http.Handler, header map[string]string) int {
		rec := raw(t, h, "POST", "/api/v1/demo/remove/alice", `{"confirm":"alice"}`, func(r *http.Request) {
			r.Host = "satchel.example:8080"
			for k, v := range header {
				r.Header.Set(k, v)
			}
		})
		if rec.Code == 403 {
			if errorOf(t, fields(t, rec)).Code != v1.CodeForbidden {
				t.Fatalf("403 应当是 forbidden：%s", rec.Body.String())
			}
		}
		return rec.Code
	}
	cases := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"Origin 同源", map[string]string{"Origin": "http://satchel.example:8080"}, 200},
		{"Origin 同源 https", map[string]string{"Origin": "https://satchel.example:8080"}, 200},
		{"Origin 不同 host", map[string]string{"Origin": "http://evil.example"}, 403},
		{"Origin 不同端口", map[string]string{"Origin": "http://satchel.example:9"}, 403},
		{"Origin null", map[string]string{"Origin": "null"}, 403},
		{"Sec-Fetch-Site cross-site", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"Sec-Fetch-Site same-site", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"Sec-Fetch-Site same-origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		{"Sec-Fetch-Site none", map[string]string{"Sec-Fetch-Site": "none"}, 200},
		{"两个头都没有（非浏览器）", nil, 200},
		{"Origin 对但 Sec-Fetch-Site 跨站：Origin 优先", map[string]string{"Origin": "http://satchel.example:8080", "Sec-Fetch-Site": "cross-site"}, 200},
	}
	for _, tc := range cases {
		if got := post(viaSession, tc.header); got != tc.want {
			t.Errorf("会话身份 %s：应当 %d，得到 %d", tc.name, tc.want, got)
		}
	}
	// socket 身份不受检查；会话身份的 GET 也不受检查。
	if got := post(viaSocket, map[string]string{"Origin": "http://evil.example"}); got != 200 {
		t.Fatalf("socket 身份不该做同源检查：%d", got)
	}
	rec := raw(t, viaSession, "GET", "/api/v1/whoami", "", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	if rec.Code != 200 {
		t.Fatalf("会话身份的 GET 不做同源检查：%d", rec.Code)
	}
	// 登出（DELETE）是写请求：跨站的登出被拒。
	rec = raw(t, viaSession, "DELETE", SessionPath, "", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example"); r.Host = "satchel.example:8080" })
	if rec.Code != 403 {
		t.Fatalf("跨站登出应当 403：%d", rec.Code)
	}
	// 没有身份的写请求（登录入口）同样检查：跨站的登录被拒（登录 CSRF），同源或非浏览器放行到业务。
	login := func(header map[string]string) int {
		rec := raw(t, anonymous, "POST", SessionPath, `{"username":"admin","password":"pw"}`, func(r *http.Request) {
			r.Host = "satchel.example:8080"
			for k, v := range header {
				r.Header.Set(k, v)
			}
		})
		return rec.Code
	}
	if got := login(map[string]string{"Origin": "http://evil.example"}); got != 403 {
		t.Fatalf("跨站登录应当 403：%d", got)
	}
	if got := login(map[string]string{"Sec-Fetch-Site": "cross-site"}); got != 403 {
		t.Fatalf("Sec-Fetch-Site 跨站的登录应当 403：%d", got)
	}
	if got := login(map[string]string{"Origin": "http://satchel.example:8080"}); got != 200 {
		t.Fatalf("同源登录应当到业务：%d", got)
	}
	if got := login(nil); got != 200 {
		t.Fatalf("非浏览器客户端登录应当到业务：%d", got)
	}
	// 没有身份的 GET 不检查。
	rec = raw(t, anonymous, "GET", "/api/v1/healthz", "", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") })
	if rec.Code != 200 {
		t.Fatalf("无身份的 GET 不做同源检查：%d", rec.Code)
	}
}

// master-setup-wizard「经 REST 成功顺手发 cookie」与 master-human-verification「REST 从请求体读当场验证的值」。
func TestSetupCookieAndVerifyDecoding(t *testing.T) {
	tbl, err := command.New(
		&command.Command{Path: []string{"setup", "init"}, Summary: "s", Class: command.ClassAction, Anonymous: true,
			Flags: []command.Flag{{Name: "username", Type: command.TypeString}, {Name: "password", Type: command.TypePassword}}},
		&command.Command{Path: []string{"account", "set-password"}, Summary: "s", Class: command.ClassAction, HumanOnly: true,
			Flags: []command.Flag{{Name: "new-password", Type: command.TypePassword}}},
		&command.Command{Path: []string{"whoami"}, Summary: "s", Class: command.ClassRead},
		&command.Command{Path: []string{"demo", "poke"}, Summary: "s", Class: command.ClassAction},
	)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeSessions{}
	setup := command.RunnerFunc(func(_ context.Context, inv *command.Invocation) (any, error) {
		if inv.Name() == "setup init" {
			if inv.Flags["password"] != "secret12" {
				return nil, errors.New("password 应当按字符串收进 Flags")
			}
			return auth.SetupResult{Username: inv.Flags["username"].(string), Role: v1.RoleAdmin}, nil
		}
		return map[string]any{"flags": inv.Flags, "verify": inv.Verify}, nil
	})
	h := NewHandler(tbl, setup, fs)
	rec := raw(t, h, "POST", "/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil)
	if rec.Code != 200 || len(fs.issuedFor) != 1 || fs.issuedFor[0] != "admin" {
		t.Fatalf("setup init 应当成功并为 admin 发会话：%d %s %v", rec.Code, rec.Body.String(), fs.issuedFor)
	}
	if c := sessionCookie(t, rec); c == nil || c.Value != "tok-setup" || !c.HttpOnly {
		t.Fatalf("setup init 成功应当带 cookie：%+v", c)
	}
	if f := fields(t, rec); string(f["username"]) != `"admin"` || string(f["role"]) != `"admin"` {
		t.Fatalf("setup init 的输出：%v", f)
	}
	// 没装会话业务：setup init 照常成功，只是没有 cookie。
	rec = raw(t, NewHandler(tbl, setup, nil), "POST", "/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil)
	if rec.Code != 200 || sessionCookie(t, rec) != nil {
		t.Fatalf("没装会话业务时不发 cookie：%d %+v", rec.Code, sessionCookie(t, rec))
	}
	// 人类专属命令：verify-* 进 Verify、不进 flags；password 类型的 flag 按字符串收。
	rec = raw(t, h, "POST", "/api/v1/account/set-password", `{"new-password":"n3wpass!","verify-password":"pw","verify-code":"123456","verify-user":"admin"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("set-password：%d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Flags  map[string]any        `json:"flags"`
		Verify *command.Verification `json:"verify"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Flags["new-password"] != "n3wpass!" || len(out.Flags) != 1 || out.Verify == nil || out.Verify.Password != "pw" || out.Verify.Code != "123456" || out.Verify.User != "admin" {
		t.Fatalf("解码结果：%+v", out)
	}
	// 非字符串的 verify 值当没给；password 类型给了非字符串是 bad_request。
	rec = raw(t, h, "POST", "/api/v1/account/set-password", `{"new-password":"n3wpass!","verify-password":1}`, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out.Verify == nil || out.Verify.Password != "" {
		t.Fatalf("非字符串的 verify-password 应当当作没给：%d %+v", rec.Code, out.Verify)
	}
	rec = raw(t, h, "POST", "/api/v1/account/set-password", `{"new-password":12345678}`, nil)
	if rec.Code != 400 {
		t.Fatalf("password 类型给数字应当 400：%d %s", rec.Code, rec.Body.String())
	}
	// 不是人类专属的命令收到 verify-* 是没有这个参数。
	rec = raw(t, h, "POST", "/api/v1/demo/poke", `{"verify-password":"pw"}`, nil)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "没有参数 verify-password") {
		t.Fatalf("非人类专属命令的 verify-* 应当 400：%d %s", rec.Code, rec.Body.String())
	}
}
