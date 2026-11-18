// Package servers 是仓储层的服务器（kind Server，配置类，m2-02 node-pairing / node-channel）：读写 servers 表，
// 对外给生成的资源对象（v1.Object[ServerSpec, ServerStatus]），模型不出本包。
// 建服务器经 store.Insert（plan/apply 之前唯一的建法）；在线状态是 status，经 store.UpdateStatus 写，不抬 resourceVersion。
// 已软删除的行一律当不存在（本 change 还没有删除，留着规则）。
package servers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/satchel/satchel/internal/base/model"
	"github.com/satchel/satchel/internal/base/store"
	v1 "github.com/satchel/satchel/pkg/api/v1"
)

// KindName 是 kind 名。
const KindName = "Server"

// 在线状态的三个值（servers.status 的枚举）。
const (
	StatusPending   = "pending"
	StatusConnected = "connected"
	StatusOffline   = "offline"
)

// 连接模式（servers.connection_mode 的枚举）与节点入站默认端口。
const (
	ModeAuto      = "auto"
	ModeWebsocket = "websocket"
	ModeHTTP      = "http"
	ModePull      = "pull"

	DefaultListenPort = 23889
	DefaultPullPort   = 23890

	// fallbackFailThreshold 是 auto 模式无心跳连续扫盘失联多少轮后转入轮询兜底的阈值。
	fallbackFailThreshold = 3
)

// Modes 是合法的服务器连接模式列表。
var Modes = []string{ModeAuto, ModeWebsocket, ModeHTTP, ModePull}

// ValidMode 报告 mode 是否为四种允许的连接模式之一。
func ValidMode(mode string) bool { return slices.Contains(Modes, mode) }

// Server 是一台服务器的资源对象。
type Server = v1.Object[v1.ServerSpec, v1.ServerStatus]

// tokenBytes 是 token 的随机字节数：32 字节，base64url 不带填充是 43 个字符（node-pairing「加服务器」）。
const tokenBytes = 32

// NewToken 生成一个服务器 token。
func NewToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewTokens 一次生成 n 个互不相同的服务器凭据。
func NewTokens(n int) ([]string, error) {
	out := make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	for len(out) < n {
		tok, err := NewToken()
		if err != nil {
			return nil, err
		}
		if _, dup := seen[tok]; dup {
			continue
		}
		seen[tok] = struct{}{}
		out = append(out, tok)
	}
	return out, nil
}

// ErrNotFound 表示没有这台服务器（含已软删除的行）。
var ErrNotFound = errors.New("server not found")

// Repo 是服务器仓储。
type Repo struct {
	db    *bun.DB
	store *store.Store
}

// New 建仓储。
func New(bdb *bun.DB, st *store.Store) *Repo {
	return &Repo{db: bdb, store: st}
}

// CreateParams 是创建服务器时的初始配置（Spec）。
type CreateParams struct {
	Name          string
	Mode          string
	Domain        string
	DomainV6      string
	ListenPort    int64
	PullAddress   string
	PullAddressV6 string
	PullPort      int64
}

// Create 建一台服务器：仅传名字与连接模式（省略时用 auto），其余参数取默认值。
func (r *Repo) Create(ctx context.Context, name, mode string) (*Server, error) {
	return r.CreateWith(ctx, CreateParams{Name: name, Mode: mode})
}

// CreateWith 按初始寻址参数建一台服务器：一次生成三把互不相同的 43 字符随机凭据（token、agent_token、pull_token），
// 「省略即为真」的两个布尔字段（ipv6_enabled、include_in_traffic_stats）置真，其余取库默认值。名字重复由 store 翻译成 name_taken。
func (r *Repo) CreateWith(ctx context.Context, p CreateParams) (*Server, error) {
	tokens, err := NewTokens(3)
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "生成服务器凭据失败", err)
	}
	mode := p.Mode
	if mode == "" {
		mode = ModeAuto
	}
	m := &model.Server{
		Name:                  p.Name,
		ConnectionMode:        mode,
		Domain:                p.Domain,
		DomainV6:              p.DomainV6,
		ListenPort:            p.ListenPort,
		PullAddress:           p.PullAddress,
		PullAddressV6:         p.PullAddressV6,
		PullPort:              p.PullPort,
		Token:                 tokens[0],
		AgentToken:            tokens[1],
		PullToken:             tokens[2],
		Ipv6Enabled:           true,
		IncludeInTrafficStats: true,
	}
	if err := r.store.Insert(ctx, m); err != nil {
		return nil, err
	}
	return object(m), nil
}

