// Package websites 是业务层的网站（kind Website）查询、扫盘、无状态向导与模板目录服务（node-nginx-websites）：
// 提供 website list / get / scan / build / templates 共 5 条管理员专属命令。
// 所有站点配置写操作一律经由 plan / apply 唯一写路径完成，严守分层纪律（不 import 其它 service/*）。
package websites

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/command"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	corewebsites "github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// WebsiteScanner 是调用节点 nginx.websites.scan 管理 RPC 的最小接口（由 service/servers 实现）。
type WebsiteScanner interface {
	CallNginxWebsitesScan(ctx context.Context, serverID int64) (*xrpc.NginxWebsitesScanResult, error)
}

// Deps 聚合网站服务所需仓储与节点扫描接口。
type Deps struct {
	Servers  *coreservers.Repo
	Websites *corewebsites.Repo
	Scanner  WebsiteScanner
	Now      func() time.Time
}

// Service 是网站查询、扫盘与无状态向导服务。
type Service struct {
	deps Deps
}

// New 建服务。
func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{deps: deps}
}

// Bindings 返回全部 5 条管理员专属命令绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"website list":      s.list,
		"website get":       s.get,
		"website scan":      s.scan,
		"website build":     s.build,
		"website templates": s.templates,
	}
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "网站管理命令只对管理员开放")
	}
	return nil
}

func (s *Service) now() time.Time {
	return s.deps.Now().UTC().Truncate(time.Microsecond)
}

// WebsiteView 是 website list / website get 的完整对象视图。
type WebsiteView struct {
	APIVersion string           `json:"apiVersion"`
	Kind       v1.Kind          `json:"kind"`
	Metadata   v1.Metadata      `json:"metadata"`
	Spec       v1.WebsiteSpec   `json:"spec"`
	Status     v1.WebsiteStatus `json:"status"`
	ServerName string           `json:"server_name,omitempty"`
}

func makeWebsiteView(w *corewebsites.Website, serverName string) WebsiteView {
	return WebsiteView{
		APIVersion: w.APIVersion,
		Kind:       w.Kind,
		Metadata:   w.Metadata,
		Spec:       w.Spec,
		Status:     w.Status,
		ServerName: serverName,
	}
}

func (s *Service) resolveServer(ctx context.Context, ref string) (*coreservers.Server, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return nil, v1.New(v1.CodeBadRequest, "缺少服务器名或 ID")
	}
	if id, err := strconv.ParseInt(trimmed, 10, 64); err == nil && id > 0 {
		srv, err := s.deps.Servers.ByID(ctx, id)
		if err == nil {
			return srv, nil
		}
		if !errors.Is(err, coreservers.ErrNotFound) {
			return nil, err
		}
	}
	srv, err := s.deps.Servers.ByName(ctx, trimmed)
	if err != nil {
		if errors.Is(err, coreservers.ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "没有叫 %q 的服务器", trimmed)
		}
		return nil, err
	}
	return srv, nil
}

func (s *Service) serverNameMap(ctx context.Context) map[int64]string {
	out := map[int64]string{}
	list, err := s.deps.Servers.ListAll(ctx)
	if err != nil {
		return out
	}
	for _, srv := range list {
		out[srv.Metadata.ID] = srv.Spec.Name
	}
	return out
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
	offset, err := command.DecodeOffsetCursor(page.Cursor)
	if err != nil {
		return nil, err
	}
	var srvID int64
	if ref := strings.TrimSpace(inv.String("server-name", "")); ref != "" {
		srv, err := s.resolveServer(ctx, ref)
		if err != nil {
			return nil, err
		}
		srvID = srv.Metadata.ID
	}
	typ := strings.ToLower(strings.TrimSpace(inv.String("type", "")))
	if typ != "" && typ != nginx.TypeStatic && typ != nginx.TypeProxy && typ != nginx.TypeDecoy {
		return nil, v1.Newf(v1.CodeBadRequest, "不支持的站点类型过滤 %q（支持 static、proxy 或 decoy）", typ)
	}
	rows, total, err := s.deps.Websites.List(ctx, corewebsites.ListFilter{
		ServerID:       srvID,
		Type:           typ,
		IncludeDeleted: inv.Bool("include-deleted"),
	}, page.Limit, offset)
	if err != nil {
		return nil, err
	}
	nameByID := s.serverNameMap(ctx)
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, makeWebsiteView(r, nameByID[r.Spec.ServerID]))
	}
	res := &command.PageResult{Items: items, Total: total}
	if offset+len(rows) < total {
		res.NextCursor = command.EncodeOffsetCursor(offset + len(rows))
	}
	return res, nil
}

