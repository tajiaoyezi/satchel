// Package inbounds 是仓储层的入站配置（kind Inbound，config 类别）：
// 严守单向分层纪律，不依赖其它 internal/core/* 包，所有写操作均经 store 原语执行。
package inbounds

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// KindName 是 Inbound 的 kind 名。
const KindName = "Inbound"

// ErrNotFound 表示目标入站不存在。
var ErrNotFound = errors.New("inbound not found")

// Inbound 是入站资源对象（TLS 与 Settings 在序列化时默认打码）。
type Inbound = v1.Object[v1.InboundSpec, v1.InboundStatus]

var specColumns = []string{"server_id", "tag", "protocol", "port", "listen", "tls", "transport", "settings", "enabled", "sort_order"}

// Repo 是入站仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// NormalizeSpec 规范并校验 InboundSpec（不跨 server 校验由 UpdateTx 额外保证）。
func NormalizeSpec(spec v1.InboundSpec) (v1.InboundSpec, error) {
	spec.Tag = strings.TrimSpace(spec.Tag)
	spec.Protocol = strings.TrimSpace(spec.Protocol)
	spec.Listen = strings.TrimSpace(spec.Listen)
	if spec.ServerID <= 0 {
		return spec, v1.New(v1.CodeBadRequest, "Inbound.spec.server_id 必须为正整数")
	}
	if spec.Tag == "" {
		return spec, v1.New(v1.CodeBadRequest, "Inbound.spec.tag 不能为空")
	}
	if spec.Protocol == "" {
		return spec, v1.New(v1.CodeBadRequest, "Inbound.spec.protocol 不能为空")
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return spec, v1.Newf(v1.CodeBadRequest, "Inbound.spec.port %d 必须在 1..65535 之间", spec.Port)
	}
	tlsRaw, err := normalizeJSONObj(spec.TLS.Reveal(), "Inbound.spec.tls")
	if err != nil {
		return spec, err
	}
	spec.TLS = v1.SecretJSON(tlsRaw)
	trRaw, err := normalizeJSONObj(spec.Transport, "Inbound.spec.transport")
	if err != nil {
		return spec, err
	}
	spec.Transport = trRaw
	setRaw, err := normalizeJSONObj(spec.Settings.Reveal(), "Inbound.spec.settings")
	if err != nil {
		return spec, err
	}
	spec.Settings = v1.SecretJSON(setRaw)
	return spec, nil
}

func normalizeJSONObj(raw json.RawMessage, field string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	if !json.Valid(trimmed) {
		return nil, v1.Newf(v1.CodeBadRequest, "%s 不是合法的 JSON", field)
	}
	return trimmed, nil
}

// ByID 读取未软删除的入站。
func (r *Repo) ByID(ctx context.Context, id int64) (*Inbound, error) {
	return r.byID(ctx, r.db, id, false)
}

// ByIDIncludeDeleted 按 id 读取入站（含已软删除行）。
func (r *Repo) ByIDIncludeDeleted(ctx context.Context, id int64) (*Inbound, error) {
	return r.byID(ctx, r.db, id, true)
}

// ByIDTx 在事务中按 id 读取入站。
func (r *Repo) ByIDTx(ctx context.Context, tx bun.Tx, id int64, includeDeleted bool) (*Inbound, error) {
	return r.byID(ctx, tx, id, includeDeleted)
}

// ByServerTag 按 (server_id, tag) 读取未软删除入站。
func (r *Repo) ByServerTag(ctx context.Context, serverID int64, tag string) (*Inbound, error) {
	m := new(model.Inbound)
	err := r.db.NewSelect().Model(m).
		Where("server_id = ? AND tag = ?", serverID, tag).
		Where("deleted_at IS NULL").
		Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取入站失败", err)
	}
	return toObject(m), nil
}

// ListFilter 是分页列出入站的过滤条件。
type ListFilter struct {
	ServerID       int64
	Protocol       string
	IncludeDeleted bool
}

// List 按 server_id ASC, sort_order ASC, id ASC 分页列出入站。
func (r *Repo) List(ctx context.Context, f ListFilter, limit, offset int) ([]*Inbound, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []model.Inbound
	q := r.db.NewSelect().Model(&rows)
	if f.ServerID > 0 {
		q = q.Where("server_id = ?", f.ServerID)
	}
	if p := strings.ToLower(strings.TrimSpace(f.Protocol)); p != "" {
		q = q.Where("LOWER(protocol) = ?", p)
	}
	if !f.IncludeDeleted {
		q = q.Where("deleted_at IS NULL")
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "统计入站数量失败", err)
	}
	if err := q.OrderExpr("server_id ASC, sort_order ASC, id ASC").Limit(limit).Offset(offset).Scan(ctx); err != nil {
		return nil, 0, v1.Wrap(v1.CodeDatabase, "列出入站失败", err)
	}
	out := make([]*Inbound, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, total, nil
}

