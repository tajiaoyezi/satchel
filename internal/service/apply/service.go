// Package apply 是业务层的配置类唯一写入编排服务（node-config-tx）：
// 提供 plan / apply / plans list / plans get / snapshot list / snapshot get / snapshot rollback 七条管理员命令，
// 并向 servers 服务提供规范 sing-box 配置渲染回调。严守分层纪律：不 import 其它 service/* 或 middleware/*。
package apply

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/nginx"
	"github.com/satchel/satchel/internal/base/singbox"
	"github.com/satchel/satchel/internal/command"
	corebatch "github.com/satchel/satchel/internal/core/batch"
	corebatchin "github.com/satchel/satchel/internal/core/batchinbounds"
	corebatchout "github.com/satchel/satchel/internal/core/batchoutbounds"
	coreinbounds "github.com/satchel/satchel/internal/core/inbounds"
	coreoutbounds "github.com/satchel/satchel/internal/core/outbounds"
	coreplans "github.com/satchel/satchel/internal/core/plans"
	corerouting "github.com/satchel/satchel/internal/core/routingrules"
	coreservers "github.com/satchel/satchel/internal/core/servers"
	coresettings "github.com/satchel/satchel/internal/core/settings"
	coresnapshots "github.com/satchel/satchel/internal/core/snapshots"
	corewebsites "github.com/satchel/satchel/internal/core/websites"
	v1 "github.com/satchel/satchel/pkg/api/v1"
	"github.com/satchel/satchel/pkg/xrpc"
)

// NodeClient 是向节点下发原子配置事务与获取单机配置锁的最小接口（由 service/servers 实现）。
type NodeClient interface {
	LockServerConfig(serverID int64) func()
	CallConfigApply(ctx context.Context, serverID int64, params xrpc.ConfigApplyParams) (*xrpc.ConfigApplyResult, error)
}

// WebsiteNodeClient 是向节点下发 nginx.websites.apply 原子站点同步的最小接口（由 service/servers 实现）。
type WebsiteNodeClient interface {
	CallNginxWebsitesApply(ctx context.Context, serverID int64, params xrpc.NginxWebsitesApplyParams) (*xrpc.NginxWebsitesApplyResult, error)
}

// Deps 聚合 apply 编排服务所需的仓储与回调。
type Deps struct {
	DB             *bun.DB
	Servers        *coreservers.Repo
	Inbounds       *coreinbounds.Repo
	Outbounds      *coreoutbounds.Repo
	RoutingRules   *corerouting.Repo
	Websites       *corewebsites.Repo
	Plans          *coreplans.Repo
	Snapshots      *coresnapshots.Repo
	Batch          *corebatch.Repo
	BatchInbounds  *corebatchin.Repo
	BatchOutbounds *corebatchout.Repo
	Settings       *coresettings.Repo
	Nodes          NodeClient
	WebsiteNodes   WebsiteNodeClient
	Verifier       command.HumanVerifier
	Now            func() time.Time
	PlanTTL        time.Duration
}

// Service 是配置类 plan / apply 与快照回滚服务。
type Service struct {
	deps Deps
}

// New 建服务。
func New(deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.PlanTTL <= 0 {
		deps.PlanTTL = coreplans.DefaultTTL
	}
	if deps.WebsiteNodes == nil {
		if w, ok := deps.Nodes.(WebsiteNodeClient); ok {
			deps.WebsiteNodes = w
		}
	}
	return &Service{deps: deps}
}

// Bindings 返回本服务负责的 7 条命令绑定。
func (s *Service) Bindings() command.Bindings {
	return command.Bindings{
		"plan":              s.planCmd,
		"apply":             s.applyCmd,
		"plans list":        s.plansList,
		"plans get":         s.plansGet,
		"snapshot list":     s.snapshotList,
		"snapshot get":      s.snapshotGet,
		"snapshot rollback": s.snapshotRollback,
	}
}

func (s *Service) now() time.Time {
	return s.deps.Now().UTC().Truncate(time.Microsecond)
}

func requireAdmin(ctx context.Context) error {
	if !v1.IdentityFrom(ctx).IsAdmin() {
		return v1.New(v1.CodeForbidden, "配置事务与快照命令只对管理员开放")
	}
	return nil
}

func (s *Service) masterURL(ctx context.Context) string {
	if s.deps.Settings == nil {
		return ""
	}
	st, err := s.deps.Settings.Load(ctx)
	if err != nil || st == nil {
		return ""
	}
	return st.Gates().MasterURL
}

