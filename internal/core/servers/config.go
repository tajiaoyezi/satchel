package servers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// serverSpecColumns 是 servers 表全部 spec 档列名（与 schema 同步，用于完整 spec 更新）。
var serverSpecColumns = []string{
	"name", "ipv6_enabled", "domain", "domain_v6", "connection_mode",
	"pull_address", "pull_address_v6", "pull_port", "listen_port", "lock_entry_ip",
	"use_443", "steal_mode", "site_type", "site_value", "port_range_min", "port_range_max",
	"traffic_limit", "traffic_reset_day", "traffic_stats_mode", "traffic_source", "include_in_traffic_stats", "traffic_calibration",
	"region", "region_country", "region_name", "region_city",
	"renewal_price", "renewal_cycle", "renewal_currency", "provider_name", "provider_url", "telecom_paid_peer", "expires_at",
	"ddns_enabled", "ddns_provider_id", "ddns_record_name",
	"core_log_level", "core_dns", "core_stats_enabled", "sort_order",
}

// DefaultSpec 返回新建 Server 时的缺省 spec（匹配 schema 默认值与 DefaultTrue 语义）。
func DefaultSpec() v1.ServerSpec {
	return v1.ServerSpec{
		Ipv6Enabled:           true,
		ConnectionMode:        ModeAuto,
		StealMode:             "tunnel",
		TrafficStatsMode:      TrafficStatsBoth,
		TrafficSource:         TrafficSourceSystem,
		IncludeInTrafficStats: true,
		RenewalCycle:          "month",
		RenewalCurrency:       "CNY",
		CoreLogLevel:          "warn",
		CoreDNS:               json.RawMessage(`{}`),
	}
}

// NormalizeSpec 补齐新建或更新 ServerSpec 时的空枚举/空 JSON 默认值并校验枚举与端口范围。
func NormalizeSpec(spec v1.ServerSpec) (v1.ServerSpec, error) {
	spec.Name = strings.TrimSpace(spec.Name)
	if spec.Name == "" {
		return spec, v1.New(v1.CodeBadRequest, "Server.spec.name 不能为空")
	}
	if spec.ConnectionMode == "" {
		spec.ConnectionMode = ModeAuto
	}
	if !ValidMode(spec.ConnectionMode) {
		return spec, v1.Newf(v1.CodeBadRequest, "Server.spec.connection_mode %q 不合法（支持 auto/websocket/http/pull）", spec.ConnectionMode)
	}
	if spec.StealMode == "" {
		spec.StealMode = "tunnel"
	}
	if spec.StealMode != "tunnel" && spec.StealMode != "fallback" {
		return spec, v1.Newf(v1.CodeBadRequest, "Server.spec.steal_mode %q 不合法（支持 tunnel/fallback）", spec.StealMode)
	}
	if spec.TrafficStatsMode == "" {
		spec.TrafficStatsMode = TrafficStatsBoth
	}
	if !ValidTrafficStatsMode(spec.TrafficStatsMode) {
		return spec, v1.Newf(v1.CodeBadRequest, "Server.spec.traffic_stats_mode %q 不合法", spec.TrafficStatsMode)
	}
	if spec.TrafficSource == "" {
		spec.TrafficSource = TrafficSourceSystem
	}
	if !ValidTrafficSource(spec.TrafficSource) {
		return spec, v1.Newf(v1.CodeBadRequest, "Server.spec.traffic_source %q 不合法", spec.TrafficSource)
	}
	if spec.RenewalCycle == "" {
		spec.RenewalCycle = "month"
	}
	if spec.RenewalCurrency == "" {
		spec.RenewalCurrency = "CNY"
	}
	if spec.CoreLogLevel == "" {
		spec.CoreLogLevel = "warn"
	}
	if spec.ListenPort < 0 || spec.ListenPort > 65535 {
		return spec, v1.Newf(v1.CodeBadRequest, "Server.spec.listen_port %d 必须在 0..65535 之间", spec.ListenPort)
	}
	if spec.PullPort < 0 || spec.PullPort > 65535 {
		return spec, v1.Newf(v1.CodeBadRequest, "Server.spec.pull_port %d 必须在 0..65535 之间", spec.PullPort)
	}
	if len(bytes.TrimSpace(spec.CoreDNS)) == 0 || bytes.Equal(bytes.TrimSpace(spec.CoreDNS), []byte("null")) {
		spec.CoreDNS = json.RawMessage(`{}`)
	} else if !json.Valid(spec.CoreDNS) {
		return spec, v1.New(v1.CodeBadRequest, "Server.spec.core_dns 不是合法的 JSON")
	}
	return spec, nil
}

// ByIDIncludeDeleted 按 id 读取服务器，includeDeleted 为真时含已软删除行（供 restore 与 plan/apply 检查使用）。
func (r *Repo) ByIDIncludeDeleted(ctx context.Context, id int64) (*Server, error) {
	return r.byID(ctx, r.db, id, true)
}

// ByIDTx 在事务中按 id 读取服务器。
func (r *Repo) ByIDTx(ctx context.Context, tx bun.Tx, id int64, includeDeleted bool) (*Server, error) {
	return r.byID(ctx, tx, id, includeDeleted)
}

// ByNameIncludeDeleted 按自然键 name 读取服务器（含已软删除行，用于检测保留期内自然键占坑）。
func (r *Repo) ByNameIncludeDeleted(ctx context.Context, name string) (*Server, error) {
	m := new(model.Server)
	err := r.db.NewSelect().Model(m).Where("name = ?", name).Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取服务器失败", err)
	}
	return object(m), nil
}

