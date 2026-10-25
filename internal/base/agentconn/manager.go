package agentconn

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/internal/base/selfupdate"
	"github.com/satchel/satchel/pkg/xrpc"
)

// 照 mmwx 的几个时间常数（m2-02 design 第 4、5 条）。
const (
	// DevVersion 是开发构建的版本号：主控是开发构建时不比较节点要求的最低主控版本。
	DevVersion = "0.0.0-dev"

	defaultAuthTimeout  = 10 * time.Second // 连上之后要在这么久之内完成握手与认证
	defaultPingInterval = 30 * time.Second
	defaultPingTimeout  = 10 * time.Second
	defaultFailWindow   = 60 * time.Second // 同一来源 IP 在这段时间里认证失败 failLimit 次
	defaultFailBlock    = 60 * time.Second // 就在之后这段时间里拒绝它的新连接
	failLimit           = 3
	writeTimeout        = 10 * time.Second
	// preAuthReadLimit 是握手与认证阶段一帧的上限：还没认出是谁的连接，不让它让主控备下大缓冲；认证成功后放开到 xrpc.ReadLimit。
	preAuthReadLimit = 64 << 10
)

// ErrOffline 是要请求的服务器现在没有连接。
var ErrOffline = errors.New("服务器不在线")

// errClosing 是 Shutdown 已经开始：认证完的连接不再登记。
var errClosing = errors.New("主控正在停止")

// errReply 是认证成功的回复写不出去：连接已经坏了，不再回认证失败。
var errReply = errors.New("认证成功的回复写不出去")

// Node 是认证通过的那台服务器。
type Node struct {
	ID   int64
	Name string
}

// Directory 是连接管理要用的服务器仓储（由上层把 core/servers 接进来，base 层不引用 core）。
// Lookup 按 token 找服务器，找不到时 ok 为假；Online 记一次上线或心跳；Offline 把在线的服务器标为离线。
type Directory interface {
	Lookup(ctx context.Context, token string) (node Node, ok bool, err error)
	Online(ctx context.Context, id int64, ip string, v6 bool, boot, at time.Time) error
	Offline(ctx context.Context, id int64, at time.Time) error
}

// Config 是连接管理的依赖。时间常数为零时用默认值（测试里缩短）。
type Config struct {
	Identity  ed25519.PrivateKey
	Directory Directory
	// HeartbeatInterval 是系统设置的心跳间隔（heartbeat_interval）；认证成功时下发给节点，最多是离线阈值的三分之一。
	HeartbeatInterval func(context.Context) time.Duration
	// OfflineThreshold 是离线阈值（SATCHEL_OFFLINE_THRESHOLD_SECONDS）；为零时用 db.DefaultOfflineThreshold。
	OfflineThreshold time.Duration
	// Version 是主控的版本号。
	Version string
	Logger  *slog.Logger
	Now     func() time.Time

	AuthTimeout  time.Duration
	PingInterval time.Duration
	PingTimeout  time.Duration
	FailWindow   time.Duration
	FailBlock    time.Duration
}

// Manager 管所有节点的连接：握手、认证、连接表、心跳、ping 与向节点发请求（node-channel）。
//
// 一台服务器的在线状态只由它的当前连接写：登记并写上线、写心跳、摘掉并写离线这三件事在这台服务器的锁里排队，
// 进锁之后再核对连接是否还是当前连接——旧连接的离线与心跳不会盖掉新连接写下的状态。持锁时的数据库写入都有期限，
// 等锁的认证连接照样受认证期限约束。
type Manager struct {
	cfg     Config
	base    context.Context    // Shutdown 时取消：每条连接的 ctx 都跟着它，进行中的状态写入随之取消
	stopAll context.CancelFunc // 取消 base

	mu        sync.Mutex
	conns     map[int64]*session
	locks     map[int64]serverLock // 每台服务器一把（数量与服务器一样多，不回收）
	fails     map[string]*failRecord
	lastSweep time.Time
	live      map[*websocket.Conn]struct{} // 所有正在跑的 Serve 的连接，含还没认证的：Shutdown 全部断开
	closing   bool                         // Shutdown 开始之后为真：新连接直接断开，认证完的连接不再登记
	active    sync.WaitGroup               // 正在跑的 Serve：Shutdown 等它们把离线写完；只在 closing 为假时加
}

// serverLock 是一台服务器的锁：容量为 1 的 channel，拿锁时可以带期限。
type serverLock chan struct{}

