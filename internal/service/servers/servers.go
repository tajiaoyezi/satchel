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
	"time"

	"github.com/satchel/satchel/internal/base/agentconn"
	"github.com/satchel/satchel/internal/command"
	core "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/securechan"
)

// ModeWebsocket 是本 change 唯一支持的连接模式。
const ModeWebsocket = "websocket"

// Service 是服务器的业务。
type Service struct {
	repo      *core.Repo
	settings  *coresettings.Repo
	masterKey ed25519.PublicKey
	logger    *slog.Logger
	conns     Conns // 建好连接管理之后由 SetConns 设上
	trafficState
}

// New 建服务；masterKey 是主控通信身份的公钥（安装命令里要带它）；logger 为空时不记日志。
func New(repo *core.Repo, settings *coresettings.Repo, masterKey ed25519.PublicKey, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Service{repo: repo, settings: settings, masterKey: masterKey, logger: logger,
		trafficState: trafficState{accs: map[int64]*accumulator{}, lastPull: map[int64]time.Time{}}}
}

// Bindings 是本服务的命令处理函数。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"server add":             s.add,
		"server install-command": s.installCommand,
		"server list":            s.list,
		"server get":             s.get,
		"server limit set":       s.limitSet,
		"server limit clear":     s.limitClear,
		"server kick":            s.kick,
		"server connections":     s.connections,
		"server traffic":         s.traffic,
	}
}

func (s *Service) add(ctx context.Context, inv *command.Invocation) (any, error) {
	name := strings.TrimSpace(inv.Arg(0))
	if name == "" {
		return nil, v1.New(v1.CodeBadRequest, "服务器的名字不能为空")
	}
	mode := inv.String("mode", ModeWebsocket)
	if mode != ModeWebsocket {
		return nil, v1.Newf(v1.CodeBadRequest, "连接模式 %q 这一版还不支持：只支持 websocket（auto、http、pull 随后续版本）", mode)
	}
	srv, err := s.repo.Create(ctx, name, mode)
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
	token := v1.Redacted
	if v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets) {
		token = srv.Status.Token.Reveal()
	}
	key := securechan.EncodePublicKey(s.masterKey)
	cmd := fmt.Sprintf("curl -fsSL '%s/api/remote/install.sh' | sh -s -- --master-url '%s' --token '%s' --master-key '%s' --mode %s",
		master, master, token, key, srv.Spec.ConnectionMode)
	return &InstallCommand{Server: srv.Spec.Name, Command: cmd, MasterURL: master, MasterKeyFingerprint: securechan.Fingerprint(s.masterKey)}, nil
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
		res.Items = append(res.Items, Summary{ID: r.Metadata.ID, Name: r.Spec.Name, ConnectionMode: r.Spec.ConnectionMode,
			Status: r.Status.Status, LastHeartbeat: r.Status.LastHeartbeat, IPAddress: r.Status.IPAddress, IPAddressV6: r.Status.IPAddressV6})
	}
	return res, nil
}

func (s *Service) get(ctx context.Context, inv *command.Invocation) (any, error) {
	return s.byName(ctx, inv.Arg(0))
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

// SweepOffline 是内置任务 server_offline_sweep：把心跳超过离线阈值的在线服务器标为离线。
func (s *Service) SweepOffline(threshold time.Duration, now func() time.Time) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		at := now().UTC().Truncate(time.Microsecond)
		n, err := s.repo.SweepOffline(ctx, at.Add(-threshold), at)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("标为离线 %d 台", n), nil
	}
}
