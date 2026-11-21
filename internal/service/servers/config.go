package servers

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// SetConfigRenderer 注入单台服务器的规范 kernel.json 渲染回调（由装配根挂接 service/apply 的渲染方法，免除服务层互引）。
func (s *Service) SetConfigRenderer(fn func(ctx context.Context, serverID int64) (*singbox.Rendered, error)) {
	s.configMu.Lock()
	s.renderConfig = fn
	s.configMu.Unlock()
}

func (s *Service) getConfigRenderer() func(ctx context.Context, serverID int64) (*singbox.Rendered, error) {
	s.configMu.Lock()
	fn := s.renderConfig
	s.configMu.Unlock()
	return fn
}

// LockServerConfig 获取单台服务器的配置事务串行锁，返回解锁函数；与 restart-kernel 及 apply 共用。
func (s *Service) LockServerConfig(serverID int64) func() {
	s.configMu.Lock()
	if s.configLocks == nil {
		s.configLocks = map[int64]*sync.Mutex{}
	}
	mu := s.configLocks[serverID]
	if mu == nil {
		mu = &sync.Mutex{}
		s.configLocks[serverID] = mu
	}
	s.configMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// CallConfigApply 向目标节点下发 config.apply 原子配置事务（调用方负责按需持有 LockServerConfig）。
func (s *Service) CallConfigApply(ctx context.Context, serverID int64, params xrpc.ConfigApplyParams) (*xrpc.ConfigApplyResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	var res xrpc.ConfigApplyResult
	if err := s.call(ctx, srv, xrpc.MethodConfigApply, params, &res, restartTimeout); err != nil {
		return nil, err
	}
	if res.Inbounds == nil {
		res.Inbounds = []string{}
	}
	return &res, nil
}

// CallOutboundProbe 向目标节点下发 outbound.probe 实时出站探活请求。
func (s *Service) CallOutboundProbe(ctx context.Context, serverID int64, params xrpc.OutboundProbeParams) (*xrpc.OutboundProbeResult, error) {
	srv, err := s.repo.ByID(ctx, serverID)
	if err != nil {
		return nil, v1.Newf(v1.CodeNotFound, "没有叫 id=%d 的服务器", serverID)
	}
	timeout := time.Duration(params.TimeoutMS)*time.Millisecond + 5*time.Second
	if timeout < callTimeout {
		timeout = callTimeout
	}
	if timeout > 40*time.Second {
		timeout = 40 * time.Second
	}
	var res xrpc.OutboundProbeResult
	if err := s.call(ctx, srv, xrpc.MethodOutboundProbe, params, &res, timeout); err != nil {
		return nil, err
	}
	return &res, nil
}

// ConfigStatusView 是 server config status 的输出。
type ConfigStatusView struct {
	Server             string     `json:"server"`
	Exists             bool       `json:"exists"`
	Path               string     `json:"path,omitempty"`
	Hash               string     `json:"hash"`
	AppliedHash        string     `json:"applied_hash"`
	DesiredHash        string     `json:"desired_hash,omitempty"`
	AppliedGeneration  int64      `json:"applied_generation"`
	FirstApplyEligible bool       `json:"first_apply_eligible"`
	InSync             bool       `json:"in_sync"`
	Size               int64      `json:"size"`
	Modified           *time.Time `json:"modified,omitempty"`
	Inbounds           []string   `json:"inbounds"`
	KernelRunning      bool       `json:"kernel_running"`
	KernelVersion      string     `json:"kernel_version,omitempty"`
	Generation         uint64     `json:"generation"`
}

func (s *Service) configStatus(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	var res xrpc.ConfigStatusResult
	if err := s.call(ctx, srv, xrpc.MethodConfigStatus, nil, &res, callTimeout); err != nil {
		return nil, err
	}
	if res.Inbounds == nil {
		res.Inbounds = []string{}
	}
	desiredHash := ""
	if render := s.getConfigRenderer(); render != nil {
		if r, rerr := render(ctx, srv.Metadata.ID); rerr == nil && r != nil {
			desiredHash = r.Hash
		}
	}
	inSync := res.Hash != "" && srv.Status.AppliedHash != "" && res.Hash == srv.Status.AppliedHash
	if inSync && desiredHash != "" {
		inSync = res.Hash == desiredHash
	}
	return &ConfigStatusView{
		Server:             srv.Spec.Name,
		Exists:             res.Exists,
		Path:               res.Path,
		Hash:               res.Hash,
		AppliedHash:        srv.Status.AppliedHash,
		DesiredHash:        desiredHash,
		AppliedGeneration:  srv.Status.AppliedGeneration,
		FirstApplyEligible: srv.Status.FirstApplyEligible,
		InSync:             inSync,
		Size:               res.Size,
		Modified:           res.Modified,
		Inbounds:           res.Inbounds,
		KernelRunning:      res.KernelRunning,
		KernelVersion:      res.KernelVersion,
		Generation:         res.Generation,
	}, nil
}

func (s *Service) configFiles(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	scope := strings.TrimSpace(inv.String("scope", "all"))
	if scope == "" {
		scope = "all"
	}
	switch scope {
	case "all", "kernel", "nginx":
	default:
		return nil, v1.Newf(v1.CodeBadRequest, "不支持的 --scope %q（仅支持 all、kernel 或 nginx）", scope)
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
	var res xrpc.ConfigFilesResult
	if err := s.call(ctx, srv, xrpc.MethodConfigFiles, xrpc.ConfigFilesParams{Scope: scope}, &res, callTimeout); err != nil {
		return nil, err
	}
	files := res.Files
	total := len(files)
	if offset > total {
		offset = total
	}
	end := offset + page.Limit
	if end > total {
		end = total
	}
	out := &command.PageResult{Items: make([]any, 0, end-offset), Total: total}
	for _, f := range files[offset:end] {
		out.Items = append(out.Items, f)
	}
	if end < total {
		out.NextCursor = command.EncodeOffsetCursor(end)
	}
	return out, nil
}

// ConfigReadView 是 server config read 的输出。
type ConfigReadView struct {
	Server    string    `json:"server"`
	Scope     string    `json:"scope"`
	Path      string    `json:"path"`
	Content   string    `json:"content"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Truncated bool      `json:"truncated,omitempty"`
	Hash      string    `json:"hash,omitempty"`
	Redacted  bool      `json:"redacted"`
}

func (s *Service) configRead(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	path, err := required(inv, "path")
	if err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	var res xrpc.ConfigReadResult
	if err := s.call(ctx, srv, xrpc.MethodConfigRead, xrpc.ConfigReadParams{
		Path:     path,
		MaxBytes: xrpc.ConfigReadMaxBytes,
	}, &res, callTimeout); err != nil {
		return nil, err
	}
	content := res.Content
	redacted := false
	if !v1.IdentityFrom(ctx).HasScope(v1.ScopeSecrets) {
		content = singbox.RedactText(content)
		redacted = true
	}
	return &ConfigReadView{
		Server:    srv.Spec.Name,
		Scope:     res.Scope,
		Path:      res.Path,
		Content:   content,
		Size:      res.Size,
		Modified:  res.Modified,
		Truncated: res.Truncated,
		Hash:      res.Hash,
		Redacted:  redacted,
	}, nil
}

// ConfigTestView 是 server config test 的输出。
type ConfigTestView struct {
	Server   string   `json:"server"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Inbounds []string `json:"inbounds"`
	Hash     string   `json:"hash"`
}

func (s *Service) configTest(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.byName(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	render := s.getConfigRenderer()
	if render == nil {
		return nil, v1.New(v1.CodeInternal, "配置渲染器尚未装配")
	}
	rendered, err := render(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}
	var res xrpc.ConfigTestResult
	if err := s.call(ctx, srv, xrpc.MethodConfigTest, xrpc.ConfigTestParams{Config: string(rendered.Pretty)}, &res, callTimeout); err != nil {
		return nil, err
	}
	inbounds := res.Inbounds
	if inbounds == nil {
		inbounds = []string{}
	}
	hash := res.Hash
	if hash == "" {
		hash = rendered.Hash
	}
	return &ConfigTestView{
		Server:   srv.Spec.Name,
		OK:       res.OK,
		Error:    res.Error,
		Inbounds: inbounds,
		Hash:     hash,
	}, nil
}
