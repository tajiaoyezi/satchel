// Package returnroutes 是仓储层的返程路由配置（kind ReturnRoute，config 类别）：
// 复用 0001_init 已有 return_routes 表，零数据库迁移；不依赖其它 internal/core/* 包，所有写操作经 store 原语执行。
package returnroutes

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// KindName 是 ReturnRoute 的 kind 名。
const KindName = "ReturnRoute"

// ErrNotFound 表示目标返程路由记录不存在。
var ErrNotFound = errors.New("return route not found")

// ReturnRoute 是返程路由资源对象（配置权威在 Spec，节点返程测路结果进 Status）。
type ReturnRoute = v1.Object[v1.ReturnRouteSpec, v1.ReturnRouteStatus]

var (
	specColumns   = []string{"server_id", "carrier", "enabled", "probe"}
	statusColumns = []string{"route_type", "region", "entry_ip", "entry_asn", "reason", "tested_at"}
	carrierRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{1,31}$`)
)

// ProbeConfig 是 ReturnRoute.spec.probe JSON 支持的已知探测参数结构。
type ProbeConfig struct {
	TargetIP  string `json:"target_ip,omitempty"`
	Port      int    `json:"port,omitempty"`
	MaxHops   int    `json:"max_hops,omitempty"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// Repo 是返程路由仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// NormalizeCarrier 规范并校验返程运营商代码：CT/CU/CM/EDU 统一转大写，其余允许 2..32 位字母/数字/下划线/短横线。
func NormalizeCarrier(raw string) (string, error) {
	c := strings.TrimSpace(raw)
	if c == "" {
		return "", v1.New(v1.CodeBadRequest, "ReturnRoute.spec.carrier 不能为空")
	}
	if !carrierRe.MatchString(c) {
		return "", v1.Newf(v1.CodeBadRequest, "ReturnRoute.spec.carrier %q 格式非法（仅允许 2..32 位字母、数字、下划线或短横线，且以字母或数字开头）", raw)
	}
	switch strings.ToUpper(c) {
	case "CT", "CU", "CM", "EDU":
		return strings.ToUpper(c), nil
	default:
		return c, nil
	}
}

// ParseProbe 解析并校验 ReturnRoute.spec.probe JSON 对象（空值视为 {}）。
func ParseProbe(raw json.RawMessage) (ProbeConfig, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ProbeConfig{}, json.RawMessage(`{}`), nil
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err != nil || obj == nil {
		return ProbeConfig{}, nil, v1.New(v1.CodeBadRequest, "ReturnRoute.spec.probe 必须是合法 JSON 对象")
	}
	var cfg ProbeConfig
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return ProbeConfig{}, nil, v1.Newf(v1.CodeBadRequest, "ReturnRoute.spec.probe 含未知或类型错误字段：%v", err)
	}
	cfg.TargetIP = strings.TrimSpace(cfg.TargetIP)
	if cfg.TargetIP != "" {
		if strings.ContainsAny(cfg.TargetIP, " \t\r\n/\\") {
			return ProbeConfig{}, nil, v1.Newf(v1.CodeBadRequest, "ReturnRoute.spec.probe.target_ip %q 含非法字符", cfg.TargetIP)
		}
		if ip := net.ParseIP(cfg.TargetIP); ip == nil && len(cfg.TargetIP) > 253 {
			return ProbeConfig{}, nil, v1.Newf(v1.CodeBadRequest, "ReturnRoute.spec.probe.target_ip %q 过长", cfg.TargetIP)
		}
	}
	if cfg.Port < 0 || cfg.Port > 65535 {
		return ProbeConfig{}, nil, v1.Newf(v1.CodeBadRequest, "ReturnRoute.spec.probe.port %d 必须在 1..65535 之间", cfg.Port)
	}
	if cfg.MaxHops < 0 || cfg.MaxHops > xrpc.ReturnRouteMaxHops {
		return ProbeConfig{}, nil, v1.Newf(v1.CodeBadRequest, "ReturnRoute.spec.probe.max_hops %d 必须在 1..%d 之间", cfg.MaxHops, xrpc.ReturnRouteMaxHops)
	}
	if cfg.TimeoutMS != 0 && (cfg.TimeoutMS < xrpc.ReturnRouteMinTimeoutMS || cfg.TimeoutMS > xrpc.ReturnRouteMaxTimeoutMS) {
		return ProbeConfig{}, nil, v1.Newf(v1.CodeBadRequest, "ReturnRoute.spec.probe.timeout_ms %d 必须在 %d..%d 之间", cfg.TimeoutMS, xrpc.ReturnRouteMinTimeoutMS, xrpc.ReturnRouteMaxTimeoutMS)
	}
	canon, err := json.Marshal(cfg)
	if err != nil {
		return ProbeConfig{}, nil, v1.Wrap(v1.CodeInternal, "序列化 ReturnRoute.spec.probe 失败", err)
	}
	return cfg, canon, nil
}

