package gate

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coresecurity "github.com/satchel/satchel/internal/core/security"
	"github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/internal/middleware/authn"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

var routes = []Route{
	{Pattern: "/api/v1/healthz", Entry: EntryMachine},
	{Pattern: "/public/", Entry: EntryMachine},
	{Pattern: "/mcp", Entry: EntryMCP},
	{Pattern: "/api/v1/", Entry: EntryPanel},
}

type fakeBans map[string]coresecurity.Ban

func (f fakeBans) Banned(ip string) (coresecurity.Ban, bool) {
	b, ok := f[ip]
	return b, ok
}

// fixture 是一扇门加一个会记下来源的 next，与一个模仿「不存在的路径」的 notFound。
type fixture struct {
	gate    *Gate
	now     time.Time
	bans    fakeBans
	reached []v1.Remote
}

func newFixture(forcePublic bool) *fixture {
	f := &fixture{now: time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC), bans: fakeBans{}}
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, v1.Newf(v1.CodeNotFound, "没有这个接口：%s %s", r.Method, r.URL.Path))
	})
	f.gate = New(routes, f.bans, notFound, forcePublic)
	f.gate.SetNow(func() time.Time { return f.now })
	return f
}

func (f *fixture) serve(r *http.Request) *httptest.ResponseRecorder {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reached = append(f.reached, v1.RemoteFrom(r.Context()))
		w.WriteHeader(http.StatusOK)
	})
	w := httptest.NewRecorder()
	f.gate.Wrap(next).ServeHTTP(w, r)
	return w
}

func req(method, target, remote, host string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = remote
	if host != "" {
		r.Host = host
	}
	return r
}

func overSocket(r *http.Request) *http.Request {
	r.RemoteAddr = "@"
	return r.WithContext(authn.MarkSocket(r.Context()))
}