func (s *Service) get(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(inv.Arg(0)), 10, 64)
	if err != nil || id <= 0 {
		return nil, v1.Newf(v1.CodeBadRequest, "站点 id 必须是正整数，得到 %q", inv.Arg(0))
	}
	w, err := s.deps.Websites.ByID(ctx, id)
	if err != nil {
		if errors.Is(err, corewebsites.ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "Website 里没有 id 为 %d 的对象", id)
		}
		return nil, err
	}
	srvName := ""
	if srv, err := s.deps.Servers.ByIDIncludeDeleted(ctx, w.Spec.ServerID); err == nil {
		srvName = srv.Spec.Name
	}
	return makeWebsiteView(w, srvName), nil
}

// ScannedManagedSite 是 website scan 中已匹配主控 Website 期望规格并更新 status 的站点摘要。
type ScannedManagedSite struct {
	WebsiteID   int64    `json:"website_id"`
	Domain      string   `json:"domain"`
	ConfPath    string   `json:"conf_path"`
	Managed     bool     `json:"managed"`
	Type        string   `json:"type"`
	Target      string   `json:"target,omitempty"`
	ListenPorts []string `json:"listen_ports"`
	HasSSL      bool     `json:"has_ssl"`
}

// WebsiteScanView 是 website scan <name> 的输出。
type WebsiteScanView struct {
	Server    string                `json:"server"`
	ServerID  int64                 `json:"server_id"`
	Total     int                   `json:"total"`
	Managed   []ScannedManagedSite  `json:"managed"`
	Unmanaged []xrpc.ScannedWebsite `json:"unmanaged"`
	Missing   []string              `json:"missing,omitempty"`
	ScannedAt time.Time             `json:"scanned_at"`
}

func (s *Service) scan(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	if s.deps.Scanner == nil {
		return nil, v1.New(v1.CodeUnavailable, "节点网站扫描通道未装配")
	}
	srv, err := s.resolveServer(ctx, inv.Arg(0))
	if err != nil {
		return nil, err
	}
	scanRes, err := s.deps.Scanner.CallNginxWebsitesScan(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}
	dbSites, err := s.deps.Websites.ListByServer(ctx, srv.Metadata.ID)
	if err != nil {
		return nil, err
	}
	byDomain := make(map[string]*corewebsites.Website, len(dbSites))
	for _, w := range dbSites {
		byDomain[strings.ToLower(strings.TrimSpace(w.Spec.Domain))] = w
	}

	seenDB := map[int64]bool{}
	managed := make([]ScannedManagedSite, 0)
	unmanaged := make([]xrpc.ScannedWebsite, 0)
	for _, site := range scanRes.Sites {
		if site.ListenPorts == nil {
			site.ListenPorts = []string{}
		}
		dom := strings.ToLower(strings.TrimSpace(site.Domain))
		if w, ok := byDomain[dom]; ok {
			seenDB[w.Metadata.ID] = true
			rawScanned, _ := json.Marshal(site)
			if err := s.deps.Websites.UpdateScanStatus(ctx, w.Metadata.ID, rawScanned, site.Managed, site.ConfPath); err != nil {
				return nil, err
			}
			managed = append(managed, ScannedManagedSite{
				WebsiteID:   w.Metadata.ID,
				Domain:      site.Domain,
				ConfPath:    site.ConfPath,
				Managed:     site.Managed,
				Type:        site.Type,
				Target:      site.Target,
				ListenPorts: site.ListenPorts,
				HasSSL:      site.HasSSL,
			})
		} else {
			// 未纳管站点绝不写入 websites.spec，原样返回提示。
			unmanaged = append(unmanaged, site)
		}
	}

	var missing []string
	for _, w := range dbSites {
		if !seenDB[w.Metadata.ID] {
			_ = s.deps.Websites.UpdateScanStatus(ctx, w.Metadata.ID, json.RawMessage(`{"found":false}`), false, "")
			missing = append(missing, w.Spec.Domain)
		}
	}
	sort.Strings(missing)

	return &WebsiteScanView{
		Server:    srv.Spec.Name,
		ServerID:  srv.Metadata.ID,
		Total:     len(scanRes.Sites),
		Managed:   managed,
		Unmanaged: unmanaged,
		Missing:   missing,
		ScannedAt: s.now(),
	}, nil
}

