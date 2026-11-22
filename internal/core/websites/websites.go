// Package websites 是仓储层的站点配置（kind Website，config 类别）：
// 复用 0001_init 已有 websites 表，零数据库迁移；不依赖其它 internal/core/* 包，所有写操作经 store 原语执行。
package websites

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// KindName 是 Website 的 kind 名。
const KindName = "Website"

// ErrNotFound 表示目标站点不存在。
var ErrNotFound = errors.New("website not found")

// Website 是站点资源对象（权威在 Spec，节点扫盘结果与落盘路径进 Status）。
type Website = v1.Object[v1.WebsiteSpec, v1.WebsiteStatus]

var specColumns = []string{"server_id", "domain", "type", "target", "certificate_id", "use_443"}

// Repo 是站点仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// NormalizeSpec 规范并校验 WebsiteSpec（不含跨服务器 same_host 校验，同机 443 守卫由业务层与 nginx.ValidateWebsiteSpec 叠加）。
func NormalizeSpec(spec v1.WebsiteSpec) (v1.WebsiteSpec, error) {
	if spec.ServerID <= 0 {
		return spec, v1.New(v1.CodeBadRequest, "Website.spec.server_id 必须为正整数")
	}
	dom, err := nginx.ValidateDomain(spec.Domain)
	if err != nil {
		return spec, err
	}
	spec.Domain = dom
	spec.Type = strings.ToLower(strings.TrimSpace(spec.Type))
	spec.Target = strings.TrimSpace(spec.Target)
	if err := nginx.ValidateWebsiteSpec(spec, false); err != nil {
		return spec, err
	}
	return spec, nil
}

// ByID 读取未软删除站点。
func (r *Repo) ByID(ctx context.Context, id int64) (*Website, error) {
	return r.byID(ctx, r.db, id, false)
}

// ByIDIncludeDeleted 按 id 读取站点（含软删除行）。
func (r *Repo) ByIDIncludeDeleted(ctx context.Context, id int64) (*Website, error) {
	return r.byID(ctx, r.db, id, true)
}

// ByIDTx 在事务内按 id 读取站点。
func (r *Repo) ByIDTx(ctx context.Context, tx bun.Tx, id int64, includeDeleted bool) (*Website, error) {
	return r.byID(ctx, tx, id, includeDeleted)
}

// ByServerDomainIncludeDeleted 按 (server_id, domain) 读取站点（含软删除行，用于唯一键预检）。
func (r *Repo) ByServerDomainIncludeDeleted(ctx context.Context, serverID int64, domain string) (*Website, error) {
	dom := strings.ToLower(strings.TrimSpace(domain))
	m := new(model.Website)
	err := r.db.NewSelect().Model(m).Where("server_id = ? AND domain = ?", serverID, dom).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取站点失败", err)
	}
	return toObject(m), nil
}

// ListFilter 是分页列出站点的过滤条件。
type ListFilter struct {
	ServerID       int64
	Type           string
	IncludeDeleted bool
}

// List 按 server_id ASC, domain ASC, id ASC 分页列出站点。
func (r *Repo) List(ctx context.Context, f ListFilter, limit, offset int) ([]*Website, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []model.Website
	q := r.db.NewSelect().Model(&rows)
	if f.ServerID > 0 {
		q = q.Where("server_id = ?", f.ServerID)
	}
	if typ := strings.ToLower(strings.TrimSpace(f.Type)); typ != "" {
		q = q.Where("type = ?", typ)
	}
	if !f.IncludeDeleted {
		q = q.Where("deleted_at IS NULL")
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "统计站点数量失败", err)
	}
	if err := q.OrderExpr("server_id ASC, domain ASC, id ASC").Limit(limit).Offset(offset).Scan(ctx); err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "列出站点失败", err)
	}
	out := make([]*Website, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, total, nil
}

// ListByServer 列出单台服务器未软删除的全部站点，按 domain ASC, id ASC 排序。
func (r *Repo) ListByServer(ctx context.Context, serverID int64) ([]*Website, error) {
	return r.listByServer(ctx, r.db, serverID)
}

// ListByServerTx 在事务内列出单台服务器未软删除的全部站点。
func (r *Repo) ListByServerTx(ctx context.Context, tx bun.Tx, serverID int64) ([]*Website, error) {
	return r.listByServer(ctx, tx, serverID)
}

