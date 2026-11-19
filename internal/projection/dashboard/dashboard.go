package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/satchel/satchel/internal/command"
	"github.com/satchel/satchel/internal/projection/rest"
	svcservers "github.com/satchel/satchel/internal/service/servers"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// Path 是仪表盘实时推送 WebSocket 的挂载路径，在入口归属表中归入 gate.EntryPanel。
const Path = command.APIPrefix + "ws/dashboard"

const (
	minRefreshInterval     = 1 * time.Second
	maxRefreshInterval     = 60 * time.Second
	defaultRefreshInterval = 5 * time.Second
	defaultPingInterval    = 30 * time.Second
	defaultPingTimeout     = 10 * time.Second
	defaultWriteTimeout    = 10 * time.Second
	clientReadLimit        = 16 << 10
)

// SnapshotFunc 读取当前管理员可见的服务器与聚合快照（绝不含任何凭据）。
type SnapshotFunc func(ctx context.Context) ([]svcservers.DashboardServer, svcservers.DashboardSummary, error)

// Frame 是经 /api/v1/ws/dashboard 下推的实时快照 JSON 帧。
type Frame struct {
	Type              string                       `json:"type"`
	Timestamp         time.Time                    `json:"timestamp"`
	RefreshIntervalMS int                          `json:"refresh_interval_ms"`
	Servers           []svcservers.DashboardServer `json:"servers"`
	Summary           svcservers.DashboardSummary  `json:"summary"`
}

// Config 是 Hub 的可选参数（测试可缩短心跳与写超时）。
type Config struct {
	Snapshot        SnapshotFunc
	RefreshInterval time.Duration
	PingInterval    time.Duration
	PingTimeout     time.Duration
	WriteTimeout    time.Duration
	Logger          *slog.Logger
	Now             func() time.Time
}

type client struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *client) writeText(ctx context.Context, timeout time.Duration, raw []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, raw)
}

// Hub 管理全部管理员仪表盘 WebSocket 连接与按需启停的惰性推送循环。
type Hub struct {
	snapshot     SnapshotFunc
	logger       *slog.Logger
	now          func() time.Time
	pingInterval time.Duration
	pingTimeout  time.Duration
	writeTimeout time.Duration

	mu           sync.Mutex
	clients      map[*client]struct{}
	interval     time.Duration
	tickerCancel context.CancelFunc
	tickerWake   chan struct{}
	tickerWG     sync.WaitGroup
	activeWG     sync.WaitGroup
	closing      bool
}

// New 创建仪表盘 WebSocket Hub。
func New(cfg Config) *Hub {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = defaultPingInterval
	}
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = defaultPingTimeout
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	return &Hub{
		snapshot:     cfg.Snapshot,
		logger:       cfg.Logger,
		now:          cfg.Now,
		pingInterval: cfg.PingInterval,
		pingTimeout:  cfg.PingTimeout,
		writeTimeout: cfg.WriteTimeout,
		clients:      map[*client]struct{}{},
		interval:     clampRefresh(cfg.RefreshInterval),
	}
}

func clampRefresh(d time.Duration) time.Duration {
	if d <= 0 {
		return defaultRefreshInterval
	}
	if d < minRefreshInterval {
		return minRefreshInterval
	}
	if d > maxRefreshInterval {
		return maxRefreshInterval
	}
	return d
}

// SetRefreshInterval 热更新广播刷新周期（由 settings.OnWrite 触发）。
func (h *Hub) SetRefreshInterval(d time.Duration) {
	clamped := clampRefresh(d)
	h.mu.Lock()
	changed := h.interval != clamped
	h.interval = clamped
	wake := h.tickerWake
	h.mu.Unlock()
	if changed && wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// ClientCount 返回当前在线管理员 WS 客户端数（便于测试验证惰性生命周期）。
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// TickerRunning 报告惰性推送循环当前是否在运行（测试用）。
func (h *Hub) TickerRunning() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tickerCancel != nil
}