func (s *Service) isSameHost443Protected(ctx context.Context, srv *coreservers.Server, spec v1.ServerSpec) bool {
	sameHost := false
	status := ""
	if srv != nil {
		sameHost = srv.Status.SameHostAsMaster
		status = srv.Status.Status
	}
	return nginx.IsSameHost443Protected(sameHost, status, spec.Domain, s.masterURL(ctx))
}

// RenderServerConfig 根据当前库内某台服务器的期望 spec 与启用资源渲染规范 sing-box 配置（供 server config status/test 复用）。
func (s *Service) RenderServerConfig(ctx context.Context, serverID int64) (*singbox.Rendered, error) {
	srv, err := s.deps.Servers.ByID(ctx, serverID)
	if err != nil {
		if errors.Is(err, coreservers.ErrNotFound) {
			return nil, v1.Newf(v1.CodeNotFound, "Server 里没有 id 为 %d 的对象", serverID)
		}
		return nil, err
	}
	ibs, err := s.deps.Inbounds.ListByServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	obs, err := s.deps.Outbounds.ListByServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	rrs, err := s.deps.RoutingRules.ListByServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	inSpecs := make([]v1.InboundSpec, len(ibs))
	for i, x := range ibs {
		inSpecs[i] = x.Spec
	}
	outSpecs := make([]v1.OutboundSpec, len(obs))
	for i, x := range obs {
		outSpecs[i] = x.Spec
	}
	rrSpecs := make([]v1.RoutingRuleSpec, len(rrs))
	for i, x := range rrs {
		rrSpecs[i] = x.Spec
	}
	return singbox.Render(singbox.ServerInput{
		Server:           srv.Spec,
		Inbounds:         inSpecs,
		Outbounds:        outSpecs,
		RoutingRules:     rrSpecs,
		ProtectMaster443: s.isSameHost443Protected(ctx, srv, srv.Spec),
	})
}

// Operation 是 plan 输入中的单条变更请求。
type Operation struct {
	Op              string          `json:"op"`
	Kind            string          `json:"kind"`
	ID              int64           `json:"id,omitempty"`
	ResourceVersion int64           `json:"resourceVersion,omitempty"`
	Spec            json.RawMessage `json:"spec,omitempty"`
}

// FieldChange 描述单字段更新前后的值。
type FieldChange struct {
	Before any `json:"before"`
	After  any `json:"after"`
}

// PlannedObject 是持久化在 Plan.objects 中的单条变更描述与预演基线。
type PlannedObject struct {
	Op              string                 `json:"op"`
	Kind            string                 `json:"kind"`
	ID              int64                  `json:"id,omitempty"`
	Name            string                 `json:"name"`
	ServerID        int64                  `json:"server_id,omitempty"`
	ExpectedVersion int64                  `json:"expected_version,omitempty"`
	Spec            json.RawMessage        `json:"spec,omitempty"`
	FieldDiff       map[string]FieldChange `json:"field_diff,omitempty"`
	FieldDiffHash   string                 `json:"field_diff_hash"`
	BeforeSpec      json.RawMessage        `json:"before_spec,omitempty"`
	AfterSpec       json.RawMessage        `json:"after_spec,omitempty"`
	Dangers         []v1.Danger            `json:"dangers,omitempty"`
}

// WebsiteConfigDiff 描述单台受影响服务器在预演中待同步或移除的受管站点配置预览。
type WebsiteConfigDiff struct {
	Domain   string `json:"domain"`
	Action   string `json:"action"` // "apply" | "remove"
	ConfPath string `json:"conf_path"`
	Conf     string `json:"conf,omitempty"`
}

// ServerConfigDiff 是单台受影响服务器在预演前后的 sing-box 配置摘要与站点配置预览。
type ServerConfigDiff struct {
	ServerID            int64               `json:"server_id"`
	ServerName          string              `json:"server_name"`
	ExpectedCurrentHash string              `json:"expected_current_hash"`
	CurrentRenderedHash string              `json:"current_rendered_hash"`
	NextHash            string              `json:"next_hash"`
	ConfigChanged       bool                `json:"config_changed"`
	FirstApplyEligible  bool                `json:"first_apply_eligible"`
	Inbounds            []string            `json:"inbounds"`
	BeforeConfig        string              `json:"before_config,omitempty"`
	AfterConfig         string              `json:"after_config,omitempty"`
	WebsiteChanged      bool                `json:"website_changed,omitempty"`
	Websites            []WebsiteConfigDiff `json:"websites,omitempty"`
	StealChanged        bool                `json:"steal_changed,omitempty"`
	StealConfig         *WebsiteConfigDiff  `json:"steal_config,omitempty"`
}