func code(t *testing.T, w *httptest.ResponseRecorder) v1.Code {
	t.Helper()
	var e struct {
		Code v1.Code `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return e.Code
}

func TestEntryOf(t *testing.T) {
	g := New(routes, nil, http.NotFoundHandler(), false)
	for path, want := range map[string]Entry{
		"/api/v1/healthz": EntryMachine, "/api/v1/healthz/x": EntryPanel, "/public/a.png": EntryMachine, "/public": EntryPanel,
		"/mcp": EntryMCP, "/mcp/x": EntryPanel, "/": EntryPanel, "/api/v1/whoami": EntryPanel, "/api/v1/session/captcha": EntryPanel,
		"/some/page": EntryPanel,
	} {
		if got := g.EntryOf(path); got != want {
			t.Errorf("%s 应当属于 %d，得到 %d", path, want, got)
		}
	}
}

// master-access-gates「关闭公网访问」：本机 / 域名 / 其它 × 主控地址 https 或不是 × 三行。
func TestCloseToPublic(t *testing.T) {
	registered := []settings.TrustedProxy{proxy("127.0.0.1/32", settings.HeaderXForwardedFor)}
	paths := []struct {
		method, path string
		entry        Entry
	}{
		{http.MethodGet, "/api/v1/whoami", EntryPanel},
		{http.MethodGet, "/some/page?x=1", EntryPanel},
		{http.MethodPost, "/mcp", EntryMCP},
		{http.MethodGet, "/mcp", EntryMCP},
		{http.MethodGet, "/api/v1/healthz", EntryMachine},
		{http.MethodGet, "/public/logo.png", EntryMachine},
	}
	for _, master := range []string{"https://panel.example.com", "http://panel.example.com", ""} {
		for _, p := range paths {
			// 本机：回环、没登记反代。
			f := newFixture(false)
			f.gate.Configure(Config{MasterLocalOnly: true, MasterURL: master})
			if w := f.serve(req(p.method, p.path, "127.0.0.1:5000", "203.0.113.5:12889")); w.Code != http.StatusOK {
				t.Errorf("[%s] 本机请求 %s %s 应当放行，得到 %d", master, p.method, p.path, w.Code)
			}
			// 经 unix socket：不受影响。
			if w := f.serve(overSocket(req(p.method, p.path, "", "satchel"))); w.Code != http.StatusOK {
				t.Errorf("[%s] socket 请求 %s %s 应当放行，得到 %d", master, p.method, p.path, w.Code)
			}
			// 此后回环经登记的反代进来，不算本机。
			f.gate.Configure(Config{MasterLocalOnly: true, MasterURL: master, TrustedProxies: registered})
			domain := f.serve(req(p.method, p.path, "127.0.0.1:5000", "Panel.Example.com:12889"))
			other := f.serve(req(p.method, p.path, "127.0.0.1:5000", "203.0.113.5:12889"))
			https := strings.HasPrefix(master, "https://")
			if https && domain.Code != http.StatusOK {
				t.Errorf("[%s] 带主控域名的 %s %s 应当放行，得到 %d", master, p.method, p.path, domain.Code)
			}
			if !https && domain.Code == http.StatusOK {
				t.Errorf("[%s] 主控地址不是 https 时带域名也只放本机：%s %s", master, p.method, p.path)
			}
			page := p.method == http.MethodGet && !strings.HasPrefix(p.path, "/api/") && p.entry != EntryMCP
			switch {
			case https && page:
				if other.Code != http.StatusTemporaryRedirect || other.Header().Get("Location") != "https://panel.example.com"+p.path {
					t.Errorf("[%s] 网页请求 %s 应当 307 到主控地址，得到 %d %s", master, p.path, other.Code, other.Header().Get("Location"))
				}
			default:
				if other.Code != http.StatusForbidden || code(t, other) != v1.CodeForbidden {
					t.Errorf("[%s] %s %s 应当是 403 forbidden，得到 %d %s", master, p.method, p.path, other.Code, other.Body.String())
				}
			}
		}
	}
}

func TestCloseToPublicSwitches(t *testing.T) {
	registered := []settings.TrustedProxy{proxy("127.0.0.1/32", settings.HeaderXForwardedFor)}
	// 关掉后下一个请求就放行。
	f := newFixture(false)
	f.gate.Configure(Config{MasterLocalOnly: true, TrustedProxies: registered})
	if w := f.serve(req(http.MethodGet, "/api/v1/healthz", "127.0.0.1:5000", "")); w.Code != http.StatusForbidden {
		t.Fatalf("经反代进来不算本机，应当 403：%d", w.Code)
	}
	f.gate.Configure(Config{TrustedProxies: registered})
	if w := f.serve(req(http.MethodGet, "/api/v1/healthz", "127.0.0.1:5000", "")); w.Code != http.StatusOK {
		t.Fatalf("关掉后应当放行：%d", w.Code)
	}
	// 自救开关只跳过这一道门：静默模式照常。
	f = newFixture(true)
	f.gate.Configure(Config{MasterLocalOnly: true, TrustedProxies: registered, SilentMode: true, SilentTimeout: 15 * time.Minute})
	f.now = f.now.Add(16 * time.Minute)
	if w := f.serve(req(http.MethodGet, "/api/v1/healthz", "127.0.0.1:5000", "203.0.113.5")); w.Code != http.StatusOK {
		t.Fatalf("自救开关下关闭公网访问被跳过：%d", w.Code)
	}
	if w := f.serve(req(http.MethodGet, "/api/v1/whoami", "127.0.0.1:5000", "203.0.113.5")); w.Code != http.StatusNotFound {
		t.Fatalf("自救开关不影响静默模式：%d", w.Code)
	}
}

// master-access-gates「静默模式」。
func TestSilentMode(t *testing.T) {
	f := newFixture(false)
	f.gate.Configure(Config{SilentMode: true, SilentTimeout: 15 * time.Minute})
	panel := func() *httptest.ResponseRecorder {
		r := req(http.MethodGet, "/api/v1/whoami", "198.51.100.7:5000", "")
		r.AddCookie(&http.Cookie{Name: "satchel_session", Value: "x"})
		return f.serve(r)
	}
	// 启动后的开放期：第 5 分钟放行，第 16 分钟藏起来。
	f.now = f.now.Add(5 * time.Minute)
	if w := panel(); w.Code != http.StatusOK {
		t.Fatalf("启动后的开放期应当放行：%d", w.Code)
	}
	f.now = f.now.Add(11 * time.Minute)
	w := panel()
	if w.Code != http.StatusNotFound || code(t, w) != v1.CodeNotFound || !strings.Contains(w.Body.String(), "/api/v1/whoami") {
		t.Fatalf("锁定期内面板回与不存在的路径一样的 404：%d %s", w.Code, w.Body.String())
	}
	for k := range w.Header() {
		if strings.Contains(strings.ToLower(k), "silent") {
			t.Fatalf("不该有表明静默模式的头：%s", k)
		}
	}
	for _, r := range []*http.Request{
		req(http.MethodPost, "/api/v1/session", "198.51.100.7:5000", ""),
		req(http.MethodGet, "/api/v1/session/captcha", "198.51.100.7:5000", ""),
		req(http.MethodGet, "/", "198.51.100.7:5000", ""),
	} {
		if w := f.serve(r); w.Code != http.StatusNotFound {
			t.Errorf("锁定期内 %s %s 应当 404，得到 %d", r.Method, r.URL.Path, w.Code)
		}
	}
	// MCP、机器入口、unix socket 放行。
	for _, r := range []*http.Request{
		req(http.MethodPost, "/mcp", "198.51.100.7:5000", ""),
		req(http.MethodGet, "/api/v1/healthz", "198.51.100.7:5000", ""),
		req(http.MethodGet, "/public/logo.png", "198.51.100.7:5000", ""),
		overSocket(req(http.MethodGet, "/api/v1/whoami", "", "satchel")),
	} {
		if w := f.serve(r); w.Code != http.StatusOK {
			t.Errorf("锁定期内 %s %s 应当放行，得到 %d", r.Method, r.URL.Path, w.Code)
		}
	}
	// 解锁（M3 的订阅入口调）：开放 timeout 分钟；按当前的 timeout 算。
	f.gate.Unlock()
	if w := panel(); w.Code != http.StatusOK {
		t.Fatalf("解锁后应当放行：%d", w.Code)
	}
	f.now = f.now.Add(16 * time.Minute)
	if w := panel(); w.Code != http.StatusNotFound {
		t.Fatalf("解锁的开放期过了应当再藏起来：%d", w.Code)
	}
	f.gate.Configure(Config{SilentMode: true, SilentTimeout: 60 * time.Minute})
	if w := panel(); w.Code != http.StatusOK {
		t.Fatalf("改大 timeout 后刚结束的开放期重新打开：%d", w.Code)
	}
	// 关掉立刻恢复。
	f.now = f.now.Add(2 * time.Hour)
	f.gate.Configure(Config{SilentMode: false, SilentTimeout: 15 * time.Minute})
	if w := panel(); w.Code != http.StatusOK {
		t.Fatalf("关掉后应当放行：%d", w.Code)
	}
}

// master-login-protection「封禁的效果与恢复」：带 Authorization 头的请求在判定身份之前拒绝，别的请求不受影响。
func TestBanCheck(t *testing.T) {
	f := newFixture(false)
	until := f.now.Add(24 * time.Hour)
	f.bans["198.51.100.7"] = coresecurity.Ban{IP: "198.51.100.7", ExpiresAt: &until}
	f.bans["203.0.113.9"] = coresecurity.Ban{IP: "203.0.113.9", Permanent: true}
	withToken := func(remote, path string) *http.Request {
		r := req(http.MethodGet, path, remote, "")
		r.Header.Set("Authorization", "Bearer sat_x")
		return r
	}
	w := f.serve(withToken("198.51.100.7:5000", "/api/v1/whoami"))
	var e v1.Error
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusForbidden || e.Code != v1.CodeForbidden || e.State["ip"] != "198.51.100.7" || e.State["until"] != until.Format(time.RFC3339) {
		t.Fatalf("被封的 IP 带令牌应当 403 并给出到期时间：%d %s", w.Code, w.Body.String())
	}
	w = f.serve(withToken("203.0.113.9:5000", "/api/v1/healthz"))
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if w.Code != http.StatusForbidden || e.State["permanent"] != true {
		t.Fatalf("永久封禁连带令牌的 healthz 也拒：%d %s", w.Code, w.Body.String())
	}
	// 不带 Authorization 头：会话、登录、healthz 都不受影响。
	for _, r := range []*http.Request{
		req(http.MethodGet, "/api/v1/whoami", "198.51.100.7:5000", ""),
		req(http.MethodPost, "/api/v1/session", "198.51.100.7:5000", ""),
		req(http.MethodGet, "/api/v1/healthz", "198.51.100.7:5000", ""),
	} {
		if w := f.serve(r); w.Code != http.StatusOK {
			t.Errorf("不带令牌的 %s %s 不受封禁影响，得到 %d", r.Method, r.URL.Path, w.Code)
		}
	}
	// 没被封的、socket 来的，带令牌照常。
	if w := f.serve(withToken("198.51.100.8:5000", "/api/v1/whoami")); w.Code != http.StatusOK {
		t.Fatalf("没被封的带令牌照常：%d", w.Code)
	}
	if w := f.serve(overSocket(withToken("", "/api/v1/whoami"))); w.Code != http.StatusOK {
		t.Fatalf("socket 来的不查封禁：%d", w.Code)
	}
}

// 次序：关闭公网访问 → 静默模式 → 封禁；下游从 ctx 拿到来源。
func TestOrderAndRemote(t *testing.T) {
	f := newFixture(false)
	f.bans["198.51.100.7"] = coresecurity.Ban{IP: "198.51.100.7", Permanent: true}
	registered := []settings.TrustedProxy{proxy("127.0.0.1/32", settings.HeaderXRealIP)}
	f.gate.Configure(Config{MasterLocalOnly: true, TrustedProxies: registered, SilentMode: true, SilentTimeout: time.Minute})
	f.now = f.now.Add(2 * time.Minute)
	banned := func(path string) *http.Request {
		r := req(http.MethodGet, path, "127.0.0.1:5000", "203.0.113.5")
		r.Header.Set("X-Real-IP", "198.51.100.7")
		r.Header.Set("Authorization", "Bearer sat_x")
		return r
	}
	if w := f.serve(banned("/api/v1/whoami")); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "关闭了公网访问") {
		t.Fatalf("关闭公网访问最先：%d %s", w.Code, w.Body.String())
	}
	f.gate.Configure(Config{TrustedProxies: registered, SilentMode: true, SilentTimeout: time.Minute})
	if w := f.serve(banned("/api/v1/whoami")); w.Code != http.StatusNotFound {
		t.Fatalf("静默模式在封禁之前：被封的 IP 请求面板路径也是 404，得到 %d", w.Code)
	}
	if w := f.serve(banned("/mcp")); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "已被封禁") {
		t.Fatalf("MCP 过了静默模式，到封禁这一步拒：%d %s", w.Code, w.Body.String())
	}
	f.reached = nil
	r := req(http.MethodGet, "/api/v1/healthz", "127.0.0.1:5000", "")
	r.Header.Set("X-Real-IP", "198.51.100.99")
	r.Header.Set("X-Forwarded-Proto", "https")
	f.serve(r)
	if len(f.reached) != 1 || f.reached[0] != (v1.Remote{IP: "198.51.100.99", HTTPS: true}) {
		t.Fatalf("下游应当从 ctx 拿到来源：对端在登记里，头里的地址与 X-Forwarded-Proto 都算数：%+v", f.reached)
	}
}