func (r *Repo) one(ctx context.Context, where string, arg any) (*model.Server, error) {
	m := new(model.Server)
	err := r.db.NewSelect().Model(m).Where(where, arg).Where("deleted_at IS NULL").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "读取服务器失败", err)
	}
	return m, nil
}

// ByName 按名字取一台服务器；没有时返回 ErrNotFound。
// ByID 按 id 取服务器。
func (r *Repo) ByID(ctx context.Context, id int64) (*Server, error) {
	m, err := r.one(ctx, "id = ?", id)
	if err != nil {
		return nil, err
	}
	return object(m), nil
}

func (r *Repo) ByName(ctx context.Context, name string) (*Server, error) {
	m, err := r.one(ctx, "name = ?", name)
	if err != nil {
		return nil, err
	}
	return object(m), nil
}

// ByToken 按 token 取一台服务器（节点认证）；token 为空或没有时返回 ErrNotFound。
func (r *Repo) ByToken(ctx context.Context, token string) (*Server, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	m, err := r.one(ctx, "token = ?", token)
	if err != nil {
		return nil, err
	}
	return object(m), nil
}

// Count 是服务器总数。
func (r *Repo) Count(ctx context.Context) (int, error) {
	n, err := r.db.NewSelect().Model((*model.Server)(nil)).Where("deleted_at IS NULL").Count(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "统计服务器失败", err)
	}
	return n, nil
}

// List 按 id 从小到大列出 id 大于 afterID 的服务器，至多 limit 台。
func (r *Repo) List(ctx context.Context, afterID int64, limit int) ([]*Server, error) {
	var rows []model.Server
	err := r.db.NewSelect().Model(&rows).Where("deleted_at IS NULL").Where("id > ?", afterID).OrderExpr("id ASC").Limit(limit).Scan(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出服务器失败", err)
	}
	out := make([]*Server, len(rows))
	for i := range rows {
		out[i] = object(&rows[i])
	}
	return out, nil
}

// MarkOnline 记一次上线或心跳：status 为 connected、最近心跳时间、来源 IP（IPv4 写 ip_address，IPv6 写 ip_address_v6，
// 空串不写）、节点进程的启动时间，清空 offline_since，并清零 auto 回退计数（push_fail_count=0、fallback_to_pull=false）。
func (r *Repo) MarkOnline(ctx context.Context, id int64, ip string, v6 bool, boot time.Time, at time.Time) error {
	m := &model.Server{
		ID:             id,
		Status:         StatusConnected,
		LastHeartbeat:  &at,
		OfflineSince:   nil,
		PushFailCount:  0,
		FallbackToPull: false,
	}
	cols := []string{"status", "last_heartbeat", "offline_since", "push_fail_count", "fallback_to_pull"}
	if !boot.IsZero() {
		b := boot.UTC()
		m.BootTime = &b
		cols = append(cols, "boot_time")
	}
	switch {
	case ip == "":
	case v6:
		m.IPAddressV6 = ip
		cols = append(cols, "ip_address_v6")
	default:
		m.IPAddress = &ip
		cols = append(cols, "ip_address")
	}
	return r.store.UpdateStatus(ctx, m, cols)
}

// ClearPendingFlags 清除凭据轮换或吊销的待确认标记（仅在节点使用库里当前有效 token 完成认证或心跳时调用）。
func (r *Repo) ClearPendingFlags(ctx context.Context, id int64) error {
	m := &model.Server{ID: id, RotationPending: false, RevokePending: false}
	return r.store.UpdateStatus(ctx, m, []string{"rotation_pending", "revoke_pending"})
}

// SetSameHost 更新服务器与主控是否同机（status 档字段，不抬 resourceVersion，不进审计）。
func (r *Repo) SetSameHost(ctx context.Context, id int64, sameHost bool) error {
	m := &model.Server{ID: id, SameHostAsMaster: sameHost}
	return r.store.UpdateStatus(ctx, m, []string{"same_host_as_master"})
}

