package agentconn

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/satchel/satchel/internal/base/db"
	"github.com/satchel/satchel/pkg/securechan"
	"github.com/satchel/satchel/pkg/xrpc"
)

// 照 mmwx 的几个时间常数（m2-02 design 第 4、5 条）与 m2-06 轮换/冲突窗口常数。
const (
	// DevVersion 是开发构建的版本号：主控是开发构建时不比较节点要求的最低主控版本。
	DevVersion = "0.0.0-dev"

	defaultAuthTimeout      = 10 * time.Second // 连上之后要在这么久之内完成握手与认证
	defaultPingInterval     = 30 * time.Second
	defaultPingTimeout      = 10 * time.Second
	defaultFailWindow       = 60 * time.Second // 同一来源 IP 在这段时间里认证失败 failLimit 次
	defaultFailBlock        = 60 * time.Second // 就在之后这段时间里拒绝它的新连接
	defaultRotateWindow     = 30 * time.Second // token 轮换期间主控内存保留旧 token 的双凭据窗口
	defaultConflictWindow   = 60 * time.Second // 活跃或最近心跳在此时间内，不同 IP 同 token 判定冲突
	defaultConflictCooldown = 60 * time.Second // 冲突触发后冷却窗口
	defaultHTTPDialTimeout  = 5 * time.Second
	failLimit               = 3
	writeTimeout            = 10 * time.Second
	// preAuthReadLimit 是握手与认证阶段一帧的上限：还没认出是谁的连接，不让它让主控备下大缓冲；认证成功后放开到 xrpc.ReadLimit。
	preAuthReadLimit = 64 << 10
)

// ErrOffline 是要请求的服务器现在没有可用通道或不可达。
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

// TargetInfo 是主控反向请求节点加密 HTTP 管理口或只读轮询口所需的寻址与凭据信息。
type TargetInfo struct {
	ID             int64
	Name           string
	ConnectionMode string
	Domain         string
	DomainV6       string
	IPAddress      string
	IPAddressV6    string
	ListenPort     int64
	PullAddress    string
	PullAddressV6  string
	PullPort       int64
	AgentToken     string
	PullToken      string
}

// Directory 是连接管理要用的服务器仓储（由上层把 core/servers 接进来，base 层不引用 core）。
// Lookup 按当前库里有效 token 找服务器，找不到时 ok 为假；Online 记一次上线或心跳；Offline 把在线的服务器标为离线。
type Directory interface {
	Lookup(ctx context.Context, token string) (node Node, ok bool, err error)
	Online(ctx context.Context, id int64, ip string, v6 bool, boot, at time.Time) error
	Offline(ctx context.Context, id int64, at time.Time) error
}

// PendingClearer 是 Directory 的可选扩展：当节点凭当前有效 token 认证或心跳时清除 rotation_pending / revoke_pending。
type PendingClearer interface {
	ClearPending(ctx context.Context, id int64) error
}

// TargetDirectory 是 Directory 的可选扩展：按 serverID 读取管理口与轮询口反向请求所需寻址与凭据。
type TargetDirectory interface {
	Target(ctx context.Context, id int64) (TargetInfo, bool, error)
}

// SameHostUpdater 是 Directory 的可选扩展：由主控服务器端根据连接对端回环或 machine-id 计算写回同机标记。
type SameHostUpdater interface {
	SetSameHost(ctx context.Context, id int64, sameHost bool) error
}

