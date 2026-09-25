package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/captcha"
	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/db/dbtest"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// 门与登录防护的端到端（master-access-gates、master-login-protection）：真的装配、真的监听、真的库。
// 经 TCP 的请求对端都是 127.0.0.1；要模拟公网来源，就把 127.0.0.1 登记成信 X-Real-IP 的反代，再在头里写公网地址。

// clock 是端到端测试用的可拨动时钟：从真实的现在起步，只往后拨（主控的请求在别的 goroutine 里读它）。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now().UTC()} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// setClock 把门、登录限流、封禁与会话的时钟都换成 c（门的「进程启动时刻」随之重设为现在）。
func (h *harness) setClock(c *clock) {
	h.app.gate.SetNow(c.now)
	h.app.guard.SetNow(c.now)
	h.app.identity.SetNow(c.now)
}

// gates 经 unix socket 用 settings gates set 改门这一组：本机管理员，当场验证指明 admin、密码从假终端读。
func (h *harness) gates(kv ...string) envelope {
	h.t.Helper()
	cur, e, _ := h.settingsCLI("settings", "show")
	if e != nil {
		h.t.Fatal(e)
	}
	args := []string{"settings", "gates", "set"}
	for _, x := range kv {
		args = append(args, "--set", x)
	}
	args = append(args, "--resource-version", strconv.FormatInt(cur.Metadata.ResourceVersion, 10), "--verify-user", "admin", "--json")
	stdout, stderr, code := h.cliPrompt([]string{"secret12", ""}, args...)
	if code != 0 {
		h.t.Fatalf("settings gates set %v：%d %s", kv, code, stderr)
	}
	return parseEnvelope(h.t, stdout)
}

// human 经 unix socket 跑一条人类专属命令（本机管理员，当场验证指明 admin），返回 stdout、stderr 与退出码。
func (h *harness) human(args ...string) (string, string, int) {
	h.t.Helper()
	return h.cliPrompt([]string{"secret12", ""}, append(args, "--verify-user", "admin", "--json")...)
}

// reply 是一次 HTTP 请求的结果；body 不是 JSON 时 fields 为空。
type reply struct {
	status int
	fields map[string]json.RawMessage
	header http.Header
	body   string
}

func (r reply) code() string { return str(r.fields["code"]) }