// ByServerTagIncludeDeleted 按 (server_id, tag) 读取入站（含软删除行，用于校验自然键占坑）。
func (r *Repo) ByServerTagIncludeDeleted(ctx context.Context, serverID int64, tag string) (*Inbound, error) {
	m := new(model.Inbound)
	err := r.db.NewSelect().Model(m).Where("server_id = ? AND tag = ?", serverID, tag).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取入站失败", err)
	}
	return toObject(m), nil
}

func (r *Repo) byID(ctx context.Context, q bun.IDB, id int64, includeDeleted bool) (*Inbound, error) {
	m := new(model.Inbound)
	sel := q.NewSelect().Model(m).Where("id = ?", id)
	if !includeDeleted {
		sel = sel.Where("deleted_at IS NULL")
	}
	err := sel.Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取入站失败", err)
	}
	return toObject(m), nil
}

// ListByServer 列出单台服务器上未软删除的全部入站，按 sort_order ASC, id ASC 稳定排序。
func (r *Repo) ListByServer(ctx context.Context, serverID int64) ([]*Inbound, error) {
	return r.listByServer(ctx, r.db, serverID)
}

// ListByServerTx 在事务内列出单台服务器未软删除的全部入站。
func (r *Repo) ListByServerTx(ctx context.Context, tx bun.Tx, serverID int64) ([]*Inbound, error) {
	return r.listByServer(ctx, tx, serverID)
}

func (r *Repo) listByServer(ctx context.Context, q bun.IDB, serverID int64) ([]*Inbound, error) {
	var rows []model.Inbound
	err := q.NewSelect().Model(&rows).
		Where("server_id = ?", serverID).
		Where("deleted_at IS NULL").
		OrderExpr("sort_order ASC, id ASC").
		Scan(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出入站失败", err)
	}
	out := make([]*Inbound, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, nil
}

// CreateTx 在事务中插入新入站。
func (r *Repo) CreateTx(ctx context.Context, tx bun.Tx, spec v1.InboundSpec) (*Inbound, error) {
	norm, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	m := modelFromSpec(norm)
	if err := r.store.WithTx(tx).Insert(ctx, m); err != nil {
		return nil, err
	}
	return toObject(m), nil
}

// UpdateTx 在事务中更新未软删除入站：禁止跨 server_id 迁移，遇到打码标记 "***" 保留原值。
func (r *Repo) UpdateTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool, spec v1.InboundSpec) (*Inbound, error) {
	cur, err := r.byID(ctx, tx, id, true)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "Inbound 里没有 id 为 %d 的对象", id)
		}
		return nil, err
	}
	if cur.Metadata.DeletedAt != nil {
		return nil, v1.Newf(v1.CodeConflict, "Inbound %d 处于软删除状态，不能修改 spec（需先 restore）", id)
	}
	if spec.ServerID != cur.Spec.ServerID {
		return nil, v1.Newf(v1.CodeBadRequest, "Inbound %d 不允许跨服务器修改 server_id（当前 %d，尝试改为 %d）", id, cur.Spec.ServerID, spec.ServerID)
	}
	if spec.TLS.IsRedacted() {
		spec.TLS = cur.Spec.TLS
	}
	if spec.Settings.IsRedacted() {
		spec.Settings = cur.Spec.Settings
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

// SoftDeleteTx 在事务中软删除入站。
func (r *Repo) SoftDeleteTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*Inbound, error) {
	m := &model.Inbound{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).SoftDelete(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, true)
}

// RestoreTx 在事务中按原 id 恢复入站。
func (r *Repo) RestoreTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*Inbound, error) {
	m := &model.Inbound{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).Restore(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

func modelFromSpec(spec v1.InboundSpec) *model.Inbound {
	return &model.Inbound{
		ServerID:  spec.ServerID,
		Tag:       spec.Tag,
		Protocol:  spec.Protocol,
		Port:      spec.Port,
		Listen:    spec.Listen,
		TLS:       spec.TLS.Reveal(),
		Transport: spec.Transport,
		Settings:  spec.Settings.Reveal(),
		Enabled:   spec.Enabled,
		SortOrder: spec.SortOrder,
	}
}

func toObject(m *model.Inbound) *Inbound {
	spec := v1.InboundSpec{
		ServerID:  m.ServerID,
		Tag:       m.Tag,
		Protocol:  m.Protocol,
		Port:      m.Port,
		Listen:    m.Listen,
		TLS:       v1.SecretJSON(m.TLS),
		Transport: m.Transport,
		Settings:  v1.SecretJSON(m.Settings),
		Enabled:   m.Enabled,
		SortOrder: m.SortOrder,
	}
	name := ""
	if info, ok := v1.Lookup(v1.Kind(KindName)); ok {
		name, _ = info.ObjectName(spec)
	}
	return &Inbound{
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
	}
}