// NodeRuntime 是主控在内存中缓存的节点最近运行态元数据（不落库表新增列，保持 0001_init 不变）。
type NodeRuntime struct {
	AgentVersion     string    `json:"agent_version,omitempty"`
	InDocker         bool      `json:"in_docker"`
	MachineID        string    `json:"machine_id,omitempty"`
	MasterURL        string    `json:"master_url,omitempty"`
	MinMasterVersion string    `json:"min_master_version,omitempty"`
	ExecRunning      bool      `json:"exec_running,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
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
	// MasterMachineID 是主控宿主机标识；为空时在 New 中尝试读取 /etc/machine-id。
	MasterMachineID string
	// CurrentMasterURL 返回主控当前已配置的规范化 https:// master_url，用于检测重连节点的旧地址并发起补推收敛。
	CurrentMasterURL func(context.Context) string
	// MasterSyncBlocked 返回指定服务器当前是否处于显式阻断旧地址自动补推的状态。
	MasterSyncBlocked func(context.Context, int64) bool
	Logger            *slog.Logger
	Now               func() time.Time
	// OnTraffic 在节点经加密 HTTP 上报 traffic 时回调，将快照汇入与 WS/Pull 相同的累计入口。
	OnTraffic func(ctx context.Context, serverID int64, snap xrpc.TrafficSnapshot) error

	AuthTimeout      time.Duration
	PingInterval     time.Duration
	PingTimeout      time.Duration
	FailWindow       time.Duration
	FailBlock        time.Duration
	RotateWindow     time.Duration
	ConflictWindow   time.Duration
	ConflictCooldown time.Duration
	SessionTTL       time.Duration
	HTTPDialTimeout  time.Duration
}

// Manager 管所有节点的连接与加密 HTTP 双向通道：握手、认证、连接表、双凭据窗口、冲突锁定、心跳与向节点发请求（node-channel）。
//
// 一台服务器的在线状态只由它的当前连接（或持有服务器锁的 HTTP 上报）写：登记并写上线、写心跳、摘掉并写离线这三件事在这台服务器的锁里排队，
// 进锁之后再核对连接是否还是当前连接——旧连接的离线与心跳不会盖掉新连接写下的状态。持锁时的数据库写入都有期限，
// 等锁的认证连接照样受认证期限约束。
type Manager struct {
	cfg     Config
	base    context.Context    // Shutdown 时取消：每条连接的 ctx 都跟着它，进行中的状态写入随之取消
	stopAll context.CancelFunc // 取消 base

	uplinkSessions *securechan.SessionCache
	nodeClient     *nodeClient

	mu                sync.Mutex
	conns             map[int64]*session
	locks             map[int64]serverLock // 每台服务器一把（数量与服务器一样多，不回收）
	fails             map[string]*failRecord
	rotations         map[string]*rotateWindow
	agentRotations    map[int64]*agentRotateWindow
	conflicts         map[int64]*conflictState
	runtimes          map[int64]NodeRuntime
	blockedMasterSync map[int64]bool
	masterPush        map[int64]*masterPushState
	lastSweep         time.Time
	live              map[*websocket.Conn]struct{} // 所有正在跑的 Serve 的连接，含还没认证的：Shutdown 全部断开
	closing           bool                         // Shutdown 开始之后为真：新连接直接断开，认证完的连接不再登记
	active            sync.WaitGroup               // 正在跑的 Serve：Shutdown 等它们把离线写完；只在 closing 为假时加
}

type masterPushState struct {
	inFlight bool
	target   string
	at       time.Time
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
	node             Node
	conn             *xrpc.Conn
	caller           *xrpc.Caller
	ip               string
	v6               bool
	agentVersion     string
	inDocker         bool
	machineID        string
	masterURL        string
	minMasterVersion string
	isCurrentTok     bool
	revoked          atomic.Bool
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
	if cfg.MasterMachineID == "" {
		cfg.MasterMachineID = HostMachineID()
	} else {
		cfg.MasterMachineID = strings.TrimSpace(cfg.MasterMachineID)
	}
	for _, d := range []struct {
		p   *time.Duration
		def time.Duration
	}{
		{&cfg.AuthTimeout, defaultAuthTimeout},
		{&cfg.PingInterval, defaultPingInterval},
		{&cfg.PingTimeout, defaultPingTimeout},
		{&cfg.FailWindow, defaultFailWindow},
		{&cfg.FailBlock, defaultFailBlock},
		{&cfg.OfflineThreshold, db.DefaultOfflineThreshold},
		{&cfg.RotateWindow, defaultRotateWindow},
		{&cfg.ConflictWindow, defaultConflictWindow},
		{&cfg.ConflictCooldown, defaultConflictCooldown},
		{&cfg.SessionTTL, securechan.DefaultSessionTTL},
		{&cfg.HTTPDialTimeout, defaultHTTPDialTimeout},
	} {
		if *d.p <= 0 {
			*d.p = d.def
		}
	}
	base, stopAll := context.WithCancel(context.Background())
	m := &Manager{
		cfg:               cfg,
		base:              base,
		stopAll:           stopAll,
		uplinkSessions:    securechan.NewSessionCache(securechan.SessionCacheConfig{TTL: cfg.SessionTTL, Now: cfg.Now}),
		conns:             map[int64]*session{},
		locks:             map[int64]serverLock{},
		fails:             map[string]*failRecord{},
		rotations:         map[string]*rotateWindow{},
		agentRotations:    map[int64]*agentRotateWindow{},
		conflicts:         map[int64]*conflictState{},
		runtimes:          map[int64]NodeRuntime{},
		blockedMasterSync: map[int64]bool{},
		masterPush:        map[int64]*masterPushState{},
		live:              map[*websocket.Conn]struct{}{},
	}
	m.nodeClient = newNodeClient(m)
	return m
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
	if s.masterURL != "" {
		go m.maybeSyncMasterURL(s.node.ID, s.node.Name, s.masterURL)
	}
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
	node, isCurrent, reason, errMsg := m.authorizeNode(ctx, ip, auth.Token, auth.ControlProtocol, auth.MinMasterVersion)
	if reason != "" {
		code := websocket.StatusPolicyViolation
		if reason == xrpc.AuthUnavailable {
			code = websocket.StatusInternalError
		}
		return fail(code, reason, errMsg)
	}
	s := &session{
		node:             node,
		conn:             conn,
		caller:           xrpc.NewCaller(),
		ip:               ip,
		v6:               isV6(ip),
		agentVersion:     auth.AgentVersion,
		inDocker:         auth.InDocker,
		machineID:        strings.TrimSpace(auth.MachineID),
		masterURL:        strings.TrimSpace(auth.MasterURL),
		minMasterVersion: strings.TrimSpace(auth.MinMasterVersion),
		isCurrentTok:     isCurrent,
	}
	return s, m.heartbeatInterval(ctx), true
}

func isV6(ip string) bool {
	if p := net.ParseIP(ip); p != nil && p.To4() == nil {
		return true
	}
	return false
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
		m.updateSameHost(ctx, s.node.ID, s.node.Name, s.ip, s.machineID)
	}
	if err == nil && s.isCurrentTok {
		if clearErr := m.clearPending(ctx, s.node.ID); clearErr != nil {
			m.cfg.Logger.Warn("清除待确认标志失败", "server", s.node.Name, "err", clearErr)
		}
	}
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
	m.recordActiveIPLocked(s.node.ID, s.ip, m.cfg.Now())
	m.recordRuntimeLocked(s.node.ID, NodeRuntime{
		AgentVersion:     s.agentVersion,
		InDocker:         s.inDocker,
		MachineID:        s.machineID,
		MasterURL:        s.masterURL,
		MinMasterVersion: s.minMasterVersion,
	})
	m.mu.Unlock()
	if old != nil {
		m.cfg.Logger.Info("同一台服务器的新连接取代旧连接", "server", s.node.Name, "old_ip", old.ip, "new_ip", s.ip)
		go old.conn.Close(websocket.StatusNormalClosure, "replaced by a new connection")
	}
	return nil
}

// heartbeat 在这台服务器的锁里记一次心跳；s 已被新连接取代时不写。
func (m *Manager) heartbeat(ctx context.Context, s *session, hb xrpc.Heartbeat) error {
	l := m.serverLock(s.node.ID)
	if err := l.lock(ctx); err != nil {
		return err
	}
	defer l.unlock()
	if !m.current(s) {
		return nil
	}
	m.mu.Lock()
	if hb.AgentVersion != "" {
		s.agentVersion = hb.AgentVersion
	}
	if hb.MachineID != "" {
		s.machineID = strings.TrimSpace(hb.MachineID)
	}
	if hb.MasterURL != "" {
		s.masterURL = strings.TrimSpace(hb.MasterURL)
	}
	if hb.AgentVersion != "" || hb.InDocker || hb.MachineID != "" || hb.MasterURL != "" {
		s.inDocker = hb.InDocker
	}
	m.recordRuntimeLocked(s.node.ID, NodeRuntime{
		AgentVersion: s.agentVersion,
		InDocker:     s.inDocker,
		MachineID:    s.machineID,
		MasterURL:    s.masterURL,
	})
	machineID := s.machineID
	m.mu.Unlock()

	m.touchConflict(s.node.ID, s.ip)
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := m.cfg.Directory.Online(wctx, s.node.ID, s.ip, s.v6, hb.BootTime, m.now()); err != nil {
		return err
	}
	m.updateSameHost(wctx, s.node.ID, s.node.Name, s.ip, machineID)
	if s.isCurrentTok {
		if err := m.clearPending(wctx, s.node.ID); err != nil {
			m.cfg.Logger.Warn("清除待确认标志失败", "server", s.node.Name, "err", err)
		}
	}
	return nil
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
			if err := m.heartbeat(ctx, s, hb); err != nil {
				m.cfg.Logger.Warn("记录心跳失败", "server", s.node.Name, "err", err)
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := s.conn.Write(wctx, xrpc.TypeHeartbeatAck, xrpc.HeartbeatAck{ServerTime: m.now()})
			cancel()
			if err != nil {
				return err
			}
			if hb.MasterURL != "" {
				go m.maybeSyncMasterURL(s.node.ID, s.node.Name, hb.MasterURL)
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

// Call 向一台服务器发请求并等回复：
//  1. 若有活跃 WebSocket 连接，优先经 WS 下发；
//  2. 否则查寻址配置（mode 为 http/pull/auto 且已知可达地址与 agent_token），按 domain -> ip_address -> domain_v6 -> ip_address_v6
//     搭配 listen_port（默认 23889）经加密 HTTP 管理口发送；
//  3. 均不可达时立即返回 ErrOffline。timeout 为 0 时用 xrpc.DefaultTimeout。
func (m *Manager) Call(ctx context.Context, serverID int64, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	m.mu.Lock()
	s := m.conns[serverID]
	m.mu.Unlock()
	if s != nil && !s.revoked.Load() {
		return s.caller.Call(ctx, s.conn, method, params, timeout)
	}
	return m.nodeClient.CallManage(ctx, serverID, method, params, timeout)
}

// PullTraffic 从指定服务器的只读轮询口（pull_port，默认 23890）拉取一份流量快照，成功时同时返回实际命中的节点地址与是否 IPv6。
func (m *Manager) PullTraffic(ctx context.Context, serverID int64, timeout time.Duration) (xrpc.TrafficSnapshot, string, bool, error) {
	return m.nodeClient.PullTraffic(ctx, serverID, timeout)
}

// ConnectedIDs 列出现在有活跃 WS 连接（已登记且未吊销）的服务器 id，从小到大。
func (m *Manager) ConnectedIDs() []int64 {
	m.mu.Lock()
	ids := make([]int64, 0, len(m.conns))
	for id, s := range m.conns {
		if !s.revoked.Load() {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	slices.Sort(ids)
	return ids
}

// Connected 报告这台服务器现在有没有活跃 WS 连接。
func (m *Manager) Connected(serverID int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.conns[serverID]
	return s != nil && !s.revoked.Load()
}

// Runtime 返回指定服务器最近缓存的节点运行态元数据（不存在时 ok 为 false）。
func (m *Manager) Runtime(serverID int64) (NodeRuntime, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.runtimes[serverID]
	return rt, ok
}

// RecordRuntime 合并并刷新指定服务器的内存运行态缓存（非空字符串覆盖旧值）。
func (m *Manager) RecordRuntime(serverID int64, rt NodeRuntime) {
	if serverID <= 0 {
		return
	}
	m.mu.Lock()
	m.recordRuntimeLocked(serverID, rt)
	m.mu.Unlock()
}

func (m *Manager) recordRuntimeLocked(serverID int64, rt NodeRuntime) {
	cur := m.runtimes[serverID]
	if v := strings.TrimSpace(rt.AgentVersion); v != "" {
		cur.AgentVersion = v
	}
	if v := strings.TrimSpace(rt.MachineID); v != "" {
		cur.MachineID = v
	}
	if v := strings.TrimSpace(rt.MasterURL); v != "" {
		cur.MasterURL = v
	}
	if v := strings.TrimSpace(rt.MinMasterVersion); v != "" {
		cur.MinMasterVersion = v
	}
	if rt.AgentVersion != "" || rt.InDocker || rt.MachineID != "" || rt.MasterURL != "" {
		cur.InDocker = rt.InDocker
	}
	cur.ExecRunning = rt.ExecRunning
	cur.UpdatedAt = m.now()
	m.runtimes[serverID] = cur
}

// SetMasterSyncBlocked 设置或解除指定服务器的旧地址自动补推阻断标记。
func (m *Manager) SetMasterSyncBlocked(serverID int64, blocked bool) {
	if serverID <= 0 {
		return
	}
	m.mu.Lock()
	if blocked {
		m.blockedMasterSync[serverID] = true
	} else {
		delete(m.blockedMasterSync, serverID)
	}
	m.mu.Unlock()
}

// Disconnect 断开节点连接并清理内存会话与缓存后将数据库状态置为 offline（保留服务器配置记录）。
func (m *Manager) Disconnect(ctx context.Context, serverID int64) error {
	if serverID <= 0 {
		return nil
	}
	l := m.serverLock(serverID)
	if err := l.lock(ctx); err != nil {
		return err
	}
	defer l.unlock()

	m.InvalidateServer(serverID, true, roleManage, rolePull)
	m.mu.Lock()
	delete(m.conns, serverID)
	delete(m.runtimes, serverID)
	delete(m.masterPush, serverID)
	m.mu.Unlock()

	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return m.cfg.Directory.Offline(wctx, serverID, m.now())
}

func (m *Manager) sameHost(ip, machineID string) bool {
	if p := net.ParseIP(strings.TrimSpace(ip)); p != nil && p.IsLoopback() {
		return true
	}
	mid := strings.TrimSpace(machineID)
	return m.cfg.MasterMachineID != "" && mid != "" && mid == m.cfg.MasterMachineID
}

func (m *Manager) updateSameHost(ctx context.Context, id int64, name, ip, machineID string) {
	upd, ok := m.cfg.Directory.(SameHostUpdater)
	if !ok {
		return
	}
	if err := upd.SetSameHost(ctx, id, m.sameHost(ip, machineID)); err != nil {
		m.cfg.Logger.Warn("写同机判定标记失败", "server", name, "err", err)
	}
}

func (m *Manager) isMasterSyncBlocked(ctx context.Context, serverID int64) bool {
	m.mu.Lock()
	blocked := m.blockedMasterSync[serverID]
	m.mu.Unlock()
	if blocked {
		return true
	}
	if m.cfg.MasterSyncBlocked != nil {
		return m.cfg.MasterSyncBlocked(ctx, serverID)
	}
	return false
}

const (
	masterPushCooldown = 5 * time.Second
	masterPushTimeout  = 10 * time.Second
)

func normalizeMasterOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Host == "" {
		return ""
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if path != "" {
		return "https://" + strings.ToLower(u.Host) + path
	}
	return "https://" + strings.ToLower(u.Host)
}

func (m *Manager) maybeSyncMasterURL(serverID int64, serverName, reportedURL string) {
	if m.cfg.CurrentMasterURL == nil || serverID <= 0 {
		return
	}
	reported := normalizeMasterOrigin(reportedURL)
	if reported == "" {
		return
	}
	target := normalizeMasterOrigin(m.cfg.CurrentMasterURL(m.base))
	if target == "" {
		return
	}
	if reported == target {
		m.mu.Lock()
		delete(m.masterPush, serverID)
		m.mu.Unlock()
		return
	}
	if m.isMasterSyncBlocked(m.base, serverID) {
		return
	}
	now := m.now()
	m.mu.Lock()
	st := m.masterPush[serverID]
	if st != nil && (st.inFlight || (st.target == target && now.Sub(st.at) < masterPushCooldown)) {
		m.mu.Unlock()
		return
	}
	st = &masterPushState{inFlight: true, target: target, at: now}
	m.masterPush[serverID] = st
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(m.base, masterPushTimeout)
	defer cancel()
	_, err := m.Call(ctx, serverID, xrpc.MethodAgentUpdateMaster, xrpc.UpdateMasterParams{MasterURL: target}, masterPushTimeout)

	m.mu.Lock()
	if cur := m.masterPush[serverID]; cur == st {
		st.inFlight = false
	}
	if err == nil {
		if rt, ok := m.runtimes[serverID]; ok {
			rt.MasterURL = target
			m.runtimes[serverID] = rt
		}
	}
	m.mu.Unlock()

	if err != nil {
		m.cfg.Logger.Warn("向旧地址重连节点补推新 master_url 失败", "server", serverName, "reported", reported, "target", target, "err", err)
		return
	}
	m.cfg.Logger.Info("已向旧地址重连节点自动补推新 master_url", "server", serverName, "old", reported, "new", target)
}

// Shutdown 关掉所有节点连接（含还在握手、认证的），并等各条连接把服务器标为离线（主控停止时、关库之前调用；websocket
// 连接不归 http.Server 的 Shutdown 管）。开始之后新来的连接直接断开，认证完的连接不再登记，各条连接进行中的状态写入
// 被取消（不拖到关库之后才落地）。直接断开、不走关闭握手：节点不回应时关闭握手要等 5 秒，主控停止不该被它拖住；
// 节点看到断开会按退避重连。ctx 到期就不再等，之后可能还有认证失败的回复或关闭握手在后台收尾，它们不写库。
func (m *Manager) Shutdown(ctx context.Context) {
	m.nodeClient.CloseIdleConnections()
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