// send 经 TCP 发一个请求：headers 里的 Host 改写请求的 Host；c 为 nil 时用不带 cookie、不跟随跳转的客户端。
func send(t *testing.T, c *http.Client, method, target string, headers map[string]string, body string) reply {
	t.Helper()
	if c == nil {
		c = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	return reply{status: resp.StatusCode, fields: fields, header: resp.Header, body: string(raw)}
}

func from(ip string, extra ...string) map[string]string {
	h := map[string]string{"X-Real-IP": ip}
	for i := 0; i+1 < len(extra); i += 2 {
		h[extra[i]] = extra[i+1]
	}
	return h
}

const loopbackProxy = `trusted_proxies=[{"cidr":"127.0.0.1/32","header":"X-Real-IP"}]`

// setupAdmin 在空库上经 TCP 建管理员 admin / secret12，返回拿着它会话的浏览器。
func setupAdmin(t *testing.T, base string) *browser {
	t.Helper()
	b := newBrowser(t)
	if status, fields, _ := b.call("POST", base+"/api/v1/setup/init", `{"username":"admin","password":"secret12"}`, nil); status != 200 {
		t.Fatalf("setup init：%d %v", status, fields)
	}
	return b
}

// issueToken 用管理员的会话带当场验证签一把令牌，返回明文。
func issueToken(t *testing.T, admin *browser, base, preset string) string {
	t.Helper()
	status, fields, _ := admin.call("POST", base+"/api/v1/token/create", `{"name":"t","preset":"`+preset+`","verify-password":"secret12"}`, nil)
	if status != 200 {
		t.Fatalf("token create：%d %v", status, fields)
	}
	return str(fields["token"])
}

// master-access-gates「每个入口在三道门表里的归属」：顶层 mux 的每个挂载点在入口归属表里都有一行。
func TestEntryRoutesCoverMounts(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	dataDir := shortTempDir(t)
	if err := db.EnsureDataDir(dataDir); err != nil {
		t.Fatal(err)
	}
	a, err := newApp(dataDir, bdb, slog.New(slog.NewTextHandler(io.Discard, nil)), db.ServeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, r := range entryRoutes {
		declared[r.Pattern] = true
	}
	if len(a.mounts) == 0 {
		t.Fatal("没有挂载点")
	}
	for _, m := range a.mounts {
		if !declared[m] {
			t.Errorf("挂载点 %s 没有在入口归属表里声明属于哪一行", m)
		}
	}
}

// 三道门那张表逐格走一遍（双库）：面板、MCP、机器入口、unix socket × 关闭公网访问、静默模式；外加隐藏登录入口只存不生效。
func TestGatesEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		clk := newClock()
		h.setClock(clk)
		base := h.tcpURL
		port := strings.TrimPrefix(base, "http://127.0.0.1")
		alice := setupAdmin(t, base)
		ro := issueToken(t, alice, base, "readonly")
		if err := os.WriteFile(filepath.Join(h.dataDir, db.PublicDir, "logo.png"), []byte("PNG"), 0o600); err != nil {
			t.Fatal(err)
		}

		// ── 关闭公网访问 ──
		if _, stderr, code := h.human("settings", "master-url", "set", "--url", "https://panel.example.com", "--resource-version", "1"); code != 0 {
			t.Fatalf("master-url set：%s", stderr)
		}
		h.gates("master_local_only=true")
		if r := send(t, nil, "GET", base+"/api/v1/healthz", nil, ""); r.status != 200 {
			t.Fatalf("回环、没登记反代算本机，应当放行：%d", r.status)
		}
		h.gates(loopbackProxy) // 此后回环经登记的反代进来，不算本机
		if r := send(t, nil, "GET", base+"/api/v1/healthz", map[string]string{"Host": "panel.example.com" + port}, ""); r.status != 200 {
			t.Fatalf("带主控域名的请求应当放行：%d %s", r.status, r.body)
		}
		other := map[string]string{"Host": "203.0.113.5:12889"}
		if r := send(t, nil, "GET", base+"/api/v1/healthz", other, ""); r.status != 403 || r.code() != "forbidden" || len(r.fields) != 4 {
			t.Fatalf("带 IP 的 healthz 应当 403 四字段：%d %s", r.status, r.body)
		}
		if r := send(t, nil, "GET", base+"/some/page?x=1", other, ""); r.status != 307 || r.header.Get("Location") != "https://panel.example.com/some/page?x=1" {
			t.Fatalf("网页请求应当 307 到主控地址：%d %s", r.status, r.header.Get("Location"))
		}
		if r := send(t, nil, "POST", base+"/mcp", other, `{}`); r.status != 403 {
			t.Fatalf("MCP 同面板 API，应当 403：%d", r.status)
		}
		if r := send(t, alice.c, "GET", base+"/api/v1/whoami", other, ""); r.status != 403 {
			t.Fatalf("有会话也一样被拦：%d", r.status)
		}
		if _, stderr, code := h.cli("whoami", "--json"); code != 0 {
			t.Fatalf("unix socket 不受影响：%s", stderr)
		}
		h.gates("master_local_only=false")
		if r := send(t, nil, "GET", base+"/api/v1/healthz", other, ""); r.status != 200 {
			t.Fatalf("关掉后下一个请求就放行：%d", r.status)
		}

		// ── 静默模式 ──
		h.gates("silent_mode=true", "silent_mode_timeout=15")
		clk.advance(5 * time.Minute)
		if r := send(t, alice.c, "GET", base+"/api/v1/whoami", nil, ""); r.status != 200 {
			t.Fatalf("启动后的开放期里照常：%d", r.status)
		}
		clk.advance(11 * time.Minute)
		audits := h.auditCount()
		nosuch := send(t, nil, "GET", base+"/api/v1/nosuch", nil, "")
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/api/v1/whoami", ""}, {"POST", "/api/v1/session", `{"username":"admin","password":"secret12"}`}, {"GET", "/api/v1/session/captcha", ""},
		} {
			r := send(t, alice.c, c.method, base+c.path, nil, c.body)
			if r.status != 404 || r.code() != "not_found" || len(r.fields) != 4 || r.header.Get("Content-Type") != nosuch.header.Get("Content-Type") {
				t.Fatalf("锁定期内 %s %s 应当与不存在的路径一样是 404：%d %s", c.method, c.path, r.status, r.body)
			}
			if c.method == "GET" && str(r.fields["reason"]) != strings.ReplaceAll(str(nosuch.fields["reason"]), "/api/v1/nosuch", c.path) {
				t.Fatalf("reason 只差在路径上：%s / %s", r.fields["reason"], nosuch.fields["reason"])
			}
			for k := range r.header {
				if strings.Contains(strings.ToLower(k), "silent") || strings.Contains(r.body, "静默") {
					t.Fatalf("回应里不该提到静默模式：%s %s", k, r.body)
				}
			}
		}
		if n := h.auditCount(); n != audits {
			t.Fatalf("被门拦下的请求不进审计：%d → %d", audits, n)
		}
		text, isErr := runTool(t, mcpOverTCP(t, base, ro), "whoami")
		var viaMCP identityJSON
		decodeInto(t, text, &viaMCP)
		if isErr || viaMCP.ActorKind != "token" {
			t.Fatalf("MCP 在锁定期照常：%s", text)
		}
		if r := send(t, nil, "GET", base+"/api/v1/healthz", nil, ""); r.status != 200 {
			t.Fatalf("healthz 在锁定期照常：%d", r.status)
		}
		if r := send(t, nil, "GET", base+"/public/logo.png", nil, ""); r.status != 200 || r.body != "PNG" {
			t.Fatalf("/public/ 在锁定期照常：%d", r.status)
		}
		if _, stderr, code := h.cli("whoami", "--json"); code != 0 {
			t.Fatalf("unix socket 在锁定期照常：%s", stderr)
		}
		h.gates("silent_mode=false")
		if r := send(t, alice.c, "GET", base+"/api/v1/whoami", nil, ""); r.status != 200 {
			t.Fatalf("关掉静默模式立刻恢复：%d", r.status)
		}

		// ── 隐藏登录入口只存不生效 ──
		if env := h.gates("probe_disguise_block_login=true"); string(env.Status["probe_disguise_block_login"]) != "true" {
			t.Fatalf("status 里应当读到 true：%s", env.Status["probe_disguise_block_login"])
		}
		if status, fields := newBrowser(t).login(base, "admin", "secret12", ""); status != 200 {
			t.Fatalf("隐藏登录入口不影响登录：%d %v", status, fields)
		}
	})
}

