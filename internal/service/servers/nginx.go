package servers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

const (
	serviceControlTimeout = 30 * time.Second
	nginxInstallTimeout   = 5 * time.Minute
)

// CallServiceStatus 调用节点 service.status 管理 RPC。
func (s *Service) CallServiceStatus(ctx context.Context, serverID int64) (*xrpc.ServiceStatusResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.ServiceStatusResult
	if err := s.call(ctx, srv, xrpc.MethodServiceStatus, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallServiceControl 调用节点 service.control 管理 RPC。
func (s *Service) CallServiceControl(ctx context.Context, serverID int64, params xrpc.ServiceControlParams) (*xrpc.ServiceControlResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.ServiceControlResult
	if err := s.call(ctx, srv, xrpc.MethodServiceControl, params, &res, serviceControlTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallNginxInstall 调用节点 nginx.install 管理 RPC。
func (s *Service) CallNginxInstall(ctx context.Context, serverID int64, params xrpc.NginxInstallParams) (*xrpc.NginxInstallResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NginxInstallResult
	if err := s.call(ctx, srv, xrpc.MethodNginxInstall, params, &res, nginxInstallTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallNginxUninstall 调用节点 nginx.uninstall 管理 RPC。
func (s *Service) CallNginxUninstall(ctx context.Context, serverID int64, params xrpc.NginxUninstallParams) (*xrpc.NginxUninstallResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NginxUninstallResult
	if err := s.call(ctx, srv, xrpc.MethodNginxUninstall, params, &res, nginxInstallTimeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// CallNginxServers 调用节点 nginx.servers 管理 RPC。
func (s *Service) CallNginxServers(ctx context.Context, serverID int64) (*xrpc.NginxServersResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NginxServersResult
	if err := s.call(ctx, srv, xrpc.MethodNginxServers, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	if res.Servers == nil {
		res.Servers = []xrpc.NginxServerBlock{}
	}
	return &res, nil
}

// CallNginxClearStream 调用节点 nginx.clear_stream 管理 RPC。
func (s *Service) CallNginxClearStream(ctx context.Context, serverID int64, params xrpc.NginxClearStreamParams) (*xrpc.NginxClearStreamResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NginxClearStreamResult
	if err := s.call(ctx, srv, xrpc.MethodNginxClearStream, params, &res, serviceControlTimeout); err != nil {
		return nil, err
	}
	if res.RemovedFiles == nil {
		res.RemovedFiles = []string{}
	}
	return &res, nil
}

// CallNginxWebsitesScan 调用节点 nginx.websites.scan 管理 RPC（供 service/websites 经小接口复用）。
func (s *Service) CallNginxWebsitesScan(ctx context.Context, serverID int64) (*xrpc.NginxWebsitesScanResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NginxWebsitesScanResult
	if err := s.call(ctx, srv, xrpc.MethodNginxWebsitesScan, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	if res.Sites == nil {
		res.Sites = []xrpc.ScannedWebsite{}
	}
	return &res, nil
}

// CallNginxWebsitesApply 调用节点 nginx.websites.apply 管理 RPC（供 service/apply 经小接口复用）。
func (s *Service) CallNginxWebsitesApply(ctx context.Context, serverID int64, params xrpc.NginxWebsitesApplyParams) (*xrpc.NginxWebsitesApplyResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.NginxWebsitesApplyResult
	if err := s.call(ctx, srv, xrpc.MethodNginxWebsitesApply, params, &res, serviceControlTimeout); err != nil {
		return nil, err
	}
	if res.Applied == nil {
		res.Applied = []xrpc.AppliedWebsiteFile{}
	}
	if res.Removed == nil {
		res.Removed = []string{}
	}
	return &res, nil
}

// ServiceStatusView 是 server service status 的输出。
type ServiceStatusView struct {
	Server           string                   `json:"server"`
	SameHostAsMaster bool                     `json:"same_host_as_master"`
	Kernel           xrpc.KernelServiceStatus `json:"kernel"`
	Nginx            xrpc.NginxServiceStatus  `json:"nginx"`
	Agent            xrpc.AgentServiceStatus  `json:"agent"`
	CheckedAt        time.Time                `json:"checked_at"`
}

func (s *Service) serviceStatus(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	var res xrpc.ServiceStatusResult
	if err := s.call(ctx, srv, xrpc.MethodServiceStatus, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	return &ServiceStatusView{
		Server:           srv.Spec.Name,
		SameHostAsMaster: srv.Status.SameHostAsMaster,
		Kernel:           res.Kernel,
		Nginx:            res.Nginx,
		Agent:            res.Agent,
		CheckedAt:        res.CheckedAt,
	}, nil
}

// ServiceControlView 是 server service control 的输出。
type ServiceControlView struct {
	Server  string                   `json:"server"`
	Service string                   `json:"service"`
	Action  string                   `json:"action"`
	Running bool                     `json:"running"`
	Message string                   `json:"message,omitempty"`
	Status  xrpc.ServiceStatusResult `json:"status"`
}

func (s *Service) serviceControl(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	svcName, err := required(inv, "service")
	if err != nil {
		return nil, err
	}
	svcName = strings.ToLower(svcName)
	action, err := required(inv, "action")
	if err != nil {
		return nil, err
	}
	action = strings.ToLower(action)

	switch svcName {
	case "kernel":
		switch action {
		case "start", "stop", "restart":
		default:
			return nil, v1.Newf(v1.CodeBadRequest, "kernel 服务不支持 --action %q（仅支持 start、stop 或 restart）", action)
		}
	case "nginx":
		switch action {
		case "start", "stop", "restart", "reload":
		default:
			return nil, v1.Newf(v1.CodeBadRequest, "nginx 服务不支持 --action %q（仅支持 start、stop、restart 或 reload）", action)
		}
	case "agent":
		return nil, v1.New(v1.CodeBadRequest, "禁止远程控制 agent 自身以防节点失联（升级请用 server update apply，卸载请用 server uninstall）")
	default:
		return nil, v1.Newf(v1.CodeBadRequest, "不支持的 --service %q（仅支持 kernel 或 nginx）", svcName)
	}

	if srv.Status.SameHostAsMaster && svcName == "nginx" {
		if err := command.VerifyDynamicHuman(ctx, s.getVerifier(), "server service control",
			fmt.Sprintf("控制主控同机节点 %s 的 nginx 服务（%s）", srv.Spec.Name, action), inv); err != nil {
			return nil, err
		}
	}

	unlock := s.LockServerConfig(srv.Metadata.ID)
	defer unlock()

	var res xrpc.ServiceControlResult
	if err := s.call(ctx, srv, xrpc.MethodServiceControl, xrpc.ServiceControlParams{
		Service: svcName,
		Action:  action,
	}, &res, serviceControlTimeout); err != nil {
		return nil, err
	}
	return &ServiceControlView{
		Server:  srv.Spec.Name,
		Service: res.Service,
		Action:  res.Action,
		Running: res.Running,
		Message: res.Message,
		Status:  res.Status,
	}, nil
}

// NginxInstallView 是 server nginx install 长任务的最终输出。
type NginxInstallView struct {
	Server           string `json:"server"`
	Installed        bool   `json:"installed"`
	AlreadyInstalled bool   `json:"already_installed"`
	InDocker         bool   `json:"in_docker"`
	Version          string `json:"version,omitempty"`
	PackageManager   string `json:"package_manager,omitempty"`
	Message          string `json:"message,omitempty"`
}

func (s *Service) nginxInstall(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.startJob == nil {
		return nil, v1.New(v1.CodeInternal, "长任务服务未装配，不能受理 server nginx install")
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	if srv.Status.SameHostAsMaster {
		if err := command.VerifyDynamicHuman(ctx, s.getVerifier(), "server nginx install",
			fmt.Sprintf("在主控同机节点 %s 上安装或初始化 nginx", srv.Spec.Name), inv); err != nil {
			return nil, err
		}
	}
	return s.startJob(ctx, srv.Metadata.ID, inv, func(jobCtx context.Context) (any, error) {
		unlock := s.LockServerConfig(srv.Metadata.ID)
		defer unlock()
		var res xrpc.NginxInstallResult
		if err := s.call(jobCtx, srv, xrpc.MethodNginxInstall, xrpc.NginxInstallParams{}, &res, nginxInstallTimeout); err != nil {
			return nil, err
		}
		return &NginxInstallView{
			Server:           srv.Spec.Name,
			Installed:        res.Installed,
			AlreadyInstalled: res.AlreadyInstalled,
			InDocker:         res.InDocker,
			Version:          res.Version,
			PackageManager:   res.PackageManager,
			Message:          res.Message,
		}, nil
	}, nil)
}

// NginxUninstallView 是 server nginx uninstall 长任务的最终输出。
type NginxUninstallView struct {
	Server      string `json:"server"`
	Uninstalled bool   `json:"uninstalled"`
	InDocker    bool   `json:"in_docker"`
	Message     string `json:"message,omitempty"`
}

func (s *Service) nginxUninstall(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.startJob == nil {
		return nil, v1.New(v1.CodeInternal, "长任务服务未装配，不能受理 server nginx uninstall")
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	if srv.Status.SameHostAsMaster {
		if err := command.VerifyDynamicHuman(ctx, s.getVerifier(), "server nginx uninstall",
			fmt.Sprintf("在主控同机节点 %s 上卸载 nginx", srv.Spec.Name), inv); err != nil {
			return nil, err
		}
	}
	return s.startJob(ctx, srv.Metadata.ID, inv, func(jobCtx context.Context) (any, error) {
		unlock := s.LockServerConfig(srv.Metadata.ID)
		defer unlock()
		var res xrpc.NginxUninstallResult
		if err := s.call(jobCtx, srv, xrpc.MethodNginxUninstall, xrpc.NginxUninstallParams{}, &res, nginxInstallTimeout); err != nil {
			return nil, err
		}
		return &NginxUninstallView{
			Server:      srv.Spec.Name,
			Uninstalled: res.Uninstalled,
			InDocker:    res.InDocker,
			Message:     res.Message,
		}, nil
	}, nil)
}

func (s *Service) nginxServers(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	page := command.Page{}
	if inv.Page != nil {
		page = *inv.Page
	}
	if err := page.Normalize(); err != nil {
		return nil, err
	}
	offset, err := command.DecodeOffsetCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	var res xrpc.NginxServersResult
	if err := s.call(ctx, srv, xrpc.MethodNginxServers, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	blocks := res.Servers
	total := len(blocks)
	if offset > total {
		offset = total
	}
	end := offset + page.Limit
	if end > total {
		end = total
	}
	out := &command.PageResult{Items: make([]any, 0, end-offset), Total: total}
	for _, b := range blocks[offset:end] {
		if b.ServerNames == nil {
			b.ServerNames = []string{}
		}
		if b.ListenPorts == nil {
			b.ListenPorts = []string{}
		}
		out.Items = append(out.Items, b)
	}
	if end < total {
		out.NextCursor = command.EncodeOffsetCursor(end)
	}
	return out, nil
}

// NginxClearStreamView 是 server nginx clear-stream 的输出。
type NginxClearStreamView struct {
	Server        string   `json:"server"`
	Port          int      `json:"port"`
	RemovedFiles  []string `json:"removed_files"`
	Reloaded      bool     `json:"reloaded"`
	RolledBack    bool     `json:"rolled_back,omitempty"`
	RollbackError string   `json:"rollback_error,omitempty"`
	Message       string   `json:"message,omitempty"`
}

func (s *Service) nginxClearStream(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	port := inv.Int("port", 0)
	if port < 1 || port > 65535 {
		return nil, v1.Newf(v1.CodeBadRequest, "--port 必须在 1..65535 之间，得到 %d", port)
	}
	if srv.Status.SameHostAsMaster {
		if err := command.VerifyDynamicHuman(ctx, s.getVerifier(), "server nginx clear-stream",
			fmt.Sprintf("清理主控同机节点 %s 的 nginx stream 配置（端口 %d）", srv.Spec.Name, port), inv); err != nil {
			return nil, err
		}
		if port == 443 {
			return nil, v1.New(v1.CodeConflict, "同机节点受主控 443 防抢占守卫保护，禁止清理 443 端口的 stream 配置")
		}
	}

	unlock := s.LockServerConfig(srv.Metadata.ID)
	defer unlock()

	var res xrpc.NginxClearStreamResult
	if err := s.call(ctx, srv, xrpc.MethodNginxClearStream, xrpc.NginxClearStreamParams{
		Port:             port,
		ProtectMaster443: srv.Status.SameHostAsMaster,
	}, &res, serviceControlTimeout); err != nil {
		return nil, err
	}
	if res.RemovedFiles == nil {
		res.RemovedFiles = []string{}
	}
	return &NginxClearStreamView{
		Server:        srv.Spec.Name,
		Port:          res.Port,
		RemovedFiles:  res.RemovedFiles,
		Reloaded:      res.Reloaded,
		RolledBack:    res.RolledBack,
		RollbackError: res.RollbackError,
		Message:       res.Message,
	}, nil
}
