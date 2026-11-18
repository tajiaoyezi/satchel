// Package servers 是服务器的业务：server add / install-command / list / get 四条命令与给节点连接管理（base/agentconn）用的
// 服务器仓储适配（m2-02 node-pairing、node-channel），以及经通道指挥节点的限速、踢线、列连接与流量的拉取和累计
// （m2-03 node-limiter、node-traffic）。
package servers

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/release"
	"github.com/satchel/satchel/pkg/securechan"
)

// 服务器的四种连接模式，透传 core 层枚举。
const (
	ModeAuto      = core.ModeAuto
	ModeWebsocket = core.ModeWebsocket
	ModeHTTP      = core.ModeHTTP
	ModePull      = core.ModePull
)

// Service 是服务器的业务。
type Service struct {
	repo           *core.Repo
	settings       *coresettings.Repo
	masterKey      ed25519.PublicKey
	masterVersion  string
	releaseSources release.Sources
	startJob       func(ctx context.Context, serverID int64, inv *command.Invocation, run func(ctx context.Context) (any, error), done func()) (any, error)
	pollInterval   time.Duration
	pollTimeout    time.Duration
	updateTimeout  time.Duration
	logger         *slog.Logger
	conns          Conns // 建好连接管理之后由 SetConns 设上
	trafficState
	lifecycleMu sync.Mutex
	updating    map[int64]bool
}

// New 建服务；masterKey 是主控通信身份的公钥（安装命令里要带它）；logger 为空时不记日志。
func New(repo *core.Repo, settings *coresettings.Repo, masterKey ed25519.PublicKey, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{
		repo:           repo,
		settings:       settings,
		masterKey:      masterKey,
		releaseSources: defaultAgentReleaseSources(),
		pollInterval:   defaultUpdatePollInterval,
		pollTimeout:    defaultUpdatePollTimeout,
		updateTimeout:  defaultUpdateRPCTimeout,
		logger:         logger,
		trafficState:   trafficState{accs: map[int64]*accumulator{}, lastPull: map[int64]time.Time{}, srvLocks: map[int64]*sync.Mutex{}},
		updating:       map[int64]bool{},
	}
}

// Bindings 是本服务的命令处理函数。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"server add":                s.add,
		"server install-command":    s.installCommand,
		"server list":               s.list,
		"server get":                s.get,
		"server token show":         s.tokenShow,
		"server token rotate":       s.tokenRotate,
		"server token revoke":       s.tokenRevoke,
		"server limit set":          s.limitSet,
		"server limit clear":        s.limitClear,
		"server kick":               s.kick,
		"server connections":        s.connections,
		"server traffic":            s.traffic,
		"server restart-kernel":     s.restartKernel,
		"server update check":       s.updateCheck,
		"server update apply":       s.updateApply,
		"server update batch":       s.updateBatch,
		"server uninstall":          s.uninstall,
		"server switch-listen-port": s.switchListenPort,
		"server probe-master-url":   s.probeMasterURL,
	}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "服务器管理只对管理员开放")
	}
	return nil
}

func summaryOf(r *core.Server) Summary {
	return Summary{
		ID: r.Metadata.ID, Name: r.Spec.Name, ConnectionMode: r.Spec.ConnectionMode,
		Status: r.Status.Status, LastHeartbeat: r.Status.LastHeartbeat, IPAddress: r.Status.IPAddress, IPAddressV6: r.Status.IPAddressV6,
	}
}

func hasControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func (s *Service) add(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(inv.Arg(0))
	if name == "" {
		return nil, v1.New(v1.CodeBadRequest, "服务器的名字不能为空")
	}
	if hasControlChar(name) {
		return nil, v1.New(v1.CodeBadRequest, "服务器名字不能包含控制字符")
	}
	mode := strings.TrimSpace(inv.String("mode", core.ModeAuto))
	if !core.ValidMode(mode) {
		return nil, v1.Newf(v1.CodeBadRequest, "连接模式 %q 不支持：必须是 auto、websocket、http 或 pull 之一", mode)
	}
	domain := strings.TrimSpace(inv.String("domain", ""))
	domainV6 := strings.TrimSpace(inv.String("domain-v6", ""))
	pullAddr := strings.TrimSpace(inv.String("pull-address", ""))
	pullAddrV6 := strings.TrimSpace(inv.String("pull-address-v6", ""))
	for field, val := range map[string]string{
		"domain":          domain,
		"domain-v6":       domainV6,
		"pull-address":    pullAddr,
		"pull-address-v6": pullAddrV6,
	} {
		if hasControlChar(val) {
			return nil, v1.Newf(v1.CodeBadRequest, "--%s 不能包含控制字符", field)
		}
	}
	listenPort := inv.Int("listen-port", 0)
	pullPort := inv.Int("pull-port", 0)
	if listenPort < 0 || listenPort > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "--listen-port 必须在 0..65535 之间，得到 %d", listenPort)
	}
	if pullPort < 0 || pullPort > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "--pull-port 必须在 0..65535 之间，得到 %d", pullPort)
	}
	effListen := listenPort
	if effListen == 0 {
		effListen = core.DefaultListenPort
	}
	effPull := pullPort
	if effPull == 0 {
		effPull = core.DefaultPullPort
	}
	if (listenPort > 0 || pullPort > 0) && effListen == effPull {
		return nil, v1.Newf(v1.CodeBadRequest, "管理监听端口（%d）与轮询只读端口（%d）不能相同", effListen, effPull)
	}
	if mode == core.ModePull && domain == "" && domainV6 == "" && pullAddr == "" && pullAddrV6 == "" {
		return nil, v1.New(v1.CodeBadRequest, "pull 模式至少要提供一个可达地址（--pull-address、--pull-address-v6、--domain 或 --domain-v6）")
	}
	srv, err := s.repo.CreateWith(ctx, core.CreateParams{
		Name:          name,
		Mode:          mode,
		Domain:        domain,
		DomainV6:      domainV6,
		ListenPort:    int64(listenPort),
		PullAddress:   pullAddr,
		PullAddressV6: pullAddrV6,
		PullPort:      int64(pullPort),
	})
	if err != nil {
		if e := v1.AsError(err); e.Code == v1.CodeNameTaken {
			return nil, e.WithNext("换一个名字，或用 server list 看已有的服务器")
		}
		return nil, err
	}
	return srv, nil
}