// NormalizeSpec 规范并校验 ReturnRouteSpec。
func NormalizeSpec(spec v1.ReturnRouteSpec) (v1.ReturnRouteSpec, error) {
	if spec.ServerID <= 0 {
		return spec, v1.New(v1.CodeBadRequest, "ReturnRoute.spec.server_id 必须为正整数")
	}
	carrier, err := NormalizeCarrier(spec.Carrier)
	if err != nil {
		return spec, err
	}
	spec.Carrier = carrier
	_, probeCanon, err := ParseProbe(spec.Probe)
	if err != nil {
		return spec, err
	}
	spec.Probe = probeCanon
	return spec, nil
}

// ByID 读取未软删除返程路由。
func (r *Repo) ByID(ctx context.Context, id int64) (*ReturnRoute, error) {
	return r.byID(ctx, r.db, id, false)
}

// ByIDIncludeDeleted 按 id 读取返程路由（含软删除行）。
func (r *Repo) ByIDIncludeDeleted(ctx context.Context, id int64) (*ReturnRoute, error) {
	return r.byID(ctx, r.db, id, true)
}

// ByIDTx 在事务内按 id 读取返程路由。
func (r *Repo) ByIDTx(ctx context.Context, tx bun.Tx, id int64, includeDeleted bool) (*ReturnRoute, error) {
	return r.byID(ctx, tx, id, includeDeleted)
}

// ByServerCarrierIncludeDeleted 按 (server_id, carrier) 读取返程路由（含软删除行，用于唯一键预检与按运营商查找）。
func (r *Repo) ByServerCarrierIncludeDeleted(ctx context.Context, serverID int64, carrier string) (*ReturnRoute, error) {
	c, err := NormalizeCarrier(carrier)
	if err != nil {
		return nil, ErrNotFound
	}
	m := new(model.ReturnRoute)
	err = r.db.NewSelect().Model(m).Where("server_id = ? AND carrier = ?", serverID, c).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取返程路由失败", err)
	}
	return toObject(m), nil
}

// ListFilter 是分页列出返程路由的过滤条件。
type ListFilter struct {
	ServerID       int64
	Carrier        string
	Enabled        *bool
	IncludeDeleted bool
}

// List 按 server_id ASC, carrier ASC, id ASC 分页列出返程路由。
func (r *Repo) List(ctx context.Context, f ListFilter, limit, offset int) ([]*ReturnRoute, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []model.ReturnRoute
	q := r.db.NewSelect().Model(&rows)
	if f.ServerID > 0 {
		q = q.Where("server_id = ?", f.ServerID)
	}
	if raw := strings.TrimSpace(f.Carrier); raw != "" {
		c := raw
		if norm, err := NormalizeCarrier(raw); err == nil {
			c = norm
		}
		q = q.Where("carrier = ?", c)
	}
	if f.Enabled != nil {
		q = q.Where("enabled = ?", *f.Enabled)
	}
	if !f.IncludeDeleted {
		q = q.Where("deleted_at IS NULL")
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "统计返程路由数量失败", err)
	}
	if err := q.OrderExpr("server_id ASC, carrier ASC, id ASC").Limit(limit).Offset(offset).Scan(ctx); err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "列出返程路由失败", err)
	}
	out := make([]*ReturnRoute, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, total, nil
}

// ListByServer 列出单台服务器未软删除的全部返程路由，按 carrier ASC, id ASC 排序。
func (r *Repo) ListByServer(ctx context.Context, serverID int64) ([]*ReturnRoute, error) {
	var rows []model.ReturnRoute
	err := r.db.NewSelect().Model(&rows).
		Where("server_id = ?", serverID).
		Where("deleted_at IS NULL").
		OrderExpr("carrier ASC, id ASC").
		Scan(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出服务器返程路由失败", err)
	}
	out := make([]*ReturnRoute, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, nil
}

// CreateTx 在事务内创建返程路由。
func (r *Repo) CreateTx(ctx context.Context, tx bun.Tx, spec v1.ReturnRouteSpec) (*ReturnRoute, error) {
	norm, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	m := modelFromSpec(norm)
	m.RouteType = "Unknown"
	if err := r.store.WithTx(tx).Insert(ctx, m); err != nil {
		return nil, err
	}
	return toObject(m), nil
}

// UpdateTx 在事务内更新未软删除返程路由（禁止跨服务器修改 server_id）。
func (r *Repo) UpdateTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool, spec v1.ReturnRouteSpec) (*ReturnRoute, error) {
	cur, err := r.byID(ctx, tx, id, true)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "ReturnRoute 里没有 id 为 %d 的对象", id)
		}
		return nil, err
	}
	if cur.Metadata.DeletedAt != nil {
		return nil, v1.Newf(v1.CodeConflict, "ReturnRoute %d 处于软删除状态，不能修改 spec（需先 restore）", id)
	}
	if spec.ServerID != cur.Spec.ServerID {
		return nil, v1.Newf(v1.CodeBadRequest, "ReturnRoute %d 不允许跨服务器修改 server_id（当前 %d，尝试改为 %d）", id, cur.Spec.ServerID, spec.ServerID)
	}
	norm, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	m := modelFromSpec(norm)
	m.ID = id
	m.ResourceVersion = expectedVersion
	if err := r.store.WithTx(tx).UpdateSpec(ctx, m, force, specColumns...); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