// BatchMeta 记录多机批量入站/出站规划附带的批次追踪元数据（存于 Plan.diff 内，零迁移）。
type BatchMeta struct {
	Kind    string `json:"kind"` // "Inbound" | "Outbound"
	BatchID string `json:"batch_id"`
}

// PlanDiffData 是持久化在 Plan.diff 中的结构化差异与危险提示。
type PlanDiffData struct {
	Objects         []PlannedObject    `json:"objects"`
	Servers         []ServerConfigDiff `json:"servers"`
	RequiredDangers []v1.Danger        `json:"required_dangers"`
	ConfirmHint     string             `json:"confirm_hint,omitempty"`
	RequiresHuman   bool               `json:"requires_human,omitempty"`
	BatchMeta       *BatchMeta         `json:"batch_meta,omitempty"`
}

// BatchWindowInfo 展示当前调用者的批量类滚动窗口使用情况。
type BatchWindowInfo struct {
	Used            int64 `json:"used"`
	Affected        int64 `json:"affected"`
	SingleThreshold int   `json:"single_threshold"`
	WindowThreshold int   `json:"window_threshold"`
	WindowMinutes   int   `json:"window_minutes"`
}

// SkippedObject 记录 snapshot rollback 跳过某个对象的原因（如已软删除或不存在）。
type SkippedObject struct {
	SnapshotID int64  `json:"snapshot_id"`
	Kind       string `json:"kind"`
	ID         int64  `json:"id"`
	Reason     string `json:"reason"`
}

// PlanView 是 plan / plans get / snapshot rollback 的统一响应结构。
type PlanView struct {
	PlanID          int64              `json:"plan_id"`
	BatchID         string             `json:"batch_id,omitempty"`
	Status          string             `json:"status"`
	AffectedCount   int64              `json:"affected_count"`
	ApplyID         *string            `json:"apply_id,omitempty"`
	ExpiresAt       time.Time          `json:"expires_at"`
	CreatedAt       time.Time          `json:"created_at"`
	RequiredDangers []v1.Danger        `json:"required_dangers"`
	ConfirmHint     string             `json:"confirm_hint,omitempty"`
	RequiresHuman   bool               `json:"requires_human,omitempty"`
	BatchWindow     *BatchWindowInfo   `json:"batch_window,omitempty"`
	Objects         []PlannedObject    `json:"objects"`
	Servers         []ServerConfigDiff `json:"servers"`
	Skipped         []SkippedObject    `json:"skipped,omitempty"`
}