func (s *Service) byName(ctx context.Context, name string) (*core.Server, error) {
	srv, err := s.repo.ByName(ctx, name)
	if errors.Is(err, core.ErrNotFound) {
		return nil, v1.Newf(v1.CodeNotFound, "没有名为 %q 的服务器", name).WithNext("用 server list 查看已有的服务器")
	}
	return srv, err
}

// InstallCommand 是 server install-command 的结果。
type InstallCommand struct {
	Server               string `json:"server"`
	Command              string `json:"command"`
	MasterURL            string `json:"master_url"`
	MasterKeyFingerprint string `json:"master_key_fingerprint"`
}

// installCommand 给出一键安装命令：主控地址必须已设且是 https（第 06 章「安装命令只在主控开启 HTTPS 后生成」）；
// token 原文只给带密钥读取的身份，否则放打码标记。
func (s *Service) installCommand(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	st, err := s.settings.Load(ctx)
	if err != nil {
		return nil, err
	}
	master := st.Gates().MasterURL
	if u, perr := url.Parse(master); master == "" || perr != nil || u.Scheme != "https" || u.Host == "" {
		return nil, v1.Newf(v1.CodeConflict, "安装命令只在主控地址是 https 时生成，当前主控地址是 %q", master).
			WithNext("请人在主控本机用 settings master-url set 设一个 https 的主控地址（人类专属，也可以在网页的「系统设置」页改），再取安装命令")
	}
	hasSecrets := v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets)
	revealOrRedact := func(sec v1.Secret) string {
		if hasSecrets {
			return sec.Reveal()
		}
		return v1.Redacted
	}
	mode := srv.Spec.ConnectionMode
	if mode == "" {
		mode = core.ModeAuto
	}
	token := revealOrRedact(srv.Status.Token)
	key := securechan.EncodePublicKey(s.masterKey)
	cmd := fmt.Sprintf("curl -fsSL %s | sh -s -- --master-url %s --token %s --master-key %s --mode %s",
		shellSingleQuote(master+"/api/remote/install.sh"), shellSingleQuote(master), shellSingleQuote(token), shellSingleQuote(key), shellModeArg(mode))
	if mode != core.ModeWebsocket {
		cmd += " --agent-token " + shellSingleQuote(revealOrRedact(srv.Status.AgentToken))
		if srv.Spec.ListenPort > 0 {
			cmd += fmt.Sprintf(" --listen-port %d", srv.Spec.ListenPort)
		}
	}
	if mode == core.ModeAuto || mode == core.ModePull {
		cmd += " --pull-token " + shellSingleQuote(revealOrRedact(srv.Status.PullToken))
		if srv.Spec.PullPort > 0 {
			cmd += fmt.Sprintf(" --pull-port %d", srv.Spec.PullPort)
		}
	}
	return &InstallCommand{Server: srv.Spec.Name, Command: cmd, MasterURL: master, MasterKeyFingerprint: securechan.Fingerprint(s.masterKey)}, nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellModeArg(mode string) string {
	if mode != "" {
		safe := true
		for i := 0; i < len(mode); i++ {
			b := mode[i]
			if !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-') {
				safe = false
				break
			}
		}
		if safe {
			return mode
		}
	}
	return shellSingleQuote(mode)
}

// Summary 是 server list 的一行。
type Summary struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	ConnectionMode string     `json:"connection_mode"`
	Status         string     `json:"status"`
	LastHeartbeat  *time.Time `json:"last_heartbeat"`
	IPAddress      *string    `json:"ip_address"`
	IPAddressV6    string     `json:"ip_address_v6"`
}