// Close 优雅停止推送循环并断开所有客户端连接。
func (h *Hub) Close(ctx context.Context) {
	h.mu.Lock()
	h.closing = true
	cancel := h.tickerCancel
	h.tickerCancel = nil
	all := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		all = append(all, c)
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, c := range all {
		_ = c.conn.Close(websocket.StatusGoingAway, "server shutting down")
	}
	done := make(chan struct{})
	go func() {
		h.tickerWG.Wait()
		h.activeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ServeHTTP 处理 GET /api/v1/ws/dashboard：鉴权 → 角色 → 防 CSWSH 同源 → 升级 → 首帧即推 → 保活循环。
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		rest.NotFound(w, r)
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("token")) != "" {
		rest.WriteError(w, v1.New(v1.CodeBadRequest, "不得通过 URL 查询参数传递认证令牌").WithNext("改用会话 Cookie 或 Authorization: Bearer 请求头"))
		return
	}
	ctx := r.Context()
	id := v1.IdentityFrom(ctx)
	src := v1.CredentialSourceFrom(ctx)
	if id.IsAnonymous() || src == v1.SourceInvalid {
		rest.WriteError(w, v1.New(v1.CodeUnauthenticated, "需要管理员登录或有效管理员令牌"))
		return
	}
	if !id.IsAdmin() {
		rest.WriteError(w, v1.New(v1.CodeForbidden, "仪表盘实时推送只对管理员开放"))
		return
	}
	// 浏览器会话 Cookie 走 WebSocket GET 握手时，rest.SameOrigin 不拦 GET，必须在此显式做防 CSWSH 同源检查。
	if src == v1.SourceSession {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				rest.WriteError(w, v1.New(v1.CodeForbidden, "跨站 WebSocket 请求被拒：Origin 与主控地址不一致").WithState("origin", origin).WithState("host", r.Host))
				return
			}
		} else if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			rest.WriteError(w, v1.New(v1.CodeForbidden, "跨站 WebSocket 请求被拒：Sec-Fetch-Site 为 "+site))
			return
		}
	}

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		rest.WriteError(w, v1.New(v1.CodeUnavailable, "主控正在停止"))
		return
	}
	h.activeWG.Add(1)
	h.mu.Unlock()
	defer h.activeWG.Done()

	// 上方已完成严格的 Cookie 同源校验，且需放行非浏览器 Bearer Token 与 unix socket 连接，因此跳过 coder/websocket 内置默认 Origin 检查。
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		h.logger.Debug("仪表盘 WebSocket 握手失败", "err", err)
		return
	}
	conn.SetReadLimit(clientReadLimit)
	c := &client{conn: conn}
	if !h.addClient(c) {
		_ = conn.Close(websocket.StatusGoingAway, "server shutting down")
		return
	}
	defer func() {
		h.removeClient(c)
		conn.CloseNow()
	}()

	// 建连立即推送首帧，避免浏览器等待一个刷新周期。
	if raw, err := h.buildFrame(ctx); err == nil {
		if err := c.writeText(ctx, h.writeTimeout, raw); err != nil {
			return
		}
	}

	connCtx, stop := context.WithCancel(ctx)
	defer stop()
	go h.pingLoop(connCtx, c)
	for {
		if _, _, err := conn.Read(connCtx); err != nil {
			return
		}
	}
}

func (h *Hub) addClient(c *client) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	h.clients[c] = struct{}{}
	if len(h.clients) == 1 && h.tickerCancel == nil {
		h.startTickerLocked()
	}
	return true
}

func (h *Hub) removeClient(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	var cancel context.CancelFunc
	if len(h.clients) == 0 && h.tickerCancel != nil {
		cancel = h.tickerCancel
		h.tickerCancel = nil
		h.tickerWake = nil
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *Hub) startTickerLocked() {
	tctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	h.tickerCancel = cancel
	h.tickerWake = wake
	h.tickerWG.Add(1)
	iv := h.interval
	go func() {
		defer h.tickerWG.Done()
		timer := time.NewTimer(iv)
		defer timer.Stop()
		for {
			select {
			case <-tctx.Done():
				return
			case <-wake:
				h.mu.Lock()
				next := h.interval
				h.mu.Unlock()
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(next)
			case <-timer.C:
				h.broadcast(tctx)
				h.mu.Lock()
				next := h.interval
				h.mu.Unlock()
				timer.Reset(next)
			}
		}
	}()
}

func (h *Hub) buildFrame(ctx context.Context) ([]byte, error) {
	var (
		servers []svcservers.DashboardServer
		summary svcservers.DashboardSummary
		err     error
	)
	if h.snapshot != nil {
		qctx, cancel := context.WithTimeout(ctx, h.writeTimeout)
		servers, summary, err = h.snapshot(qctx)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	if servers == nil {
		servers = []svcservers.DashboardServer{}
	}
	h.mu.Lock()
	intervalMS := int(h.interval / time.Millisecond)
	h.mu.Unlock()
	return json.Marshal(Frame{
		Type:              "realtime",
		Timestamp:         h.now().UTC().Truncate(time.Millisecond),
		RefreshIntervalMS: intervalMS,
		Servers:           servers,
		Summary:           summary,
	})
}

func (h *Hub) broadcast(ctx context.Context) {
	raw, err := h.buildFrame(ctx)
	if err != nil {
		h.logger.Warn("构建仪表盘实时推送帧失败", "err", err)
		return
	}
	h.mu.Lock()
	targets := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		targets = append(targets, c)
	}
	h.mu.Unlock()
	for _, c := range targets {
		if err := c.writeText(ctx, h.writeTimeout, raw); err != nil {
			c.conn.CloseNow()
		}
	}
}

func (h *Hub) pingLoop(ctx context.Context, c *client) {
	t := time.NewTicker(h.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, h.pingTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				c.conn.CloseNow()
				return
			}
		}
	}
}