func (r *Repo) byID(ctx context.Context, q bun.IDB, id int64, includeDeleted bool) (*Server, error) {
	m := new(model.Server)
	sel := q.NewSelect().Model(m).Where("id = ?", id)
	if !includeDeleted {
		sel = sel.Where("deleted_at IS NULL")
	}
	err := sel.Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取服务器失败", err)
	}
	return object(m), nil
}

func serverModelFromSpec(spec v1.ServerSpec) *model.Server {
	return &model.Server{
		Name:                  spec.Name,
		Ipv6Enabled:           spec.Ipv6Enabled,
		Domain:                spec.Domain,
		DomainV6:              spec.DomainV6,
		ConnectionMode:        spec.ConnectionMode,
		PullAddress:           spec.PullAddress,
		PullAddressV6:         spec.PullAddressV6,
		PullPort:              spec.PullPort,
		ListenPort:            spec.ListenPort,
		LockEntryIP:           spec.LockEntryIP,
		Use443:                spec.Use443,
		StealMode:             spec.StealMode,
		SiteType:              spec.SiteType,
		SiteValue:             spec.SiteValue,
		PortRangeMin:          spec.PortRangeMin,
		PortRangeMax:          spec.PortRangeMax,
		TrafficLimit:          spec.TrafficLimit,
		TrafficResetDay:       spec.TrafficResetDay,
		TrafficStatsMode:      spec.TrafficStatsMode,
		TrafficSource:         spec.TrafficSource,
		IncludeInTrafficStats: spec.IncludeInTrafficStats,
		TrafficCalibration:    spec.TrafficCalibration,
		Region:                spec.Region,
		RegionCountry:         spec.RegionCountry,
		RegionName:            spec.RegionName,
		RegionCity:            spec.RegionCity,
		RenewalPrice:          spec.RenewalPrice,
		RenewalCycle:          spec.RenewalCycle,
		RenewalCurrency:       spec.RenewalCurrency,
		ProviderName:          spec.ProviderName,
		ProviderURL:           spec.ProviderURL,
		TelecomPaidPeer:       spec.TelecomPaidPeer,
		ExpiresAt:             spec.ExpiresAt,
		DdnsEnabled:           spec.DdnsEnabled,
		DdnsProviderID:        spec.DdnsProviderID,
		DdnsRecordName:        spec.DdnsRecordName,
		CoreLogLevel:          spec.CoreLogLevel,
		CoreDNS:               spec.CoreDNS,
		CoreStatsEnabled:      spec.CoreStatsEnabled,
		SortOrder:             spec.SortOrder,
	}
}

// CreateFromSpecTx 在事务内按完整 ServerSpec 创建服务器，并同步生成三把互不相同的随机通信凭据。
func (r *Repo) CreateFromSpecTx(ctx context.Context, tx bun.Tx, spec v1.ServerSpec) (*Server, error) {
	norm, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	tokens, err := NewTokens(3)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "生成服务器凭据失败", err)
	}
	m := serverModelFromSpec(norm)
	m.Token = tokens[0]
	m.AgentToken = tokens[1]
	m.PullToken = tokens[2]
	if err := r.store.WithTx(tx).Insert(ctx, m); err != nil {
		return nil, err
	}
	return object(m), nil
}

// UpdateSpecTx 在事务内按完整 ServerSpec 更新未软删除服务器并递增 resourceVersion。
func (r *Repo) UpdateSpecTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool, spec v1.ServerSpec) (*Server, error) {
	norm, err := NormalizeSpec(spec)
	if err != nil {
		return nil, err
	}
	cur, err := r.byID(ctx, tx, id, true)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "Server 里没有 id 为 %d 的对象", id)
		}
		return nil, err
	}
	if cur.Metadata.DeletedAt != nil {
		return nil, v1.Newf(v1.CodeConflict, "Server %d 处于软删除状态，不能修改 spec（需先 restore）", id)
	}
	m := serverModelFromSpec(norm)
	m.ID = id
	m.ResourceVersion = expectedVersion
	if err := r.store.WithTx(tx).UpdateSpec(ctx, m, force, serverSpecColumns...); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

// SoftDeleteTx 在事务内软删除服务器（保留自然键占位并递增 resourceVersion）。
func (r *Repo) SoftDeleteTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*Server, error) {
	m := &model.Server{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).SoftDelete(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, true)
}

// RestoreTx 在事务内按原 id 恢复已软删除服务器并递增 resourceVersion。
func (r *Repo) RestoreTx(ctx context.Context, tx bun.Tx, id, expectedVersion int64, force bool) (*Server, error) {
	m := &model.Server{ID: id, ResourceVersion: expectedVersion}
	if err := r.store.WithTx(tx).Restore(ctx, m, force); err != nil {
		return nil, err
	}
	return r.byID(ctx, tx, id, false)
}

// MarkAppliedConfig 在节点明确回执 config.apply 成功后更新 applied_hash、applied_generation 并消费 first_apply_eligible。
func (r *Repo) MarkAppliedConfig(ctx context.Context, id int64, hash string, generation int64) error {
	m := &model.Server{
		ID:                 id,
		AppliedHash:        hash,
		AppliedGeneration:  generation,
		FirstApplyEligible: false,
	}
	return r.store.UpdateStatus(ctx, m, []string{"applied_hash", "applied_generation", "first_apply_eligible"})
}

// SetFirstApplyEligible 设置首次下发免基线资格标记（主要供导入工具与测试使用）。
func (r *Repo) SetFirstApplyEligible(ctx context.Context, id int64, eligible bool) error {
	m := &model.Server{ID: id, FirstApplyEligible: eligible}
	return r.store.UpdateStatus(ctx, m, []string{"first_apply_eligible"})
}
