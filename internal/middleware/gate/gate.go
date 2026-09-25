// Package gate 是横切层的门（master-access-gates）：在判定身份之前，先算出一个请求从哪来（来源 IP、是否本机、是否经 HTTPS
// 到达）放进 ctx，再按第 10 章「三道门与各入口」那张表依次过关闭公网访问、静默模式与 IP 封禁（master-login-protection）。
// 经 unix socket 进来的请求不属于表里任何一行，门一律不拦：能连上那个 0600 socket 的只有 root 与运行主控的用户。
package gate

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	coresecurity "github.com/satchel/satchel/internal/core/security"
	"github.com/satchel/satchel/internal/core/settings"
	"github.com/satchel/satchel/internal/middleware/authn"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Entry 是一个 HTTP 入口在三道门表里属于哪一行。
type Entry int

const (
	// EntryPanel 是面板：面板页面、登录与验证码入口、面板 API。静默模式锁定时藏起来。
	EntryPanel Entry = iota
	// EntryMCP 是 /mcp：关闭公网访问同面板 API，静默模式放行。
	EntryMCP
	// EntryMachine 是机器入口：节点通道、订阅、探针公开接口、TG webhook、测速端、联邦入站（M1 里是 healthz 与 /public/）。静默模式放行。
	EntryMachine
)

// Route 是入口归属表的一行：Pattern 以 / 结尾时管整棵子树，否则只管这一个路径（与 http.ServeMux 的写法相同）。
type Route struct {
	Pattern string
	Entry   Entry
}

// Config 是门用到的设置，来自系统设置门这一组（装配根在启动与每次设置写之后调 Configure）。
type Config struct {
	TrustedProxies  []settings.TrustedProxy
	MasterLocalOnly bool
	MasterURL       string
	SilentMode      bool
	SilentTimeout   time.Duration
}

// ConfigFrom 从门这一组的设置里取出门要的部分。
func ConfigFrom(g settings.Gates) Config {
	return Config{TrustedProxies: g.TrustedProxies, MasterLocalOnly: g.MasterLocalOnly, MasterURL: g.MasterURL,
		SilentMode: g.SilentMode, SilentTimeout: g.SilentModeTimeout}
}

// Bans 是生效中封禁的查询（service/security 实现，只查内存）。
type Bans interface {
	Banned(ip string) (coresecurity.Ban, bool)
}

// Gate 是门的状态：入口归属表、当前设置、时钟、进程启动与最近一次解锁的时刻。
type Gate struct {
	routes      []Route
	bans        Bans
	notFound    http.Handler
	forcePublic bool

	mu       sync.RWMutex
	cfg      Config
	now      func() time.Time
	started  time.Time
	unlocked time.Time
}

// New 建门。routes 是入口归属表（没有匹配的路径算面板）；notFound 是「请求一个不存在的路径」的那个处理器，静默模式锁定时
// 原样用它回应，所以藏起来的入口与真的不存在的路径看不出差别；forcePublic 是启动时读到的自救开关（只跳过关闭公网访问）。
func New(routes []Route, bans Bans, notFound http.Handler, forcePublic bool) *Gate {
	sorted := append([]Route(nil), routes...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].Pattern) > len(sorted[j].Pattern) })
	now := func() time.Time { return time.Now().UTC() }
	return &Gate{routes: sorted, bans: bans, notFound: notFound, forcePublic: forcePublic, now: now, started: now()}
}

// Configure 换上新的设置，下一个请求就按它判定。
func (g *Gate) Configure(c Config) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cfg = c
}

// SetNow 换掉时钟，并把「进程启动的时刻」重设为新时钟的现在（测试把时间往后拨，不用真的等开放期过去）。
func (g *Gate) SetNow(now func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now = now
	g.started = now()
}

// Unlock 从现在起让面板对所有来源开放 silent_mode_timeout 分钟：M3 的订阅入口在有效用户拉到订阅、身份校验通过之后调它。
func (g *Gate) Unlock() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.unlocked = g.now()
}

// EntryOf 返回一个路径在三道门表里属于哪一行：取最长的匹配，没有匹配的算面板（不认识的东西默认藏起来）。
func (g *Gate) EntryOf(path string) Entry {
	for _, r := range g.routes {
		if (strings.HasSuffix(r.Pattern, "/") && strings.HasPrefix(path, r.Pattern)) || path == r.Pattern {
			return r.Entry
		}
	}
	return EntryPanel
}