// 登录限流、令牌猜测的封禁、手动封禁、安全事件与 Turnstile（双库）。
func TestLoginProtectionEndToEnd(t *testing.T) {
	dbtest.ForEach(t, func(t *testing.T, bdb *bun.DB) {
		h := start(t, bdb)
		clk := newClock()
		h.setClock(clk)
		base := h.tcpURL
		alice := setupAdmin(t, base)
		tok := issueToken(t, alice, base, "ops")
		seedUser(t, bdb, "carol", "secret12")
		h.gates(loopbackProxy)

		// ── 令牌猜测的封禁：五次无效令牌自动封禁，只拦带令牌的请求 ──
		for i := 0; i < 5; i++ {
			if r := send(t, nil, "GET", base+"/api/v1/whoami", from("198.51.100.7", "Authorization", "Bearer sat_nosuch"), ""); r.status != 401 {
				t.Fatalf("第 %d 次无效令牌应当 401：%d", i+1, r.status)
			}
		}
		r := send(t, nil, "GET", base+"/api/v1/whoami", from("198.51.100.7", "Authorization", "Bearer "+tok), "")
		if r.status != 403 || r.code() != "forbidden" || !strings.Contains(str(r.fields["reason"]), "已被封禁") {
			t.Fatalf("被封后有效令牌也拒：%d %s", r.status, r.body)
		}
		var reason string
		var permanent bool
		if err := bdb.NewSelect().TableExpr("ip_bans").Column("reason", "permanent").Where("ip = ?", "198.51.100.7").Scan(context.Background(), &reason, &permanent); err != nil || reason != "brute_force" || permanent {
			t.Fatalf("ip_bans 应当有这一条自动封禁：%q %v %v", reason, permanent, err)
		}
		if r := send(t, alice.c, "GET", base+"/api/v1/whoami", from("198.51.100.7"), ""); r.status != 200 {
			t.Fatalf("不带令牌的会话不受封禁影响：%d", r.status)
		}
		if status, _ := newBrowserFrom(t, "198.51.100.7").login(base, "admin", "secret12", ""); status != 200 {
			t.Fatalf("登录不受封禁影响：%d", status)
		}
		if r := send(t, nil, "GET", base+"/api/v1/healthz", from("198.51.100.7"), ""); r.status != 200 {
			t.Fatalf("不带令牌的 healthz 照常：%d", r.status)
		}
		if r := send(t, nil, "GET", base+"/api/v1/healthz", from("198.51.100.7", "Authorization", "Bearer "+tok), ""); r.status != 403 {
			t.Fatalf("带令牌的 healthz 也拒：%d", r.status)
		}
		// 重启不解封：同一个库上再起一个主控。
		h2 := start(t, bdb)
		if r := send(t, nil, "GET", h2.tcpURL+"/api/v1/whoami", from("198.51.100.7", "Authorization", "Bearer "+tok), ""); r.status != 403 {
			t.Fatalf("重启后封禁照常：%d %s", r.status, r.body)
		}
		// 安全事件：四条 probe、一条 ban。
		stdout, stderr, exit := h.cli("security", "events", "list", "--ip", "198.51.100.7", "--json")
		var events struct {
			Items []struct{ Kind, Path, Detail string } `json:"items"`
			Total int                                   `json:"total"`
		}
		decodeInto(t, stdout, &events)
		if exit != 0 || events.Total != 5 || events.Items[0].Kind != "ban" || events.Items[1].Kind != "probe" || events.Items[1].Detail != "4/5" || events.Items[0].Path != "/api/v1/whoami" {
			t.Fatalf("封禁的经过：%d %s %s", exit, stdout, stderr)
		}

		// ── 手动封禁与解封 ──
		if _, stderr, code := h.human("security", "ban", "203.0.113.44"); code != 0 {
			t.Fatalf("security ban：%s", stderr)
		}
		if r := send(t, nil, "GET", base+"/api/v1/whoami", from("203.0.113.44", "Authorization", "Bearer "+tok), ""); r.status != 403 {
			t.Fatalf("手动封禁后带令牌的请求拒绝：%d", r.status)
		}
		if _, stderr, code := h.human("security", "unban", "203.0.113.44"); code != 0 {
			t.Fatalf("security unban：%s", stderr)
		}
		if r := send(t, nil, "GET", base+"/api/v1/whoami", from("203.0.113.44", "Authorization", "Bearer "+tok), ""); r.status != 200 {
			t.Fatalf("解封后照常：%d %s", r.status, r.body)
		}
		stdout, stderr, exit = h.human("security", "ban", "203.0.113.45", "--permanent")
		if exit != 0 || !strings.Contains(stdout, `"permanent":true`) || !strings.Contains(stdout, `"expires_at":null`) {
			t.Fatalf("永久封禁：%d %s %s", exit, stdout, stderr)
		}
		if stdout, _, _ := h.cli("security", "bans", "list", "--json"); !strings.Contains(stdout, "203.0.113.45") || strings.Contains(stdout, "203.0.113.44") {
			t.Fatalf("生效中的封禁列表：%s", stdout)
		}
		if _, _, code := h.human("security", "unban", "192.0.2.99"); code != v1.ExitNotFound {
			t.Fatalf("没有封禁的解封是 not_found：%d", code)
		}
		if text, isErr := h.mcpRun("security", "ban", "198.51.100.1"); !isErr || !strings.Contains(text, "human_required") {
			t.Fatalf("MCP 做不了封禁：%s", text)
		}
		stdout, _, _ = h.cli("security", "events", "list", "--ip", "203.0.113.44", "--json")
		if !strings.Contains(stdout, `"kind":"unban"`) || !strings.Contains(stdout, `"kind":"ban_manual"`) {
			t.Fatalf("手动封禁与解封都记事件：%s", stdout)
		}

		// ── 当场验证错太多次被锁，进审计 ──
		cur, _, _ := h.settingsCLI("settings", "show")
		gatesSet := func(pw string) reply {
			body := `{"set":{"silent_mode":true},"resource-version":` + strconv.FormatInt(cur.Metadata.ResourceVersion, 10) + `,"verify-password":"` + pw + `"}`
			return send(t, alice.c, "POST", base+"/api/v1/settings/gates/set", from("198.51.100.20"), body)
		}
		for i := 0; i < 5; i++ {
			if r := gatesSet("wrong"); r.status != 403 || r.code() != "human_required" {
				t.Fatalf("第 %d 次错的当场验证：%d %s", i+1, r.status, r.body)
			}
		}
		if r := gatesSet("secret12"); r.status != 429 || r.code() != "rate_limited" || !strings.Contains(string(r.fields["state"]), "until") {
			t.Fatalf("锁定后正确的密码也是 rate_limited：%d %s", r.status, r.body)
		}
		if a := h.lastAudit(); a["command"] != "settings gates set" || a["result"] != "rate_limited" {
			t.Fatalf("锁定的当场验证进审计：%v", a)
		}
		if env, _, _ := h.settingsCLI("settings", "show"); string(env.Status["silent_mode"]) != "false" {
			t.Fatal("锁定时命令不执行")
		}
		// 当场验证与登录共用账号维度：换一个地址登录 admin 也被拒。
		if status, fields := newBrowserFrom(t, "198.51.100.21").login(base, "admin", "secret12", ""); status != 429 || str(fields["code"]) != "rate_limited" {
			t.Fatalf("账号维度锁住后网页登录也拒：%d %v", status, fields)
		}
		clk.advance(61 * time.Minute)

		// ── 网页登录：账号维度跨 IP、IP 维度跨账号 ──
		for i := 0; i < 5; i++ {
			if status, _ := newBrowserFrom(t, "198.51.100.3"+strconv.Itoa(i)).login(base, "admin", "wrong", ""); status != 401 {
				t.Fatalf("第 %d 次错密码：%d", i+1, status)
			}
		}
		status, fields := newBrowserFrom(t, "198.51.100.40").login(base, "admin", "secret12", "")
		if status != 429 || str(fields["code"]) != "rate_limited" || !strings.Contains(string(fields["state"]), "until") {
			t.Fatalf("账号维度锁定后换地址也拒，state 带解锁时间：%d %v", status, fields)
		}
		clk.advance(61 * time.Minute)
		for _, name := range []string{"u1", "u2", "u3", "u4", "u5"} {
			_, _ = newBrowserFrom(t, "198.51.100.50").login(base, name, "wrong", "")
		}
		if status, _ := newBrowserFrom(t, "198.51.100.50").login(base, "carol", "secret12", ""); status != 429 {
			t.Fatalf("IP 维度锁定后这个地址登录谁都拒：%d", status)
		}
		if status, _ := newBrowserFrom(t, "198.51.100.51").login(base, "carol", "secret12", ""); status != 200 {
			t.Fatalf("换地址登录 carol 照常：%d", status)
		}

		// ── 两步登录：知道密码也不能无限次猜验证码 ──
		carol := newBrowserFrom(t, "198.51.100.60")
		if status, _ := carol.login(base, "carol", "secret12", ""); status != 200 {
			t.Fatalf("carol 登录：%d", status)
		}
		status, fields, _ = carol.call("POST", base+"/api/v1/account/totp/setup", `{"verify-password":"secret12"}`, nil)
		if status != 200 {
			t.Fatalf("totp setup：%d %v", status, fields)
		}
		secret := str(fields["secret"])
		if status, fields, _ := carol.call("POST", base+"/api/v1/account/totp/confirm", `{"code":"`+code(t, secret, clk.now())+`","verify-password":"secret12"}`, nil); status != 200 {
			t.Fatalf("totp confirm：%d %v", status, fields)
		}
		for i := 0; i < 5; i++ {
			if status, fields := newBrowserFrom(t, "198.51.100.61").login(base, "carol", "secret12", "000000"); status != 401 {
				t.Fatalf("第 %d 轮第二步错码：%d %v", i+1, status, fields)
			}
		}
		if status, _ := newBrowserFrom(t, "198.51.100.62").login(base, "carol", "secret12", ""); status != 429 {
			t.Fatalf("五轮之后第一步就是 rate_limited：%d", status)
		}
		stdout, _, _ = h.cli("security", "events", "list", "--kind", "login_locked", "--json")
		if !strings.Contains(stdout, `"path":"/api/v1/session/two-factor"`) || !strings.Contains(stdout, `"username":"carol"`) {
			t.Fatalf("触发锁定的是第二步：%s", stdout)
		}
		clk.advance(61 * time.Minute)

		// ── Turnstile ──
		var mu sync.Mutex
		var remoteIPs []string
		siteverify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			mu.Lock()
			remoteIPs = append(remoteIPs, r.PostForm.Get("remoteip"))
			mu.Unlock()
			if r.PostForm.Get("secret") == "0x4AAAAAAAsecretforsatchel" && r.PostForm.Get("response") == "good" {
				_, _ = w.Write([]byte(`{"success":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
		}))
		defer siteverify.Close()
		h.app.identity.SetCaptcha(&captcha.Client{URL: siteverify.URL, HTTP: &http.Client{Timeout: captcha.Timeout}})
		h.gates("turnstile_site_key=0x4AAAAAAAsitekeyforsatchel", "turnstile_secret_key=0x4AAAAAAAsecretforsatchel")
		if a := h.lastAudit(); a["command"] != "settings gates set" || strings.Contains(a["args_digest"].(string), "0x4AAAAAAAsecretforsatchel") {
			t.Fatalf("审计摘要里 secret key 打码：%v", a)
		}
		audits := h.auditCount()
		cfg := send(t, nil, "GET", base+"/api/v1/session/captcha", map[string]string{"Authorization": "Bearer sat_revoked"}, "")
		if cfg.status != 200 || string(cfg.fields["enabled"]) != "true" || str(cfg.fields["site_key"]) != "0x4AAAAAAAsitekeyforsatchel" || strings.Contains(cfg.body, "secret") {
			t.Fatalf("验证码配置入口：%d %s", cfg.status, cfg.body)
		}
		if n := h.auditCount(); n != audits {
			t.Fatalf("验证码配置入口不进审计：%d → %d", audits, n)
		}
		login := func(token string) reply {
			body := `{"username":"admin","password":"secret12"`
			if token != "" {
				body += `,"turnstile_token":"` + token + `"`
			}
			return send(t, newBrowser(t).c, "POST", base+"/api/v1/session", from("198.51.100.70"), body+"}")
		}
		for _, bad := range []string{"", "bad"} {
			if r := login(bad); r.status != 400 || r.code() != "bad_request" || !strings.Contains(str(r.fields["reason"]), "turnstile_token") || r.header.Get("Set-Cookie") != "" {
				t.Fatalf("验证码 %q 应当 400：%d %s", bad, r.status, r.body)
			}
		}
		if r := login("good"); r.status != 200 || r.header.Get("Set-Cookie") == "" {
			t.Fatalf("验证码通过后登录成功：%d %s", r.status, r.body)
		}
		mu.Lock()
		gotIP := remoteIPs[len(remoteIPs)-1]
		mu.Unlock()
		if gotIP != "198.51.100.70" {
			t.Fatalf("核对时带来源 IP：%q", gotIP)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		h.app.identity.SetCaptcha(&captcha.Client{URL: "http://" + ln.Addr().String(), HTTP: &http.Client{Timeout: captcha.Timeout}})
		ln.Close()
		if r := login("good"); r.status != 503 || r.code() != "unavailable" {
			t.Fatalf("验证服务连不上是 unavailable：%d %s", r.status, r.body)
		}
	})
}

// newBrowserFrom 是一个带 cookie 罐、每个请求都带 X-Real-IP 的浏览器（经登记的回环反代时，来源 IP 就是这个地址）。
func newBrowserFrom(t *testing.T, ip string) *browser {
	t.Helper()
	b := newBrowser(t)
	b.c.Transport = headerTransport{header: http.Header{"X-Real-IP": {ip}}}
	return b
}

// 自救开关与跨域（只看启动配置，SQLite 即可）。
func TestForcePublicAccessAndCORS(t *testing.T) {
	bdb := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), bdb); err != nil {
		t.Fatal(err)
	}
	h := startWith(t, bdb, db.ServeConfig{ForcePublicAccess: true, AllowedOrigins: []string{"https://dash.example.com"}})
	clk := newClock()
	h.setClock(clk)
	base := h.tcpURL
	alice := setupAdmin(t, base)
	tok := issueToken(t, alice, base, "readonly")
	if !strings.Contains(h.logs.String(), db.EnvForcePublicAccess) {
		t.Fatalf("自救开关打开时启动日志里有一条提示：%s", h.logs.String())
	}
	h.gates("master_local_only=true", loopbackProxy, "silent_mode=true", "silent_mode_timeout=1")
	clk.advance(2 * time.Minute)
	other := map[string]string{"Host": "203.0.113.5"}
	if r := send(t, nil, "GET", base+"/api/v1/healthz", other, ""); r.status != 200 {
		t.Fatalf("自救开关下关闭公网访问被跳过：%d %s", r.status, r.body)
	}
	if r := send(t, nil, "GET", base+"/api/v1/whoami", other, ""); r.status != 404 {
		t.Fatalf("自救开关不影响静默模式：%d", r.status)
	}
	if env, _, _ := h.settingsCLI("settings", "show"); string(env.Status["master_local_only"]) != "true" {
		t.Fatal("自救开关不改设置")
	}
	h.gates("silent_mode=false")

	// 跨域：列出的来源拿到跨域头、预检 204；没列出的没有；从不发 Credentials。
	r := send(t, nil, "GET", base+"/api/v1/whoami", map[string]string{"Origin": "https://dash.example.com", "Authorization": "Bearer " + tok}, "")
	if r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "https://dash.example.com" || r.header.Get("Vary") != "Origin" || r.header.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("列出的来源拿到跨域头：%d %v", r.status, r.header)
	}
	pre := send(t, nil, "OPTIONS", base+"/api/v1/whoami", map[string]string{"Origin": "https://dash.example.com", "Access-Control-Request-Method": "GET"}, "")
	if pre.status != 204 || pre.header.Get("Access-Control-Allow-Headers") != "Authorization, Content-Type" {
		t.Fatalf("预检 204：%d %v", pre.status, pre.header)
	}
	if r := send(t, nil, "GET", base+"/api/v1/whoami", map[string]string{"Origin": "https://evil.example", "Authorization": "Bearer " + tok}, ""); r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("没列出的来源照常处理、没有跨域头：%d %v", r.status, r.header)
	}
	// 默认不跨域（另起一个库：上面那个库里已经打开了关闭公网访问）。
	fresh := dbtest.OpenSQLite(t)
	if _, err := db.Migrate(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	plain := start(t, fresh)
	if r := send(t, nil, "GET", plain.tcpURL+"/api/v1/healthz", map[string]string{"Origin": "https://dash.example.com"}, ""); r.status != 200 || r.header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("不设时照常处理、不发跨域头：%d %v", r.status, r.header)
	}
	if r := send(t, nil, "OPTIONS", plain.tcpURL+"/api/v1/healthz", map[string]string{"Origin": "https://dash.example.com", "Access-Control-Request-Method": "GET"}, ""); r.status != 404 {
		t.Fatalf("不设时预检交给路由，是 404：%d", r.status)
	}
}
