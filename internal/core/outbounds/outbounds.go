// Package outbounds 是仓储层的出站配置（kind Outbound，config 类别）：
// 不依赖其它 internal/core/* 包，所有写操作经 store 原语执行。
package outbounds

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

// KindName 是 Outbound 的 kind 名。
const KindName = "Outbound"

// ErrNotFound 表示目标出站不存在。
var ErrNotFound = errors.New("outbound not found")

// Outbound 是出站资源对象（Settings 序列化时默认打码）。
type Outbound = v1.Object[v1.OutboundSpec, v1.OutboundStatus]

var specColumns = []string{"server_id", "tag", "protocol", "settings", "is_warp", "balancer_group", "probe"}

// Repo 是出站仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// NormalizeSpec 规范并校验 OutboundSpec。
func NormalizeSpec(spec v1.OutboundSpec) (v1.OutboundSpec, error) {
	spec.Tag = strings.TrimSpace(spec.Tag)
	spec.Protocol = strings.TrimSpace(spec.Protocol)
	spec.BalancerGroup = strings.TrimSpace(spec.BalancerGroup)
	if spec.ServerID <= 0 {
		return spec, v1.New(v1.CodeBadRequest, "Outbound.spec.server_id 必须为正整数")
	}
	if spec.Tag == "" {
		return spec, v1.New(v1.CodeBadRequest, "Outbound.spec.tag 不能为空")
	}
	if spec.Protocol == "" {
		return spec, v1.New(v1.CodeBadRequest, "Outbound.spec.protocol 不能为空")
	}
	setRaw, err := normalizeJSONObj(spec.Settings.Reveal(), "Outbound.spec.settings")
	if err != nil {
		return spec, err
	}
	spec.Settings = v1.SecretJSON(setRaw)
	probeRaw, err := normalizeJSONObj(spec.Probe, "Outbound.spec.probe")
	if err != nil {
		return spec, err
	}
	spec.Probe = probeRaw
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

// ByID 读取未软删除出站。
func (r *Repo) ByID(ctx context.Context, id int64) (*Outbound, error) {
	return r.byID(ctx, r.db, id, false)
}

// ByIDIncludeDeleted 按 id 读取出站（含软删除行）。
func (r *Repo) ByIDIncludeDeleted(ctx context.Context, id int64) (*Outbound, error) {
	return r.byID(ctx, r.db, id, true)
}

// ByIDTx 在事务中按 id 读取出站。
func (r *Repo) ByIDTx(ctx context.Context, tx bun.Tx, id int64, includeDeleted bool) (*Outbound, error) {
	return r.byID(ctx, tx, id, includeDeleted)
}

// ByServerTagIncludeDeleted 按 (server_id, tag) 读取出站（含软删除行）。
func (r *Repo) ByServerTagIncludeDeleted(ctx context.Context, serverID int64, tag string) (*Outbound, error) {
	m := new(model.Outbound)
	err := r.db.NewSelect().Model(m).Where("server_id = ? AND tag = ?", serverID, tag).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取出站失败", err)
	}
	return toObject(m), nil
}

func (r *Repo) byID(ctx context.Context, q bun.IDB, id int64, includeDeleted bool) (*Outbound, error) {
	m := new(model.Outbound)
	sel := q.NewSelect().Model(m).Where("id = ?", id)
	if !includeDeleted {
		sel = sel.Where("deleted_at IS NULL")
	}
	err := sel.Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取出站失败", err)
	}
	return toObject(m), nil
}

// ListByServer 列出单台服务器未软删除的全部出站，按 id ASC 排序。
func (r *Repo) ListByServer(ctx context.Context, serverID int64) ([]*Outbound, error) {
	return r.listByServer(ctx, r.db, serverID)
}

// ListByServerTx 在事务内列出单台服务器未软删除的全部出站。
func (r *Repo) ListByServerTx(ctx context.Context, tx bun.Tx, serverID int64) ([]*Outbound, error) {
	return r.listByServer(ctx, tx, serverID)
}

func (r *Repo) listByServer(ctx context.Context, q bun.IDB, serverID int64) ([]*Outbound, error) {
	var rows []model.Outbound
	err := q.NewSelect().Model(&rows).
		Where("server_id = ?", serverID).
		Where("deleted_at IS NULL").
		OrderExpr("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出出站失败", err)
	}
	out := make([]*Outbound, len(rows))
	for i := range rows {
		out[i] = toObject(&rows[i])
	}
	return out, nil
}

// CreateTx 在事务内创建出站。
func (r *Repo) CreateTx(ctx context.Context, tx bun.Tx, spec v1.OutboundSpec) (*Outbound, error) {
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

// UpdateTx 在事务内更新未软删除出站：禁止跨 server_id 迁移，遇到 "***" 保留原 Settings。
func (r *Repo) UpdateTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool, spec v1.OutboundSpec) (*Outbound, error) {
	cur, err := r.byID(ctx, tx, id, true)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "Outbound 里没有 id 为 %d 的对象", id)
		}
		return nil, err
	}
	if cur.Metadata.DeletedAt != nil {
		return nil, v1.Newf(v1.CodeConflict, "Outbound %d 处于软删除状态，不能修改 spec（需先 restore）", id)
	}
	if spec.ServerID != cur.Spec.ServerID {
		return nil, v1.Newf(v1.CodeBadRequest, "Outbound %d 不允许跨服务器修改 server_id（当前 %d，尝试改为 %d）", id, cur.Spec.ServerID, spec.ServerID)
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

// SoftDeleteTx 在事务内软删除出站。
func (r *Repo) SoftDeleteTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*Outbound, error) {
	m := &model.Outbound{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).SoftDelete(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, true)
}

// RestoreTx 在事务内恢复出站。
func (r *Repo) RestoreTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*Outbound, error) {
	m := &model.Outbound{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).Restore(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

func modelFromSpec(spec v1.OutboundSpec) *model.Outbound {
	return &model.Outbound{
		ServerID:      spec.ServerID,
		Tag:           spec.Tag,
		Protocol:      spec.Protocol,
		Settings:      spec.Settings.Reveal(),
		IsWarp:        spec.IsWarp,
		BalancerGroup: spec.BalancerGroup,
		Probe:         spec.Probe,
	}
}

func toObject(m *model.Outbound) *Outbound {
	spec := v1.OutboundSpec{
		ServerID:      m.ServerID,
		Tag:           m.Tag,
		Protocol:      m.Protocol,
		Settings:      v1.SecretJSON(m.Settings),
		IsWarp:        m.IsWarp,
		BalancerGroup: m.BalancerGroup,
		Probe:         m.Probe,
	}
	name := ""
	if info, ok := v1.Lookup(v1.Kind(KindName)); ok {
		name, _ = info.ObjectName(spec)
	}
	return &Outbound{
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
		Status: v1.OutboundStatus{
			LastProbeOk:        m.LastProbeOk,
			LastProbeLatencyMs: m.LastProbeLatencyMs,
			LastProbedAt:       m.LastProbedAt,
		},
	}
}