func (l serverLock) lock(ctx context.Context) error {
	select {
	case l <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l serverLock) unlock() { <-l }

type session struct {
	node         Node
	conn         *xrpc.Conn
	caller       *xrpc.Caller
	ip           string
	v6           bool
	agentVersion string
}

type failRecord struct {
	at           []time.Time
	blockedUntil time.Time
}

// New 建连接管理。
func New(cfg Config) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HeartbeatInterval == nil {
		cfg.HeartbeatInterval = func(context.Context) time.Duration { return 30 * time.Second }
	}
	for _, d := range []struct {
		p   *time.Duration
		def time.Duration
	}{{&cfg.AuthTimeout, defaultAuthTimeout}, {&cfg.PingInterval, defaultPingInterval}, {&cfg.PingTimeout, defaultPingTimeout},
		{&cfg.FailWindow, defaultFailWindow}, {&cfg.FailBlock, defaultFailBlock}, {&cfg.OfflineThreshold, db.DefaultOfflineThreshold}} {
		if *d.p <= 0 {
			*d.p = d.def
		}
	}
	base, stopAll := context.WithCancel(context.Background())
	return &Manager{cfg: cfg, base: base, stopAll: stopAll, conns: map[int64]*session{}, locks: map[int64]serverLock{},
		fails: map[string]*failRecord{}, live: map[*websocket.Conn]struct{}{}}
}

func (m *Manager) now() time.Time { return m.cfg.Now().UTC().Truncate(time.Microsecond) }

// Blocked 报告这个来源 IP 现在是否因为连续认证失败被拒绝（升级 websocket 之前查，回 429）。
func (m *Manager) Blocked(ip string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.fails[ip]
	if !ok {
		return false
	}
	now := m.cfg.Now()
	if now.Before(r.blockedUntil) {
		return true
	}
	if m.expired(r, now) {
		delete(m.fails, ip)
	}
	return false
}

// expired 报告一条失败记录是否已经没用了：封锁已过，窗口里也没有失败。
func (m *Manager) expired(r *failRecord, now time.Time) bool {
	return !now.Before(r.blockedUntil) && (len(r.at) == 0 || now.Sub(r.at[len(r.at)-1]) >= m.cfg.FailWindow)
}

// recordFailure 记一次认证失败；窗口里够 failLimit 次就封锁这个 IP 一段时间。
func (m *Manager) recordFailure(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.cfg.Now()
	r := m.fails[ip]
	if r == nil {
		r = &failRecord{}
		m.fails[ip] = r
	}
	kept := r.at[:0]
	for _, t := range r.at {
		if now.Sub(t) < m.cfg.FailWindow {
			kept = append(kept, t)
		}
	}
	r.at = append(kept, now)
	if len(r.at) >= failLimit {
		r.blockedUntil = now.Add(m.cfg.FailBlock)
		r.at = nil
	}
	// 每过一个窗口清一遍：失败一次就不再回来的来源不会一直留在内存里（能换 IPv6 地址的客户端也撑不大这张表）。
	if now.Sub(m.lastSweep) >= m.cfg.FailWindow {
		for k, v := range m.fails {
			if m.expired(v, now) {
				delete(m.fails, k)
			}
		}
		m.lastSweep = now
	}
}

// Serve 跑一条节点连接直到它断开：握手、认证、登记、心跳与请求的回复。ip 是来源 IP（已按反代登记的规则取好）。
// 任何不合规则的帧都直接断开（node-channel「握手与加密信封」）。
func (m *Manager) Serve(ctx context.Context, ws *websocket.Conn, ip string) {
	if !m.track(ws) {
		ws.CloseNow()
		return
	}
	defer m.untrack(ws)
	ctx, quit := context.WithCancel(ctx)
	defer quit()
	defer context.AfterFunc(m.base, quit)()
	conn := xrpc.NewConn(ws)
	ws.SetReadLimit(preAuthReadLimit)
	defer ws.CloseNow()
	authCtx, cancel := context.WithTimeout(ctx, m.cfg.AuthTimeout)
	defer cancel()
	if err := xrpc.ServerHandshake(authCtx, conn, m.cfg.Identity); err != nil {
		m.cfg.Logger.Debug("节点握手失败", "ip", ip, "err", err)
		ws.Close(websocket.StatusPolicyViolation, "handshake failed")
		return
	}
	s, interval, ok := m.authenticate(authCtx, conn, ip)
	if !ok {
		return
	}
	if err := m.activate(authCtx, s, interval); err != nil {
		if errors.Is(err, errReply) {
			m.cfg.Logger.Debug("认证成功的回复写不出去", "server", s.node.Name, "err", err)
			return
		}
		m.cfg.Logger.Warn("记录服务器上线失败", "server", s.node.Name, "err", err)
		msg := "主控记录上线失败，稍后重试"
		if errors.Is(err, errClosing) {
			msg = "主控正在停止，稍后重试"
		}
		reject(conn, websocket.StatusInternalError, xrpc.AuthUnavailable, msg)
		return
	}
	// 登记之后读循环不论因为什么结束，都摘掉这条连接并把服务器标为离线。
	var loopErr error
	defer func() {
		s.caller.CloseAll(xrpc.ErrClosed)
		if m.deactivate(s) {
			m.cfg.Logger.Info("节点已断开", "server", s.node.Name, "err", loopErr)
		}
	}()
	cancel()
	ws.SetReadLimit(xrpc.ReadLimit)
	m.cfg.Logger.Info("节点已连接", "server", s.node.Name, "ip", ip, "agent_version", s.agentVersion)

	connCtx, stop := context.WithCancel(ctx)
	defer stop()
	go m.pingLoop(connCtx, s)
	loopErr = m.readLoop(connCtx, s)
}