func (s *Service) list(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	afterID, err := command.DecodeIDCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	total, err := s.repo.Count(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.List(ctx, afterID, page.Limit+1)
	if err != nil {
		return nil, err
	}
	res := &command.PageResult{Items: make([]any, 0, len(rows)), Total: total}
	if len(rows) > page.Limit {
		rows = rows[:page.Limit]
		res.NextCursor = command.EncodeIDCursor(rows[len(rows)-1].Metadata.ID)
	}
	for _, r := range rows {
		res.Items = append(res.Items, summaryOf(r))
	}
	return res, nil
}

func (s *Service) get(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return s.byName(ctx, inv.Arg(0))
}

// Overview 返回开局总览里的服务器分区（master-overview）：管理员按 id 升序返回前 limit 条摘要与总台数；
// 普通用户（role 为 user 的会话或令牌）只返回空数组与 0，不报错。
func (s *Service) Overview(ctx context.Context, limit int) ([]any, int, error) {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return []any{}, 0, nil
	}
	total, err := s.repo.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 || limit <= 0 {
		return []any{}, total, nil
	}
	rows, err := s.repo.List(ctx, 0, limit)
	if err != nil {
		return nil, 0, err
	}
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, summaryOf(r))
	}
	return items, total, nil
}

// Directory 把服务器仓储接给节点连接管理。
func (s *Service) Directory() agentconn.Directory { return directory{s.repo} }

type directory struct{ repo *core.Repo }

func (d directory) Lookup(ctx context.Context, token string) (agentconn.Node, bool, error) {
	srv, err := d.repo.ByToken(ctx, token)
	if errors.Is(err, core.ErrNotFound) {
		return agentconn.Node{}, false, nil
	}
	if err != nil {
		return agentconn.Node{}, false, err
	}
	return agentconn.Node{ID: srv.Metadata.ID, Name: srv.Spec.Name}, true, nil
}

func (d directory) Online(ctx context.Context, id int64, ip string, v6 bool, boot, at time.Time) error {
	return d.repo.MarkOnline(ctx, id, ip, v6, boot, at)
}

func (d directory) Offline(ctx context.Context, id int64, at time.Time) error {
	_, err := d.repo.MarkOffline(ctx, id, at)
	return err
}

func (d directory) ClearPending(ctx context.Context, id int64) error {
	return d.repo.ClearPendingFlags(ctx, id)
}

func (d directory) SetSameHost(ctx context.Context, id int64, sameHost bool) error {
	return d.repo.SetSameHost(ctx, id, sameHost)
}

func (d directory) Target(ctx context.Context, id int64) (agentconn.TargetInfo, bool, error) {
	srv, err := d.repo.ByID(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return agentconn.TargetInfo{}, false, nil
	}
	if err != nil {
		return agentconn.TargetInfo{}, false, err
	}
	var ipv4 string
	if srv.Status.IPAddress != nil {
		ipv4 = *srv.Status.IPAddress
	}
	return agentconn.TargetInfo{
		ID:             srv.Metadata.ID,
		Name:           srv.Spec.Name,
		ConnectionMode: srv.Spec.ConnectionMode,
		Domain:         srv.Spec.Domain,
		DomainV6:       srv.Spec.DomainV6,
		IPAddress:      ipv4,
		IPAddressV6:    srv.Status.IPAddressV6,
		ListenPort:     srv.Spec.ListenPort,
		PullAddress:    srv.Spec.PullAddress,
		PullAddressV6:  srv.Spec.PullAddressV6,
		PullPort:       srv.Spec.PullPort,
		AgentToken:     srv.Status.AgentToken.Reveal(),
		PullToken:      srv.Status.PullToken.Reveal(),
	}, true, nil
}

// SweepOffline 是内置任务 server_offline_sweep：把心跳超过离线阈值的在线服务器标为离线，
// 并对无活跃 WS 且具备轮询可达地址的 auto 服务器统计失联轮次，满 3 轮置 fallback_to_pull=true。
func (s *Service) SweepOffline(threshold time.Duration, now func() time.Time) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		at := now().UTC().Truncate(time.Microsecond)
		before := at.Add(-threshold)
		n, err := s.repo.SweepOffline(ctx, before, at)
		if err != nil {
			return "", err
		}
		activeWS := map[int64]bool{}
		if s.conns != nil {
			for _, id := range s.conns.ConnectedIDs() {
				activeWS[id] = true
			}
		}
		fallback, err := s.repo.SweepAutoFallback(ctx, before, at, activeWS)
		if err != nil {
			return "", err
		}
		if fallback > 0 {
			return fmt.Sprintf("标为离线 %d 台，转入轮询 %d 台", n, fallback), nil
		}
		return fmt.Sprintf("标为离线 %d 台", n), nil
	}
}