// WebsitePlanOp 是可直接喂给 satchel plan --spec 的新建 Website 操作对象。
type WebsitePlanOp struct {
	Op   string         `json:"op"`
	Kind string         `json:"kind"`
	Spec v1.WebsiteSpec `json:"spec"`
}

// WebsiteBuildView 是 website build 的无状态向导输出。
type WebsiteBuildView struct {
	Server           string         `json:"server"`
	ServerID         int64          `json:"server_id"`
	SameHostAsMaster bool           `json:"same_host_as_master"`
	RequiresHuman    bool           `json:"requires_human,omitempty"`
	Spec             v1.WebsiteSpec `json:"spec"`
	ConfPath         string         `json:"conf_path"`
	PreviewConf      string         `json:"preview_conf"`
	HTMLPath         string         `json:"html_path,omitempty"`
	PlanOp           WebsitePlanOp  `json:"plan_op"`
}

func (s *Service) build(ctx context.Context, inv *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	srv, err := s.resolveServer(ctx, inv.String("server-name", ""))
	if err != nil {
		return nil, err
	}
	domain := strings.TrimSpace(inv.String("domain", ""))
	if domain == "" {
		return nil, v1.New(v1.CodeBadRequest, "缺少必填参数 --domain")
	}
	typ := strings.ToLower(strings.TrimSpace(inv.String("type", "")))
	if typ == "" {
		return nil, v1.New(v1.CodeBadRequest, "缺少必填参数 --type（static、proxy 或 decoy）")
	}
	target, err := nginx.NormalizeBuildTarget(typ, inv.String("target", ""), inv.String("wss-path", ""))
	if err != nil {
		return nil, err
	}
	var certID *int64
	if rawCert := inv.Int("certificate-id", 0); rawCert > 0 {
		v := int64(rawCert)
		certID = &v
	} else if rawCert < 0 {
		return nil, v1.New(v1.CodeBadRequest, "--certificate-id 必须是正整数")
	}
	spec := v1.WebsiteSpec{
		ServerID:      srv.Metadata.ID,
		Domain:        domain,
		Type:          typ,
		Target:        target,
		CertificateID: certID,
		Use443:        inv.Bool("use-443"),
	}
	norm, err := corewebsites.NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	rendered, err := nginx.RenderWebsite(0, norm, srv.Status.SameHostAsMaster)
	if err != nil {
		return nil, err
	}
	return &WebsiteBuildView{
		Server:           srv.Spec.Name,
		ServerID:         srv.Metadata.ID,
		SameHostAsMaster: srv.Status.SameHostAsMaster,
		RequiresHuman:    srv.Status.SameHostAsMaster,
		Spec:             norm,
		ConfPath:         rendered.ConfPath,
		PreviewConf:      rendered.Conf,
		HTMLPath:         rendered.HTMLPath,
		PlanOp: WebsitePlanOp{
			Op:   "create",
			Kind: corewebsites.KindName,
			Spec: norm,
		},
	}, nil
}

// WebsiteTemplatesView 是 website templates 的输出。
type WebsiteTemplatesView struct {
	Templates []nginx.TemplateInfo `json:"templates"`
	Rules     []string             `json:"rules"`
}

func (s *Service) templates(ctx context.Context, _ *command.Invocation) (any, error) {
	if err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	return &WebsiteTemplatesView{
		Templates: nginx.Templates(),
		Rules: []string{
			"所有 Satchel 管理的站点配置固定写入 /etc/nginx/servers/satchel-site-<domain>.conf，且首行带 # satchel-managed: 标记",
			"未带 # satchel-managed: 标记的手工站点仅在 website scan 的 unmanaged 清单中展示，apply 绝不覆盖或删除",
			"主控同机节点（same_host_as_master=true）禁止直接启用 use_443 抢占主控 443，且对同机 Website 执行 apply 时必须当场人工验证",
			"域名、target 与 wss-path 禁止包含空白、分号、花括号、引号、$、.. 与控制字符",
		},
	}, nil
}