// authenticate 读认证消息并核对：token、控制协议号、最低主控版本；都过了再取心跳间隔。只检查、不写状态，
// 失败时回认证失败并断开。
func (m *Manager) authenticate(ctx context.Context, conn *xrpc.Conn, ip string) (*session, time.Duration, bool) {
	fail := func(code websocket.StatusCode, reason, msg string) (*session, time.Duration, bool) {
		reject(conn, code, reason, msg)
		return nil, 0, false
	}
	msg, err := conn.Read(ctx)
	if err != nil {
		m.cfg.Logger.Debug("读认证消息失败", "ip", ip, "err", err)
		conn.Close(websocket.StatusPolicyViolation, "bad frame")
		return nil, 0, false
	}
	if msg.Type != xrpc.TypeAuth {
		conn.Close(websocket.StatusPolicyViolation, "expect auth")
		return nil, 0, false
	}
	var auth xrpc.Auth
	if err := xrpc.Decode(msg, &auth); err != nil {
		conn.Close(websocket.StatusPolicyViolation, "bad auth")
		return nil, 0, false
	}
	node, found, err := m.cfg.Directory.Lookup(ctx, auth.Token)
	if err != nil {
		m.cfg.Logger.Warn("按 token 查服务器失败", "err", err)
		return fail(websocket.StatusInternalError, xrpc.AuthUnavailable, "主控查询服务器失败，稍后重试")
	}
	if !found {
		// 只有 token 不对才计入来源 IP 的失败次数：版本不兼容是配置问题，不是猜 token。
		m.recordFailure(ip)
		m.cfg.Logger.Warn("节点认证失败：token 无效", "ip", ip)
		return fail(websocket.StatusPolicyViolation, xrpc.AuthInvalidToken, "token 无效")
	}
	if auth.ControlProtocol > xrpc.ControlProtocol {
		m.cfg.Logger.Warn("节点认证失败：agent 比主控新", "server", node.Name, "agent_protocol", auth.ControlProtocol)
		return fail(websocket.StatusPolicyViolation, xrpc.AuthIncompatible, fmt.Sprintf(
			"agent 的控制协议号 %d 比主控的 %d 新：agent 不能先于主控升级，先升级主控", auth.ControlProtocol, xrpc.ControlProtocol))
	}
	if min := auth.MinMasterVersion; min != "" && m.cfg.Version != DevVersion && selfupdate.Valid(min) &&
		selfupdate.Compare(m.cfg.Version, min) < 0 {
		m.cfg.Logger.Warn("节点认证失败：主控版本太旧", "server", node.Name, "master", m.cfg.Version, "min", min)
		return fail(websocket.StatusPolicyViolation, xrpc.AuthIncompatible, fmt.Sprintf(
			"这个 agent 要求主控至少是 %s，当前主控是 %s：agent 不能先于主控升级，先升级主控", min, m.cfg.Version))
	}
	v6 := false
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		v6 = true
	}
	s := &session{node: node, conn: conn, caller: xrpc.NewCaller(), ip: ip, v6: v6, agentVersion: auth.AgentVersion}
	return s, m.heartbeatInterval(ctx), true
}