// UpdatePorts 原子更新服务器的管理口与只读轮询口，按乐观并发抬升 resourceVersion。
func (r *Repo) UpdatePorts(ctx context.Context, id, resourceVersion, listenPort, pullPort int64) (*Server, error) {
	m := &model.Server{
		ID:              id,
		ResourceVersion: resourceVersion,
		ListenPort:      listenPort,
		PullPort:        pullPort,
	}
	if err := r.store.UpdateSpec(ctx, m, false, "listen_port", "pull_port"); err != nil {
		return nil, err
	}
	return r.ByID(ctx, id)
}

// MarkPullSuccess 记一次只读轮询口采集成功：status 维持或变为 connected、更新 last_heartbeat 与 last_pull_at、清空 offline_since，
// 有探测/命中地址时写回 ip_address 或 ip_address_v6；不清除 fallback_to_pull（等节点主动恢复 WS/HTTP 时清）。
func (r *Repo) MarkPullSuccess(ctx context.Context, id int64, ip string, v6 bool, at time.Time) error {
	m := &model.Server{
		ID:            id,
		Status:        StatusConnected,
		LastHeartbeat: &at,
		LastPullAt:    &at,
		OfflineSince:  nil,
	}
	cols := []string{"status", "last_heartbeat", "last_pull_at", "offline_since"}
	switch {
	case ip == "":
	case v6:
		m.IPAddressV6 = ip
		cols = append(cols, "ip_address_v6")
	default:
		m.IPAddress = &ip
		cols = append(cols, "ip_address")
	}
	return r.store.UpdateStatus(ctx, m, cols)
}

// TokensPatch 描述一次轮换或吊销涉及更新的凭据集合（非 nil 字段写入）。
type TokensPatch struct {
	Token      *string
	AgentToken *string
	PullToken  *string
}

func buildTokenActionModel(id int64, patch TokensPatch, at time.Time) (*model.Server, []string) {
	m := &model.Server{ID: id}
	cols := make([]string, 0, 5)
	if patch.Token != nil {
		m.Token = *patch.Token
		m.LastTokenRefresh = &at
		cols = append(cols, "token", "last_token_refresh")
	}
	if patch.AgentToken != nil {
		m.AgentToken = *patch.AgentToken
		m.LastAgentTokenRefresh = &at
		cols = append(cols, "agent_token", "last_agent_token_refresh")
	}
	if patch.PullToken != nil {
		m.PullToken = *patch.PullToken
		cols = append(cols, "pull_token")
	}
	return m, cols
}

// BeginRotation 在同一事务内写入新凭据并置 rotation_pending=true、last_rotated_at=at（不抬 resourceVersion）。
func (r *Repo) BeginRotation(ctx context.Context, id int64, patch TokensPatch, at time.Time) (*Server, error) {
	at = at.UTC().Truncate(time.Microsecond)
	actionModel, actionCols := buildTokenActionModel(id, patch, at)
	if len(actionCols) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "没有要更新的凭据")
	}
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		ts := r.store.WithTx(tx)
		if err := ts.UpdateAction(ctx, actionModel, actionCols); err != nil {
			return err
		}
		return ts.UpdateStatus(ctx, &model.Server{
			ID:              id,
			RotationPending: true,
			LastRotatedAt:   &at,
		}, []string{"rotation_pending", "last_rotated_at"})
	})
	if err != nil {
		return nil, err
	}
	return r.ByID(ctx, id)
}

// RollbackRotation 在通道不可达无法推送轮换时原子恢复旧凭据与轮换状态。
func (r *Repo) RollbackRotation(ctx context.Context, prev *Server) error {
	id := prev.Metadata.ID
	actionModel := &model.Server{
		ID:                    id,
		Token:                 prev.Status.Token.Reveal(),
		AgentToken:            prev.Status.AgentToken.Reveal(),
		PullToken:             prev.Status.PullToken.Reveal(),
		LastTokenRefresh:      prev.Status.LastTokenRefresh,
		LastAgentTokenRefresh: prev.Status.LastAgentTokenRefresh,
	}
	actionCols := []string{"token", "agent_token", "pull_token", "last_token_refresh", "last_agent_token_refresh"}
	statusModel := &model.Server{
		ID:              id,
		RotationPending: prev.Status.RotationPending,
		LastRotatedAt:   prev.Status.LastRotatedAt,
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		ts := r.store.WithTx(tx)
		if err := ts.UpdateAction(ctx, actionModel, actionCols); err != nil {
			return err
		}
		return ts.UpdateStatus(ctx, statusModel, []string{"rotation_pending", "last_rotated_at"})
	})
}