func (r *Repo) listByServer(ctx context.Context, q bun.IDB, serverID int64) ([]*Website, error) {
	var rows []model.Website
	err := q.NewSelect().Model(&rows).
		Where("server_id = ?", serverID).
		Where("deleted_at IS NULL").
		OrderExpr("domain ASC, id ASC").
		Scan(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出服务器站点失败", err)
	}
	out := make([]*Website, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, nil
}

// CreateTx 在事务内创建站点。
func (r *Repo) CreateTx(ctx context.Context, tx bun.Tx, spec v1.WebsiteSpec) (*Website, error) {
	norm, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	m := modelFromSpec(norm)
	m.Scanned = json.RawMessage(`{}`)
	if err := r.store.WithTx(tx).Insert(ctx, m); err != nil {
		return nil, err
	}
	return toObject(m), nil
}

// UpdateTx 在事务内更新未软删除站点（禁止跨服务器修改 server_id）。
func (r *Repo) UpdateTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool, spec v1.WebsiteSpec) (*Website, error) {
	cur, err := r.byID(ctx, tx, id, true)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "Website 里没有 id 为 %d 的对象", id)
		}
		return nil, err
	}
	if cur.Metadata.DeletedAt != nil {
		return nil, v1.Newf(v1.CodeConflict, "Website %d 处于软删除状态，不能修改 spec（需先 restore）", id)
	}
	if spec.ServerID != cur.Spec.ServerID {
		return nil, v1.Newf(v1.CodeBadRequest, "Website %d 不允许跨服务器修改 server_id（当前 %d，尝试改为 %d）", id, cur.Spec.ServerID, spec.ServerID)
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

// SoftDeleteTx 在事务内软删除站点。
func (r *Repo) SoftDeleteTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*Website, error) {
	m := &model.Website{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).SoftDelete(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, true)
}

// RestoreTx 在事务内恢复站点。
func (r *Repo) RestoreTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*Website, error) {
	m := &model.Website{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).Restore(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

// UpdateScanStatus 更新站点扫盘运行态三列（status 写，不抬升 resource_version）。
func (r *Repo) UpdateScanStatus(ctx context.Context, id int64, scanned json.RawMessage, managed bool, confPath string) error {
	trimmed := bytes.TrimSpace(scanned)
	if len(trimmed) == 0 || !json.Valid(trimmed) {
		trimmed = json.RawMessage(`{}`)
	}
	row := &model.Website{
		Scanned:  trimmed,
		Managed:  managed,
		ConfPath: strings.TrimSpace(confPath),
	}
	res, err := r.db.NewUpdate().Model(row).
		Column("scanned", "managed", "conf_path").
		Where("id = ? AND deleted_at IS NULL", id).
		Exec(ctx)
	if err != nil {
		return v1.Wrap(v1.CodeDatabase, "更新站点扫盘状态失败", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateManagedStatus 仅更新站点纳管状态与配置文件路径（apply 回执后调用，不抬升 resource_version）。
func (r *Repo) UpdateManagedStatus(ctx context.Context, id int64, managed bool, confPath string) error {
	res, err := r.db.NewUpdate().Model((*model.Website)(nil)).
		Set("managed = ?", managed).
		Set("conf_path = ?", strings.TrimSpace(confPath)).
		Where("id = ?", id).
		Exec(ctx)
	if err != nil {
		return v1.Wrap(v1.CodeDatabase, "更新站点纳管状态失败", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repo) byID(ctx context.Context, q bun.IDB, id int64, includeDeleted bool) (*Website, error) {
	m := new(model.Website)
	sel := q.NewSelect().Model(m).Where("id = ?", id)
	if !includeDeleted {
		sel = sel.Where("deleted_at IS NULL")
	}
	err := sel.Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取站点失败", err)
	}
	return toObject(m), nil
}

func modelFromSpec(spec v1.WebsiteSpec) *model.Website {
	return &model.Website{
		ServerID:      spec.ServerID,
		Domain:        spec.Domain,
		Type:          spec.Type,
		Target:        spec.Target,
		CertificateID: spec.CertificateID,
		Use443:        spec.Use443,
	}
}

func toObject(m *model.Website) *Website {
	scanned := m.Scanned
	if len(bytes.TrimSpace(scanned)) == 0 {
		scanned = json.RawMessage(`{}`)
	}
	spec := v1.WebsiteSpec{
		ServerID:      m.ServerID,
		Domain:        m.Domain,
		Type:          m.Type,
		Target:        m.Target,
		CertificateID: m.CertificateID,
		Use443:        m.Use443,
	}
	name := ""
	if info, ok := v1.Lookup(v1.Kind(KindName)); ok {
		name, _ = info.ObjectName(spec)
	}
	return &Website{
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
		Status: v1.WebsiteStatus{
			Scanned:  scanned,
			Managed:  m.Managed,
			ConfPath: m.ConfPath,
		},
	}
}