// silentLocked 报告静默模式此刻是不是在锁定期：开着，且既不在启动后的开放期、也不在最近一次解锁后的开放期里（长度按当前的 timeout 算）。
func (g *Gate) silentLocked(cfg Config, now time.Time) bool {
	if !cfg.SilentMode {
		return false
	}
	g.mu.RLock()
	started, unlocked := g.started, g.unlocked
	g.mu.RUnlock()
	if now.Before(started.Add(cfg.SilentTimeout)) {
		return false
	}
	return unlocked.IsZero() || !now.Before(unlocked.Add(cfg.SilentTimeout))
}

// Wrap 把门套在 next 外面。经 TCP 的请求：算来源 → 关闭公网访问 → 静默模式 → 封禁检查（只看带 Authorization 头的）→ next；
// 经 unix socket 的请求只记下来源（socket、本机）就放行。被门拦下的请求到不了命令执行链，也就不进审计。
func (g *Gate) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authn.OverSocket(r.Context()) {
			next.ServeHTTP(w, r.WithContext(v1.WithRemote(r.Context(), v1.Remote{Socket: true, Local: true})))
			return
		}
		g.mu.RLock()
		cfg, now := g.cfg, g.now()
		g.mu.RUnlock()
		remote := resolve(r, cfg.TrustedProxies)
		r = r.WithContext(v1.WithRemote(r.Context(), remote))
		entry := g.EntryOf(r.URL.Path)
		if cfg.MasterLocalOnly && !g.forcePublic && !remote.Local && !domainAllowed(r, cfg.MasterURL) {
			g.refuseRemote(w, r, entry, cfg.MasterURL)
			return
		}
		if entry == EntryPanel && g.silentLocked(cfg, now) {
			g.notFound.ServeHTTP(w, r)
			return
		}
		if len(r.Header.Values("Authorization")) > 0 && g.bans != nil {
			if b, banned := g.bans.Banned(remote.IP); banned {
				writeError(w, bannedError(b))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// httpsHost 返回主控地址的主机名；主控地址不是 https 时返回空（这时只有本机来的请求能进）。
func httpsHost(masterURL string) string {
	u, err := url.Parse(masterURL)
	if err != nil || u.Scheme != "https" {
		return ""
	}
	return u.Hostname()
}

// domainAllowed 报告请求的 Host（去掉端口、不分大小写）是不是主控地址的域名；只在主控地址是 https 时成立（照 mmwx）。
func domainAllowed(r *http.Request, masterURL string) bool {
	host := httpsHost(masterURL)
	if host == "" {
		return false
	}
	reqHost := r.Host
	if h, _, err := net.SplitHostPort(reqHost); err == nil {
		reqHost = h
	}
	return strings.EqualFold(strings.Trim(reqHost, "[]"), host)
}

// refuseRemote 回应被关闭公网访问拦下的请求：主控地址是 https 时，网页请求（GET / HEAD、不在 /api/ 下、不是 MCP）307 跳过去
// （307 不会被浏览器长期缓存，关掉设置后不会一直跳）；其余是四字段的 forbidden（CLI 不跟随跳转，/api/v1/ 下也不出现 HTML）。
func (g *Gate) refuseRemote(w http.ResponseWriter, r *http.Request, entry Entry, masterURL string) {
	host := httpsHost(masterURL)
	if host != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/") && entry != EntryMCP {
		http.Redirect(w, r, strings.TrimRight(masterURL, "/")+r.URL.RequestURI(), http.StatusTemporaryRedirect)
		return
	}
	e := v1.New(v1.CodeForbidden, "主控关闭了公网访问：只接受本机与经主控地址进来的请求").WithState("host", r.Host)
	if host != "" {
		e = e.WithNext("改用 " + strings.TrimRight(masterURL, "/") + " 访问")
	} else {
		e = e.WithNext("在主控本机访问；要关掉这道门，在主控本机执行 satchel settings gates set --set master_local_only=false --resource-version <N> --verify-user <管理员账号>")
	}
	writeError(w, e)
}

func bannedError(b coresecurity.Ban) *v1.Error {
	e := v1.Newf(v1.CodeForbidden, "来源 IP %s 已被封禁，带令牌的请求一律拒绝", b.IP).WithState("ip", b.IP)
	if b.Permanent {
		e = e.WithState("permanent", true)
	} else if b.ExpiresAt != nil {
		e = e.WithState("until", b.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return e.WithNext("等封禁到期；管理员可以在主控本机执行 satchel security unban " + b.IP)
}

// writeError 写四字段错误（门在中间件层，不能引用投影层的 rest.WriteError，状态码同样按 v1.HTTPStatusOf 折算）。
func writeError(w http.ResponseWriter, e *v1.Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(v1.HTTPStatusOf(e.Code))
	_ = json.NewEncoder(w).Encode(e)
}