// SoftDeleteTx 在事务内软删除返程路由。
func (r *Repo) SoftDeleteTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*ReturnRoute, error) {
	m := &model.ReturnRoute{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).SoftDelete(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, true)
}

// RestoreTx 在事务内恢复返程路由。
func (r *Repo) RestoreTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*ReturnRoute, error) {
	m := &model.ReturnRoute{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).Restore(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

// ProbeStatusPatch 描述单次返程测路结果的 status 回写值。
type ProbeStatusPatch struct {
	RouteType string
	Region    string
	EntryIP   string
	EntryASN  string
	Reason    string
	TestedAt  time.Time
}

// UpdateProbeStatus 经 Store.UpdateStatus 回写返程测路 6 个 status 列，绝不抬升 resourceVersion。
func (r *Repo) UpdateProbeStatus(ctx context.Context, id int64, patch ProbeStatusPatch) error {
	rt := strings.TrimSpace(patch.RouteType)
	if rt == "" {
		rt = "Unknown"
	}
	ts := patch.TestedAt.UTC()
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	m := &model.ReturnRoute{
		ID:        id,
		RouteType: rt,
		Region:    strings.TrimSpace(patch.Region),
		EntryIP:   strings.TrimSpace(patch.EntryIP),
		EntryAsn:  strings.TrimSpace(patch.EntryASN),
		Reason:    strings.TrimSpace(patch.Reason),
		TestedAt:  &ts,
	}
	return r.store.UpdateStatus(ctx, m, statusColumns)
}

func (r *Repo) byID(ctx context.Context, q bun.IDB, id int64, includeDeleted bool) (*ReturnRoute, error) {
	m := new(model.ReturnRoute)
	sel := q.NewSelect().Model(m).Where("id = ?", id)
	if !includeDeleted {
		sel = sel.Where("deleted_at IS NULL")
	}
	err := sel.Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取返程路由失败", err)
	}
	return toObject(m), nil
}

func modelFromSpec(spec v1.ReturnRouteSpec) *model.ReturnRoute {
	probe := bytes.TrimSpace(spec.Probe)
	if len(probe) == 0 {
		probe = json.RawMessage(`{}`)
	}
	return &model.ReturnRoute{
		ServerID: spec.ServerID,
		Carrier:  spec.Carrier,
		Enabled:  spec.Enabled,
		Probe:    probe,
	}
}

func toObject(m *model.ReturnRoute) *ReturnRoute {
	probe := bytes.TrimSpace(m.Probe)
	if len(probe) == 0 {
		probe = json.RawMessage(`{}`)
	}
	spec := v1.ReturnRouteSpec{
		ServerID: m.ServerID,
		Carrier:  m.Carrier,
		Enabled:  m.Enabled,
		Probe:    probe,
	}
	name := ""
	if info, ok := v1.Lookup(v1.Kind(KindName)); ok {
		name, _ = info.ObjectName(spec)
	}
	return &ReturnRoute{
		APIVersion: v1.APIVersion,
		Kind:       v1.Kind(KindName),
		Metadata: v1.Metadata{
			ID:              m.ID,
			Name:            name,
			ResourceVersion: m.ResourceVersion,
			CreatedAt:       m.CreatedAt,
			UpdatedAt:       m.UpdatedAt,
			DeletedAt:       m.DeletedAt,
		},
		Spec: spec,
		Status: v1.ReturnRouteStatus{
			RouteType: m.RouteType,
			Region:    m.Region,
			EntryIP:   m.EntryIP,
			EntryAsn:  m.EntryAsn,
			Reason:    m.Reason,
			TestedAt:  m.TestedAt,
		},
	}
}