// heartbeatInterval 是下发给节点的心跳间隔：系统设置的值，最多是离线阈值的三分之一——心跳一直比扫描用的阈值密得多，
// 心跳间隔设得再长，在线的节点也不会被 server_offline_sweep 标成离线。
func (m *Manager) heartbeatInterval(ctx context.Context) time.Duration {
	return min(m.cfg.HeartbeatInterval(ctx), m.cfg.OfflineThreshold/3)
}

// reject 回认证失败并断开。回复用自己的写期限：认证期限可能已经到了（例如等锁超时），节点仍要收到原因，按它退避。
func reject(conn *xrpc.Conn, code websocket.StatusCode, reason, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	_ = conn.Write(ctx, xrpc.TypeAuthResult, xrpc.AuthResult{OK: false, Reason: reason, Message: msg})
	conn.Close(code, "auth failed")
}

// track 把一条新连接记进 live；Shutdown 已经开始时返回假（调用方直接断开）。
func (m *Manager) track(ws *websocket.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return false
	}
	m.live[ws] = struct{}{}
	m.active.Add(1)
	return true
}

func (m *Manager) untrack(ws *websocket.Conn) {
	m.mu.Lock()
	delete(m.live, ws)
	m.mu.Unlock()
	m.active.Done()
}

// serverLock 是这台服务器的锁：它的在线状态的写入在里面排队。
func (m *Manager) serverLock(id int64) serverLock {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.locks[id]
	if l == nil {
		l = make(serverLock, 1)
		m.locks[id] = l
	}
	return l
}

func (m *Manager) current(s *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[s.node.ID] == s
}

// activate 在这台服务器的锁里写上线、回认证成功（带心跳间隔），再把 s 登记为当前连接；已有连接时换上 s，旧连接在后台关
// （不等它的关闭握手）。先回复、后登记：登记之后 Call 才看得到这条连接，节点读到的第一帧一定是认证结果。
// ctx 是认证的期限：等锁、写上线与回复都受它约束，Shutdown 时也会被取消。Shutdown 开始之后不再登记。
func (m *Manager) activate(ctx context.Context, s *session, interval time.Duration) error {
	l := m.serverLock(s.node.ID)
	if err := l.lock(ctx); err != nil {
		return err
	}
	defer l.unlock()
	m.mu.Lock()
	closing := m.closing
	m.mu.Unlock()
	if closing {
		return errClosing
	}
	err := m.cfg.Directory.Online(ctx, s.node.ID, s.ip, s.v6, time.Time{}, m.now())
	if err == nil {
		if werr := s.conn.Write(ctx, xrpc.TypeAuthResult, xrpc.AuthResult{OK: true, HeartbeatInterval: int(interval / time.Second)}); werr != nil {
			err = fmt.Errorf("%w：%w", errReply, werr)
		}
	}
	m.mu.Lock()
	if err == nil && m.closing {
		err = errClosing
	}
	if err != nil {
		other := m.conns[s.node.ID] != nil
		m.mu.Unlock()
		// 上线可能已经写进去了（回复写不出去、写完才发现在停止，或 ctx 取消时写入已经提交）：这台服务器没有别的连接时
		// 撤销成离线；旧连接还在时不动，它仍是 connected。
		if !other {
			m.markOffline(s)
		}
		return err
	}
	old := m.conns[s.node.ID]
	m.conns[s.node.ID] = s
	m.mu.Unlock()
	if old != nil {
		m.cfg.Logger.Info("同一台服务器的新连接取代旧连接", "server", s.node.Name, "old_ip", old.ip, "new_ip", s.ip)
		go old.conn.Close(websocket.StatusNormalClosure, "replaced by a new connection")
	}
	return nil
}