// FinishRotation 在节点确认 token.rotate 落盘后清除 rotation_pending。
func (r *Repo) FinishRotation(ctx context.Context, id int64) error {
	return r.store.UpdateStatus(ctx, &model.Server{ID: id, RotationPending: false}, []string{"rotation_pending"})
}

// RevokeTokens 覆盖凭据并清除 rotation_pending；markRevokePending 为真时置 revoke_pending=true。
func (r *Repo) RevokeTokens(ctx context.Context, id int64, patch TokensPatch, at time.Time, markRevokePending bool) (*Server, error) {
	at = at.UTC().Truncate(time.Microsecond)
	actionModel, actionCols := buildTokenActionModel(id, patch, at)
	if len(actionCols) == 0 {
		return nil, v1.New(v1.CodeBadRequest, "没有要吊销的凭据")
	}
	statusModel := &model.Server{ID: id, RotationPending: false}
	statusCols := []string{"rotation_pending"}
	if markRevokePending {
		statusModel.RevokePending = true
		statusCols = append(statusCols, "revoke_pending")
	}
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		ts := r.store.WithTx(tx)
		if err := ts.UpdateAction(ctx, actionModel, actionCols); err != nil {
			return err
		}
		return ts.UpdateStatus(ctx, statusModel, statusCols)
	})
	if err != nil {
		return nil, err
	}
	return r.ByID(ctx, id)
}

// MarkOffline 把一台在线的服务器标为离线并记 offline_since；已经不是 connected 的不动，返回 false。
func (r *Repo) MarkOffline(ctx context.Context, id int64, at time.Time) (bool, error) {
	m := &model.Server{ID: id, Status: StatusOffline, OfflineSince: &at}
	err := r.store.UpdateStatus(ctx, m, []string{"status", "offline_since"}, store.Cond{Column: "status", Value: StatusConnected})
	if e := v1.AsError(err); err != nil && (e.Code == v1.CodeConflict || e.Code == v1.CodeNotFound) {
		return false, nil
	}
	return err == nil, err
}

// MarkCore 记内核的状态（node-traffic「主控侧的累计与重启基线」）：只写 core_running、core_version、core_boot_count 与
// core_boot_time（bootTime 为空时不动它）。core_boot_count 给的是绝对值，不是「读出来加上」：写进去了却没收到应答时，
// 重试写的是同一个值，一次重启不会被记两次。只有流量拉取这一处写这几列。不抬 resourceVersion。
func (r *Repo) MarkCore(ctx context.Context, id int64, running bool, version string, bootTime *time.Time, bootCount int64) error {
	m := &model.Server{ID: id, CoreRunning: running, CoreVersion: version, CoreBootCount: bootCount}
	cols := []string{"core_running", "core_version", "core_boot_count"}
	if bootTime != nil {
		b := bootTime.UTC().Truncate(time.Microsecond)
		m.CoreBootTime = &b
		cols = append(cols, "core_boot_time")
	}
	return r.store.UpdateStatus(ctx, m, cols)
}

