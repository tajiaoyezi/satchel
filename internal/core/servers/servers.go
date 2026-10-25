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

// Create 建一台服务器：名字、连接模式与新生成的 token，「省略即为真」的两个布尔字段（ipv6_enabled、include_in_traffic_stats）置真，
// 其余取库默认值。名字重复由 store 翻译成 name_taken。
func (r *Repo) Create(ctx context.Context, name, mode string) (*Server, error) {
	token, err := NewToken()
	if err != nil {
		return nil, v1.Wrap(v1.CodeInternal, "生成服务器 token 失败", err)
	}
	m := &model.Server{Name: name, ConnectionMode: mode, Token: token, Ipv6Enabled: true, IncludeInTrafficStats: true}
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
// 空串不写）、节点进程的启动时间，清空 offline_since。
func (r *Repo) MarkOnline(ctx context.Context, id int64, ip string, v6 bool, boot time.Time, at time.Time) error {
	m := &model.Server{ID: id, Status: StatusConnected, LastHeartbeat: &at, OfflineSince: nil}
	cols := []string{"status", "last_heartbeat", "offline_since"}
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

// MarkOffline 把一台在线的服务器标为离线并记 offline_since；已经不是 connected 的不动，返回 false。
func (r *Repo) MarkOffline(ctx context.Context, id int64, at time.Time) (bool, error) {
	m := &model.Server{ID: id, Status: StatusOffline, OfflineSince: &at}
	err := r.store.UpdateStatus(ctx, m, []string{"status", "offline_since"}, store.Cond{Column: "status", Value: StatusConnected})
	if e := v1.AsError(err); err != nil && (e.Code == v1.CodeConflict || e.Code == v1.CodeNotFound) {
		return false, nil
	}
	return err == nil, err
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