// heartbeat 在这台服务器的锁里记一次心跳；s 已被新连接取代时不写。
func (m *Manager) heartbeat(ctx context.Context, s *session, boot time.Time) error {
	l := m.serverLock(s.node.ID)
	if err := l.lock(ctx); err != nil {
		return err
	}
	defer l.unlock()
	if !m.current(s) {
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return m.cfg.Directory.Online(wctx, s.node.ID, s.ip, s.v6, boot, m.now())
}

// deactivate 在这台服务器的锁里：s 还是当前连接时把它摘掉、把服务器标为离线并返回 true；已被新连接取代时返回 false、不写。
// 断线收尾一定要做，拿锁不设期限：持锁的各方（认证、心跳、收尾）都有期限，最终一定拿得到。
func (m *Manager) deactivate(s *session) bool {
	l := m.serverLock(s.node.ID)
	_ = l.lock(context.Background())
	defer l.unlock()
	m.mu.Lock()
	if m.conns[s.node.ID] != s {
		m.mu.Unlock()
		return false
	}
	delete(m.conns, s.node.ID)
	m.mu.Unlock()
	m.markOffline(s)
	return true
}

// markOffline 把服务器标为离线，持着这台服务器的锁调用。它一定要做，不跟连接的 ctx：写入最多 writeTimeout，失败只记日志。
func (m *Manager) markOffline(s *session) {
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	if err := m.cfg.Directory.Offline(ctx, s.node.ID, m.now()); err != nil {
		m.cfg.Logger.Warn("把服务器标为离线失败", "server", s.node.Name, "err", err)
	}
}

func (m *Manager) readLoop(ctx context.Context, s *session) error {
	for {
		msg, err := s.conn.Read(ctx)
		if err != nil {
			return err
		}
		switch msg.Type {
		case xrpc.TypeHeartbeat:
			var hb xrpc.Heartbeat
			if err := xrpc.Decode(msg, &hb); err != nil {
				return err
			}
			if err := m.heartbeat(ctx, s, hb.BootTime); err != nil {
				m.cfg.Logger.Warn("记录心跳失败", "server", s.node.Name, "err", err)
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := s.conn.Write(wctx, xrpc.TypeHeartbeatAck, xrpc.HeartbeatAck{ServerTime: m.now()})
			cancel()
			if err != nil {
				return err
			}
		case xrpc.TypeReply:
			var r xrpc.Reply
			if err := xrpc.Decode(msg, &r); err != nil {
				return err
			}
			s.caller.Deliver(r)
		case xrpc.TypeCall:
			// 本 change 里节点不向主控发请求：照规矩回「方法不认识」。
			var call xrpc.Call
			if err := xrpc.Decode(msg, &call); err != nil {
				return err
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := s.conn.Write(wctx, xrpc.TypeReply, xrpc.Methods{}.Serve(wctx, call))
			cancel()
			if err != nil {
				return err
			}
		default:
			m.cfg.Logger.Debug("忽略不认识的消息类型", "server", s.node.Name, "type", msg.Type)
		}
	}
}

// pingLoop 每隔 PingInterval ping 一次，PingTimeout 内没有回应就关掉连接（读循环随之结束、标离线）。
func (m *Manager) pingLoop(ctx context.Context, s *session) {
	t := time.NewTicker(m.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, m.cfg.PingTimeout)
			err := s.conn.WS().Ping(pctx)
			cancel()
			if err != nil && ctx.Err() == nil {
				m.cfg.Logger.Info("节点没有回应 ping，断开", "server", s.node.Name, "err", err)
				s.conn.WS().CloseNow()
				return
			}
		}
	}
}

// Call 向一台服务器发请求并等回复；它没有连接时立即返回 ErrOffline。timeout 为 0 时用 xrpc.DefaultTimeout。
func (m *Manager) Call(ctx context.Context, serverID int64, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	m.mu.Lock()
	s := m.conns[serverID]
	m.mu.Unlock()
	if s == nil {
		return nil, ErrOffline
	}
	return s.caller.Call(ctx, s.conn, method, params, timeout)
}

// ConnectedIDs 列出现在有连接（已登记）的服务器 id，从小到大。
func (m *Manager) ConnectedIDs() []int64 {
	m.mu.Lock()
	ids := make([]int64, 0, len(m.conns))
	for id := range m.conns {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	slices.Sort(ids)
	return ids
}

// Connected 报告这台服务器现在有没有连接。
func (m *Manager) Connected(serverID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns[serverID] != nil
}

// Shutdown 关掉所有节点连接（含还在握手、认证的），并等各条连接把服务器标为离线（主控停止时、关库之前调用；websocket
// 连接不归 http.Server 的 Shutdown 管）。开始之后新来的连接直接断开，认证完的连接不再登记，各条连接进行中的状态写入
// 被取消（不拖到关库之后才落地）。直接断开、不走关闭握手：节点不回应时关闭握手要等 5 秒，主控停止不该被它拖住；
// 节点看到断开会按退避重连。ctx 到期就不再等，之后可能还有认证失败的回复或关闭握手在后台收尾，它们不写库。
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closing = true
	m.stopAll()
	all := make([]*websocket.Conn, 0, len(m.live))
	for ws := range m.live {
		all = append(all, ws)
	}
	m.mu.Unlock()
	for _, ws := range all {
		go ws.CloseNow()
	}
	done := make(chan struct{})
	go func() { m.active.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