// SweepOffline 把 status 为 connected 而最近心跳早于 before 的服务器标为离线，返回标了几台。
// 逐台按「还是 connected 且心跳时间没变」的前置条件写：扫描与写入之间来了心跳的那台不会被误标。
func (r *Repo) SweepOffline(ctx context.Context, before, at time.Time) (int, error) {
	var stale []model.Server
	err := r.db.NewSelect().Model(&stale).Column("id", "last_heartbeat").
		Where("status = ?", StatusConnected).Where("deleted_at IS NULL").
		Where("last_heartbeat IS NULL OR last_heartbeat < ?", before).Scan(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "查找心跳超时的服务器失败", err)
	}
	n := 0
	for _, s := range stale {
		m := &model.Server{ID: s.ID, Status: StatusOffline, OfflineSince: &at}
		err := r.store.UpdateStatus(ctx, m, []string{"status", "offline_since"},
			store.Cond{Column: "status", Value: StatusConnected}, store.Cond{Column: "last_heartbeat", Value: s.LastHeartbeat})
		if e := v1.AsError(err); err != nil && (e.Code == v1.CodeConflict || e.Code == v1.CodeNotFound) {
			continue
		}
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// SweepAutoFallback 检查无活跃 WS、具有轮询可达地址且心跳早于 before（或从未心跳）的 auto 服务器，
// 每次扫盘累加 push_fail_count 并写 last_push_fail；连续达到 3 轮时置 fallback_to_pull=true 与 fallback_at=at，返回本轮转入轮询的台数。
func (r *Repo) SweepAutoFallback(ctx context.Context, before, at time.Time, activeWS map[int64]bool) (int, error) {
	var candidates []model.Server
	err := r.db.NewSelect().Model(&candidates).
		Column("id", "push_fail_count", "pull_address", "ip_address", "domain", "pull_address_v6", "ip_address_v6", "domain_v6").
		Where("deleted_at IS NULL").
		Where("connection_mode = ?", ModeAuto).
		Where("fallback_to_pull = ?", false).
		Where("last_heartbeat IS NULL OR last_heartbeat < ?", before).
		Scan(ctx)
	if err != nil {
		return 0, v1.Wrap(v1.CodeDatabase, "查找需评估轮询回退的服务器失败", err)
	}
	at = at.UTC().Truncate(time.Microsecond)
	fellBack := 0
	for _, s := range candidates {
		if activeWS[s.ID] || !hasPollEndpoint(s) {
			continue
		}
		nextCount := s.PushFailCount + 1
		m := &model.Server{ID: s.ID, PushFailCount: nextCount, LastPushFail: &at}
		cols := []string{"push_fail_count", "last_push_fail"}
		switched := false
		if nextCount >= fallbackFailThreshold {
			m.FallbackToPull = true
			m.FallbackAt = &at
			cols = append(cols, "fallback_to_pull", "fallback_at")
			switched = true
		}
		err := r.store.UpdateStatus(ctx, m, cols,
			store.Cond{Column: "push_fail_count", Value: s.PushFailCount},
			store.Cond{Column: "fallback_to_pull", Value: false})
		if e := v1.AsError(err); err != nil && (e.Code == v1.CodeConflict || e.Code == v1.CodeNotFound) {
			continue
		}
		if err != nil {
			return fellBack, err
		}
		if switched {
			fellBack++
		}
	}
	return fellBack, nil
}

func hasPollEndpoint(s model.Server) bool {
	if s.IPAddress != nil && strings.TrimSpace(*s.IPAddress) != "" {
		return true
	}
	for _, addr := range []string{s.PullAddress, s.Domain, s.PullAddressV6, s.IPAddressV6, s.DomainV6} {
		if strings.TrimSpace(addr) != "" {
			return true
		}
	}
	return false
}

// PollTargets 列出所有应当被只读轮询拉取流量的服务器：connection_mode='pull' 或处于 fallback_to_pull 状态的 auto 服务器。
func (r *Repo) PollTargets(ctx context.Context) ([]*Server, error) {
	var rows []model.Server
	err := r.db.NewSelect().Model(&rows).
		Where("deleted_at IS NULL").
		Where("(connection_mode = ? OR (connection_mode = ? AND fallback_to_pull = ?))", ModePull, ModeAuto, true).
		OrderExpr("id ASC").
		Scan(ctx)
	if err != nil {
		return nil, v1.Wrap(v1.CodeDatabase, "列出轮询目标服务器失败", err)
	}
	out := make([]*Server, len(rows))
	for i := range rows {
		out[i] = object(&rows[i])
	}
	return out, nil
}

// object 把模型套成资源对象：spec 与 status 的字段名与模型一一对应（同一份注册表生成），按名字逐个复制，
// 打码字段在对象里是 v1.Secret，序列化时只输出打码标记。
func object(m *model.Server) *Server {
	o := &Server{
		APIVersion: v1.APIVersion, Kind: v1.Kind(KindName),
		Metadata: v1.Metadata{ID: m.ID, Name: m.Name, ResourceVersion: m.ResourceVersion, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, DeletedAt: m.DeletedAt},
	}
	src := reflect.ValueOf(m).Elem()
	for _, dst := range []reflect.Value{reflect.ValueOf(&o.Spec).Elem(), reflect.ValueOf(&o.Status).Elem()} {
		for i := 0; i < dst.NumField(); i++ {
			f := src.FieldByName(dst.Type().Field(i).Name)
			if !f.IsValid() {
				panic("servers：模型里没有字段 " + dst.Type().Field(i).Name + "，生成物与注册表对不上")
			}
			dst.Field(i).Set(f.Convert(dst.Field(i).Type()))
		}
	}
	return o
}