// PlanSummary 是 plans list 的列表行。
type PlanSummary struct {
	ID            int64     `json:"id"`
	Status        string    `json:"status"`
	AffectedCount int64     `json:"affected_count"`
	ApplyID       *string   `json:"apply_id,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// ServerApplyResult 是单台节点在 apply 阶段的推送结果。
type ServerApplyResult struct {
	ServerID        int64    `json:"server_id"`
	ServerName      string   `json:"server_name"`
	Status          string   `json:"status"` // "applied" | "unchanged" | "failed"
	AppliedHash     string   `json:"applied_hash,omitempty"`
	Generation      uint64   `json:"generation,omitempty"`
	Inbounds        []string `json:"inbounds,omitempty"`
	WebsitesApplied []string `json:"websites_applied,omitempty"`
	WebsitesRemoved []string `json:"websites_removed,omitempty"`
	StealApplied    bool     `json:"steal_applied,omitempty"`
	StealRemoved    bool     `json:"steal_removed,omitempty"`
	StealConfPath   string   `json:"steal_conf_path,omitempty"`
	NginxReloaded   bool     `json:"nginx_reloaded,omitempty"`
	Code            string   `json:"code,omitempty"`
	Error           string   `json:"error,omitempty"`
	RolledBack      bool     `json:"rolled_back,omitempty"`
	RollbackError   string   `json:"rollback_error,omitempty"`
	CurrentHash     string   `json:"current_hash,omitempty"`
}

// AppliedObjectSummary 记录单条对象写库后的最新主键与版本。
type AppliedObjectSummary struct {
	Op              string `json:"op"`
	Kind            string `json:"kind"`
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	ResourceVersion int64  `json:"resourceVersion"`
}

// ApplyResult 是 apply 成功时的输出。
type ApplyResult struct {
	PlanID        int64                  `json:"plan_id"`
	ApplyID       string                 `json:"apply_id"`
	BatchID       string                 `json:"batch_id,omitempty"`
	AffectedCount int64                  `json:"affected_count"`
	Objects       []AppliedObjectSummary `json:"objects"`
	Servers       []ServerApplyResult    `json:"servers"`
}

func supportedKind(kind string) bool {
	switch kind {
	case coreservers.KindName, coreinbounds.KindName, coreoutbounds.KindName, corerouting.KindName, corewebsites.KindName:
		return true
	}
	return false
}

// parseOperations 解析 plan --spec 的单个对象或对象数组，严格拒绝顶层 status 与未知字段。
func parseOperations(raw string) ([]Operation, error) {
	trimmed := bytes.TrimSpace([]byte(raw))
	if len(trimmed) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "缺少 --spec（或通过 CLI --file 提供 JSON 变更清单）")
	}
	var rawItems []json.RawMessage
	switch trimmed[0] {
	case '{':
		rawItems = []json.RawMessage{trimmed}
	case '[':
		if err := json.Unmarshal(trimmed, &rawItems); err != nil {
			return nil, v1.Wrap(v1.CodeBadRequest, "--spec 不是合法的 JSON 数组", err)
		}
	default:
		return nil, v1.New(v1.CodeBadRequest, "--spec 必须是 JSON 对象或对象数组")
	}
	if len(rawItems) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "--spec 不能为空数组，至少包含一条变更操作")
	}

	allowedOpKeys := map[string]bool{"op": true, "kind": true, "id": true, "resourceVersion": true, "spec": true}
	ops := make([]Operation, 0, len(rawItems))
	seenTargets := map[string]bool{}
	for idx, item := range rawItems {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(item, &obj); err != nil || obj == nil {
			return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条变更不是合法的 JSON 对象", idx+1)
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "status" {
				return nil, v1.Newf(v1.CodeFieldNotApplyable, "第 %d 条变更包含 status 字段，status 为运行态只读字段，不能经 plan / apply 写入", idx+1)
			}
			if !allowedOpKeys[k] {
				return nil, v1.Newf(v1.CodeUnknownField, "第 %d 条变更包含未知顶层字段 %q（只允许 op、kind、id、resourceVersion、spec）", idx+1, k)
			}
		}
		var op Operation
		dec := json.NewDecoder(bytes.NewReader(item))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&op); err != nil {
			return nil, v1.Wrap(v1.CodeBadRequest, fmt.Sprintf("第 %d 条变更字段类型不合法", idx+1), err)
		}
		op.Op = strings.TrimSpace(op.Op)
		op.Kind = strings.TrimSpace(op.Kind)
		if !supportedKind(op.Kind) {
			return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条变更的 kind %q 不支持（当前支持 Server、Inbound、Outbound、RoutingRule、Website）", idx+1, op.Kind)
		}
		switch op.Op {
		case "create":
			if op.ID != 0 {
				return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条 create 变更不能指定 id", idx+1)
			}
			if len(bytes.TrimSpace(op.Spec)) == 0 {
				return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条 create 变更缺少 spec 对象", idx+1)
			}
		case "update":
			if op.ID <= 0 {
				return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条 update 变更必须提供正整数 id", idx+1)
			}
			if op.ResourceVersion <= 0 {
				return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条 update 变更必须提供正整数 resourceVersion", idx+1)
			}
			if len(bytes.TrimSpace(op.Spec)) == 0 {
				return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条 update 变更缺少 spec 对象", idx+1)
			}
		case "delete", "restore":
			if op.ID <= 0 {
				return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条 %s 变更必须提供正整数 id", idx+1, op.Op)
			}
			if op.ResourceVersion <= 0 {
				return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条 %s 变更必须提供正整数 resourceVersion", idx+1, op.Op)
			}
			if len(bytes.TrimSpace(op.Spec)) > 0 && !bytes.Equal(bytes.TrimSpace(op.Spec), []byte("null")) {
				return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条 %s 变更不需要传入 spec", idx+1, op.Op)
			}
		default:
			return nil, v1.Newf(v1.CodeBadRequest, "第 %d 条变更的 op %q 不支持（仅支持 create、update、delete、restore）", idx+1, op.Op)
		}
		if op.ID > 0 {
			key := fmt.Sprintf("%s#%d", op.Kind, op.ID)
			if seenTargets[key] {
				return nil, v1.Newf(v1.CodeBadRequest, "单次 plan 中不能重复操作同一目标对象 %s", key)
			}
			seenTargets[key] = true
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func newApplyID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "apply-" + hex.EncodeToString(b)
}
